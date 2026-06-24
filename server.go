package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed api/openapi.yaml
var openAPISpec string

const (
	defaultServeAddr = "127.0.0.1:8080"
	maxJSONBodySize  = 1 << 20

	playbackStopped  = "stopped"
	playbackPlaying  = "playing"
	playbackPaused   = "paused"
	playbackFinished = "finished"
	playbackFailed   = "failed"
)

type voiceProvider func() ([]string, error)

type ServeConfig struct {
	Addr       string
	TTSTimeout time.Duration
}

type Book struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Path      string    `json:"path"`
	SaveFile  string    `json:"save_file"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

type BookStore struct {
	mu    sync.RWMutex
	next  int64
	books map[string]Book
}

type PlaybackSnapshot struct {
	State           string  `json:"state"`
	BookID          string  `json:"book_id,omitempty"`
	ProgressPercent float64 `json:"progress_percent"`
	CurrentByte     int64   `json:"current_byte"`
	Voice           string  `json:"voice,omitempty"`
	ChunkSize       int     `json:"chunk_size,omitempty"`
	Error           string  `json:"error,omitempty"`
}

type PlaybackEvent struct {
	Type     string           `json:"type"`
	Time     time.Time        `json:"time"`
	Playback PlaybackSnapshot `json:"playback"`
}

type EventBroker struct {
	mu      sync.Mutex
	clients map[chan PlaybackEvent]struct{}
}

type PlaybackManager struct {
	mu         sync.Mutex
	cond       *sync.Cond
	engines    engineFactory
	ttsTimeout time.Duration
	events     *EventBroker

	state       string
	book        Book
	currentByte int64
	voice       string
	chunkSize   int
	errMessage  string
	cancel      context.CancelFunc
}

type LocalAPI struct {
	store    *BookStore
	playback *PlaybackManager
	engines  engineFactory
}

type AddBookRequest struct {
	Path     string `json:"path"`
	Title    string `json:"title"`
	SaveFile string `json:"save_file"`
}

type StartPlaybackRequest struct {
	BookID    string `json:"book_id"`
	Voice     string `json:"voice"`
	ChunkSize int    `json:"chunk_size"`
}

type SetPositionRequest struct {
	BookID      string `json:"book_id"`
	CurrentByte int64  `json:"current_byte"`
}

func parseServeConfig(args []string, output io.Writer) (ServeConfig, error) {
	fs := flag.NewFlagSet("audiobook serve", flag.ContinueOnError)
	fs.SetOutput(output)

	cfg := ServeConfig{}
	fs.StringVar(&cfg.Addr, "addr", defaultServeAddr, "Адреса локального HTTP API")
	fs.DurationVar(&cfg.TTSTimeout, "tts-timeout", defaultTTSTimeout, "Максимальний час очікування одного TTS-фрагмента")

	if err := fs.Parse(args); err != nil {
		return ServeConfig{}, err
	}
	if cfg.Addr == "" {
		return ServeConfig{}, fmt.Errorf("значення -addr не може бути порожнім")
	}
	if cfg.TTSTimeout <= 0 {
		return ServeConfig{}, fmt.Errorf("значення -tts-timeout має бути більшим за 0")
	}
	return cfg, nil
}

func runServe(args []string, stdout, stderr io.Writer, makeSpeaker speakerFactory, voices voiceProvider, enableSignals bool) int {
	cfg, err := parseServeConfig(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "Помилка: %v\n", err)
		return 2
	}

	events := NewEventBroker()
	engines := newFunctionEngineFactory(makeSpeaker, voices)
	api := NewLocalAPI(NewBookStore(), NewPlaybackManager(engines, cfg.TTSTimeout, events), engines)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	fmt.Fprintf(stdout, "Local TTS API listening on http://%s\n", cfg.Addr)
	if !enableSignals {
		return waitHTTPServer(stderr, errCh)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		api.playback.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "Помилка: не вдалося коректно зупинити HTTP API: %v\n", err)
			return 1
		}
		return 0
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintf(stderr, "Помилка: HTTP API завершився з помилкою: %v\n", err)
		return 1
	}
}

func waitHTTPServer(stderr io.Writer, errCh <-chan error) int {
	err := <-errCh
	if errors.Is(err, http.ErrServerClosed) {
		return 0
	}
	fmt.Fprintf(stderr, "Помилка: HTTP API завершився з помилкою: %v\n", err)
	return 1
}

func NewBookStore() *BookStore {
	return &BookStore{books: make(map[string]Book)}
}

func (s *BookStore) Add(req AddBookRequest) (Book, error) {
	if strings.TrimSpace(req.Path) == "" {
		return Book{}, fmt.Errorf("path is required")
	}

	absPath, err := filepath.Abs(req.Path)
	if err != nil {
		return Book{}, fmt.Errorf("invalid book path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return Book{}, fmt.Errorf("book is not readable: %w", err)
	}
	if info.IsDir() {
		return Book{}, fmt.Errorf("book path must point to a file")
	}

	saveFile := strings.TrimSpace(req.SaveFile)
	if saveFile == "" {
		saveFile = defaultProgressPath(absPath)
	} else {
		saveFile, err = filepath.Abs(saveFile)
		if err != nil {
			return Book{}, fmt.Errorf("invalid save_file path: %w", err)
		}
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	book := Book{
		ID:        fmt.Sprintf("book-%d", s.next),
		Title:     title,
		Path:      absPath,
		SaveFile:  saveFile,
		Size:      info.Size(),
		CreatedAt: time.Now().UTC(),
	}
	s.books[book.ID] = book
	return book, nil
}

func (s *BookStore) List() []Book {
	s.mu.RLock()
	defer s.mu.RUnlock()

	books := make([]Book, 0, len(s.books))
	for _, book := range s.books {
		books = append(books, book)
	}
	return books
}

func (s *BookStore) Get(id string) (Book, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	book, ok := s.books[id]
	return book, ok
}

func defaultProgressPath(bookPath string) string {
	ext := filepath.Ext(bookPath)
	if ext == "" {
		return bookPath + ".progress.json"
	}
	return strings.TrimSuffix(bookPath, ext) + ".progress.json"
}

func NewEventBroker() *EventBroker {
	return &EventBroker{clients: make(map[chan PlaybackEvent]struct{})}
}

func (b *EventBroker) Subscribe() (<-chan PlaybackEvent, func()) {
	ch := make(chan PlaybackEvent, 32)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		delete(b.clients, ch)
		close(ch)
		b.mu.Unlock()
	}
}

func (b *EventBroker) Publish(event PlaybackEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

func NewPlaybackManager(engines engineFactory, ttsTimeout time.Duration, events *EventBroker) *PlaybackManager {
	m := &PlaybackManager{
		engines:    engines,
		ttsTimeout: ttsTimeout,
		events:     events,
		state:      playbackStopped,
		chunkSize:  defaultChunkSize,
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *PlaybackManager) Start(book Book, req StartPlaybackRequest) (PlaybackSnapshot, error) {
	chunkSize := req.ChunkSize
	if chunkSize == 0 {
		chunkSize = defaultChunkSize
	}
	if chunkSize < 0 {
		return PlaybackSnapshot{}, fmt.Errorf("chunk_size must be greater than 0")
	}

	startPos, err := loadBookProgress(book)
	if err != nil {
		return PlaybackSnapshot{}, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.state == playbackPlaying || m.state == playbackPaused {
		m.mu.Unlock()
		cancel()
		return PlaybackSnapshot{}, fmt.Errorf("playback is already active")
	}
	m.state = playbackPlaying
	m.book = book
	m.currentByte = startPos
	m.voice = req.Voice
	m.chunkSize = chunkSize
	m.errMessage = ""
	m.cancel = cancel
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.started", snapshot)
	go m.play(ctx, book, startPos, req.Voice, chunkSize)
	return snapshot, nil
}

func (m *PlaybackManager) Pause() (PlaybackSnapshot, error) {
	m.mu.Lock()
	if m.state != playbackPlaying {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, fmt.Errorf("playback is not playing")
	}
	m.state = playbackPaused
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.paused", snapshot)
	return snapshot, nil
}

func (m *PlaybackManager) Resume() (PlaybackSnapshot, error) {
	m.mu.Lock()
	if m.state != playbackPaused {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, fmt.Errorf("playback is not paused")
	}
	m.state = playbackPlaying
	m.cond.Broadcast()
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.resumed", snapshot)
	return snapshot, nil
}

func (m *PlaybackManager) Stop() PlaybackSnapshot {
	var book Book
	var pos int64
	var shouldSave bool

	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.book.ID != "" {
		book = m.book
		pos = m.currentByte
		shouldSave = true
	}
	m.state = playbackStopped
	m.errMessage = ""
	m.cond.Broadcast()
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	if shouldSave {
		_ = saveBookProgress(book, pos)
	}
	m.publish("playback.stopped", snapshot)
	return snapshot
}

func (m *PlaybackManager) SetPosition(book Book, pos int64) (PlaybackSnapshot, error) {
	m.mu.Lock()
	if m.state == playbackPlaying || m.state == playbackPaused {
		m.mu.Unlock()
		return PlaybackSnapshot{}, fmt.Errorf("cannot change position while playback is active")
	}
	m.mu.Unlock()

	if pos < 0 || pos > book.Size {
		return PlaybackSnapshot{}, fmt.Errorf("position is outside the book")
	}
	ok, err := isFileUTF8Boundary(book.Path, pos, book.Size)
	if err != nil {
		return PlaybackSnapshot{}, err
	}
	if !ok {
		return PlaybackSnapshot{}, fmt.Errorf("position is inside a UTF-8 character")
	}
	if err := saveBookProgress(book, pos); err != nil {
		return PlaybackSnapshot{}, err
	}

	m.mu.Lock()
	m.book = book
	m.currentByte = pos
	m.state = playbackStopped
	m.errMessage = ""
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("position.updated", snapshot)
	return snapshot, nil
}

func (m *PlaybackManager) Snapshot() PlaybackSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *PlaybackManager) play(ctx context.Context, book Book, startPos int64, voice string, chunkSize int) {
	file, err := os.Open(book.Path)
	if err != nil {
		m.fail(book, startPos, err)
		return
	}
	defer file.Close()

	if _, err := file.Seek(startPos, io.SeekStart); err != nil {
		m.fail(book, startPos, err)
		return
	}

	reader, err := NewStreamingChunkReader(file, startPos, chunkSize)
	if err != nil {
		m.fail(book, startPos, err)
		return
	}
	engine := m.engines(Config{
		BookFile:   book.Path,
		SaveFile:   book.SaveFile,
		Voice:      voice,
		ChunkSize:  chunkSize,
		TTSTimeout: m.ttsTimeout,
	})

	for {
		if !m.waitUntilPlayable(ctx) {
			return
		}

		chunk, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				m.finish(book)
				return
			}
			m.fail(book, m.current(), err)
			return
		}

		m.updateProgress("chunk.started", chunk.StartByte)
		if err := engine.Speak(ctx, chunk.Text); err != nil {
			_ = saveBookProgress(book, chunk.StartByte)
			m.fail(book, chunk.StartByte, err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err := saveBookProgress(book, chunk.EndByte); err != nil {
			m.fail(book, chunk.StartByte, err)
			return
		}
		m.updateProgress("progress.updated", chunk.EndByte)
	}
}

func (m *PlaybackManager) waitUntilPlayable(ctx context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.state == playbackPaused && ctx.Err() == nil {
		m.cond.Wait()
	}
	return ctx.Err() == nil && m.state == playbackPlaying
}

func (m *PlaybackManager) current() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentByte
}

func (m *PlaybackManager) updateProgress(eventType string, pos int64) {
	m.mu.Lock()
	if m.state != playbackPlaying && m.state != playbackPaused {
		m.mu.Unlock()
		return
	}
	m.currentByte = pos
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish(eventType, snapshot)
}

func (m *PlaybackManager) finish(book Book) {
	_ = saveBookProgress(book, 0)

	m.mu.Lock()
	m.state = playbackFinished
	m.currentByte = book.Size
	m.errMessage = ""
	m.cancel = nil
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.finished", snapshot)
}

func (m *PlaybackManager) fail(book Book, pos int64, err error) {
	_ = saveBookProgress(book, pos)

	m.mu.Lock()
	if m.state == playbackStopped {
		m.mu.Unlock()
		return
	}
	m.state = playbackFailed
	m.currentByte = pos
	m.errMessage = err.Error()
	m.cancel = nil
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.failed", snapshot)
}

func (m *PlaybackManager) snapshotLocked() PlaybackSnapshot {
	total := m.book.Size
	return PlaybackSnapshot{
		State:           m.state,
		BookID:          m.book.ID,
		ProgressPercent: progressPercent(m.currentByte, total),
		CurrentByte:     m.currentByte,
		Voice:           m.voice,
		ChunkSize:       m.chunkSize,
		Error:           m.errMessage,
	}
}

func (m *PlaybackManager) publish(eventType string, snapshot PlaybackSnapshot) {
	m.events.Publish(PlaybackEvent{
		Type:     eventType,
		Time:     time.Now().UTC(),
		Playback: snapshot,
	})
}

func loadBookProgress(book Book) (int64, error) {
	data, err := os.ReadFile(book.SaveFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	var progress Progress
	if err := json.Unmarshal(data, &progress); err != nil {
		return 0, fmt.Errorf("invalid progress JSON: %w", err)
	}
	if progress.Unit != PositionUnit {
		return 0, fmt.Errorf("incompatible progress unit %q", progress.Unit)
	}
	if progress.LastPosition < 0 || progress.LastPosition > book.Size {
		return 0, fmt.Errorf("saved position is outside the book")
	}
	ok, err := isFileUTF8Boundary(book.Path, progress.LastPosition, book.Size)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("saved position is inside a UTF-8 character")
	}
	if progress.LastPosition == book.Size {
		return 0, nil
	}
	return progress.LastPosition, nil
}

func saveBookProgress(book Book, pos int64) error {
	data, err := json.Marshal(Progress{LastPosition: pos, Unit: PositionUnit})
	if err != nil {
		return err
	}
	return writeFileReplace(book.SaveFile, data, 0644)
}

func NewLocalAPI(store *BookStore, playback *PlaybackManager, engines engineFactory) *LocalAPI {
	return &LocalAPI{store: store, playback: playback, engines: engines}
}

func (api *LocalAPI) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", api.handleIndex)
	mux.HandleFunc("GET /api/openapi.yaml", api.handleOpenAPI)
	mux.HandleFunc("GET /api/v1/voices", api.handleVoices)
	mux.HandleFunc("POST /api/v1/books", api.handleAddBook)
	mux.HandleFunc("GET /api/v1/books", api.handleListBooks)
	mux.HandleFunc("POST /api/v1/playback", api.handleStartPlayback)
	mux.HandleFunc("GET /api/v1/playback", api.handlePlaybackState)
	mux.HandleFunc("POST /api/v1/playback/pause", api.handlePausePlayback)
	mux.HandleFunc("POST /api/v1/playback/resume", api.handleResumePlayback)
	mux.HandleFunc("POST /api/v1/playback/stop", api.handleStopPlayback)
	mux.HandleFunc("PUT /api/v1/playback/position", api.handleSetPosition)
	mux.HandleFunc("GET /api/v1/events", api.handleEvents)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (api *LocalAPI) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, localDashboardHTML)
}

func (api *LocalAPI) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = io.WriteString(w, openAPISpec)
}

func (api *LocalAPI) handleVoices(w http.ResponseWriter, r *http.Request) {
	voices, err := api.engines(Config{}).Voices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Voice{"voices": voices})
}

func (api *LocalAPI) handleAddBook(w http.ResponseWriter, r *http.Request) {
	var req AddBookRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	book, err := api.store.Add(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, book)
}

func (api *LocalAPI) handleListBooks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]Book{"books": api.store.List()})
}

func (api *LocalAPI) handleStartPlayback(w http.ResponseWriter, r *http.Request) {
	var req StartPlaybackRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.BookID == "" {
		writeError(w, http.StatusBadRequest, "book_id is required")
		return
	}
	book, ok := api.store.Get(req.BookID)
	if !ok {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	snapshot, err := api.playback.Start(book, req)
	if err != nil {
		if strings.Contains(err.Error(), "already active") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, snapshot)
}

func (api *LocalAPI) handlePlaybackState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.playback.Snapshot())
}

func (api *LocalAPI) handlePausePlayback(w http.ResponseWriter, r *http.Request) {
	snapshot, err := api.playback.Pause()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleResumePlayback(w http.ResponseWriter, r *http.Request) {
	snapshot, err := api.playback.Resume()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleStopPlayback(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.playback.Stop())
}

func (api *LocalAPI) handleSetPosition(w http.ResponseWriter, r *http.Request) {
	var req SetPositionRequest
	if err := readJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.BookID == "" {
		writeError(w, http.StatusBadRequest, "book_id is required")
		return
	}
	book, ok := api.store.Get(req.BookID)
	if !ok {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	snapshot, err := api.playback.SetPosition(book, req.CurrentByte)
	if err != nil {
		if strings.Contains(err.Error(), "active") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	events, unsubscribe := api.playback.events.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\n", event.Type)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

const localDashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Audiobook TTS Reader</title>
  <style>
    :root { color-scheme: light dark; font-family: Segoe UI, system-ui, sans-serif; }
    body { margin: 0; background: #f6f7f9; color: #18202a; }
    main { max-width: 960px; margin: 0 auto; padding: 28px; }
    h1 { margin: 0 0 20px; font-size: 28px; }
    section { background: #fff; border: 1px solid #d8dee8; border-radius: 8px; padding: 18px; margin-bottom: 16px; }
    label { display: block; font-size: 13px; font-weight: 600; margin-bottom: 6px; color: #3b4654; }
    input, select { width: 100%; box-sizing: border-box; padding: 10px 12px; border: 1px solid #b9c3d1; border-radius: 6px; font: inherit; }
    button { padding: 10px 14px; border: 0; border-radius: 6px; background: #1d6fd8; color: white; font: inherit; cursor: pointer; }
    button.secondary { background: #465568; }
    button.danger { background: #b42318; }
    button:disabled { opacity: .55; cursor: not-allowed; }
    .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    .actions { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 14px; }
    .state { display: grid; grid-template-columns: repeat(4, 1fr); gap: 10px; }
    .metric { border: 1px solid #d8dee8; border-radius: 8px; padding: 12px; background: #fbfcfe; }
    .metric strong { display: block; font-size: 12px; color: #596779; margin-bottom: 4px; }
    pre { min-height: 140px; overflow: auto; padding: 12px; background: #101820; color: #d8f3dc; border-radius: 8px; }
    @media (max-width: 760px) { .grid, .state { grid-template-columns: 1fr; } main { padding: 16px; } }
  </style>
</head>
<body>
<main>
  <h1>Audiobook TTS Reader</h1>

  <section>
    <div class="grid">
      <div>
        <label for="bookPath">Book path</label>
        <input id="bookPath" placeholder="C:\Books\novel.txt">
      </div>
      <div>
        <label for="title">Title</label>
        <input id="title" placeholder="Optional">
      </div>
      <div>
        <label for="voice">Voice</label>
        <select id="voice"></select>
      </div>
      <div>
        <label for="chunkSize">Chunk size</label>
        <input id="chunkSize" type="number" min="1" value="400">
      </div>
    </div>
    <div class="actions">
      <button id="addBook">Add book</button>
      <button id="play">Play</button>
      <button class="secondary" id="pause">Pause</button>
      <button class="secondary" id="resume">Resume</button>
      <button class="danger" id="stop">Stop</button>
    </div>
  </section>

  <section>
    <div class="state">
      <div class="metric"><strong>State</strong><span id="state">stopped</span></div>
      <div class="metric"><strong>Progress</strong><span id="progress">0%</span></div>
      <div class="metric"><strong>Current byte</strong><span id="currentByte">0</span></div>
      <div class="metric"><strong>Book</strong><span id="bookId">none</span></div>
    </div>
    <div class="actions">
      <input id="position" type="number" min="0" placeholder="Byte position">
      <button class="secondary" id="setPosition">Set position</button>
    </div>
  </section>

  <section>
    <pre id="events"></pre>
  </section>
</main>
<script>
let currentBookId = "";
const $ = (id) => document.getElementById(id);

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) }
  });
  const text = await response.text();
  const data = text ? JSON.parse(text) : {};
  if (!response.ok) throw new Error(data.error || response.statusText);
  return data;
}

function render(snapshot) {
  $("state").textContent = snapshot.state || "unknown";
  $("progress").textContent = Number(snapshot.progress_percent || 0).toFixed(2) + "%";
  $("currentByte").textContent = snapshot.current_byte || 0;
  $("bookId").textContent = snapshot.book_id || currentBookId || "none";
}

function log(line) {
  const box = $("events");
  box.textContent = new Date().toLocaleTimeString() + "  " + line + "\n" + box.textContent;
}

async function refreshState() {
  render(await api("/api/v1/playback"));
}

async function loadVoices() {
  const data = await api("/api/v1/voices");
  $("voice").innerHTML = '<option value="">System default</option>' + data.voices.map((item) => {
    const voice = item.name || item;
    return '<option value="' + voice.replaceAll('"', "&quot;") + '">' + voice + "</option>";
  }
  ).join("");
}

$("addBook").onclick = async () => {
  const book = await api("/api/v1/books", {
    method: "POST",
    body: JSON.stringify({ path: $("bookPath").value, title: $("title").value })
  });
  currentBookId = book.id;
  log("book.added " + book.id);
  await refreshState();
};

$("play").onclick = async () => {
  if (!currentBookId) throw new Error("Add a book first");
  render(await api("/api/v1/playback", {
    method: "POST",
    body: JSON.stringify({
      book_id: currentBookId,
      voice: $("voice").value,
      chunk_size: Number($("chunkSize").value || 400)
    })
  }));
};

$("pause").onclick = async () => render(await api("/api/v1/playback/pause", { method: "POST" }));
$("resume").onclick = async () => render(await api("/api/v1/playback/resume", { method: "POST" }));
$("stop").onclick = async () => render(await api("/api/v1/playback/stop", { method: "POST" }));
$("setPosition").onclick = async () => {
  if (!currentBookId) throw new Error("Add a book first");
  render(await api("/api/v1/playback/position", {
    method: "PUT",
    body: JSON.stringify({ book_id: currentBookId, current_byte: Number($("position").value || 0) })
  }));
};

const source = new EventSource("/api/v1/events");
["playback.started", "chunk.started", "progress.updated", "playback.paused", "playback.resumed", "playback.stopped", "playback.finished", "playback.failed", "position.updated"].forEach((name) => {
  source.addEventListener(name, (event) => {
    const data = JSON.parse(event.data);
    render(data.playback);
    log(name + " " + JSON.stringify(data.playback));
  });
});

loadVoices().then(refreshState).catch((err) => log("error " + err.message));
window.addEventListener("unhandledrejection", (event) => log("error " + event.reason.message));
</script>
</body>
</html>`

func readJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("request body must contain a single JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
