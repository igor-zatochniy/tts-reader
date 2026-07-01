package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
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

	maxChunkSize = 10000
)

var (
	ErrPlaybackActive      = errors.New("playback active")
	ErrPlaybackNotPlaying  = errors.New("playback not playing")
	ErrPlaybackNotPaused   = errors.New("playback not paused")
	ErrBookModified        = errors.New("book modified")
	ErrBookNotFound        = errors.New("book not found")
	ErrBookNotReadable     = errors.New("book not readable")
	ErrBookNotRegular      = errors.New("book must be a regular file")
	ErrPathRequired        = errors.New("path required")
	ErrBookIDRequired      = errors.New("book_id required")
	ErrCurrentByteRequired = errors.New("current_byte required")
	ErrPositionOutsideBook = errors.New("position outside book")
	ErrPositionInsideRune  = errors.New("position inside UTF-8 rune")
	ErrInvalidChunkSize    = errors.New("invalid chunk_size")
	ErrUnsupportedMedia    = errors.New("unsupported media type")
	ErrInvalidJSON         = errors.New("invalid JSON request")
)

type voiceProvider func() ([]string, error)

type ServeConfig struct {
	Addr       string
	TTSTimeout time.Duration
}

type Book struct {
	ID        string           `json:"-"`
	Title     string           `json:"-"`
	Path      string           `json:"-"`
	SaveFile  string           `json:"-"`
	Size      int64            `json:"-"`
	File      BookFileIdentity `json:"-"`
	CreatedAt time.Time        `json:"-"`
}

type BookFileIdentity struct {
	Size        int64
	ModifiedAt  time.Time
	Fingerprint string
}

type PublicBook struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Size  int64  `json:"size"`
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

type playbackSession struct {
	id     uint64
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	engine TTSEngine
}

type PlaybackManager struct {
	controlMu  sync.Mutex
	mu         sync.Mutex
	cond       *sync.Cond
	engines    engineFactory
	ttsTimeout time.Duration
	events     *EventBroker
	progress   ProgressStore

	state       string
	book        Book
	currentByte int64
	voice       string
	chunkSize   int
	errMessage  string
	nextID      uint64
	active      *playbackSession
}

type LocalAPI struct {
	store    *BookStore
	playback *PlaybackManager
	engines  engineFactory
	token    string
}

type AddBookRequest struct {
	Path  string `json:"path"`
	Title string `json:"title"`
}

type StartPlaybackRequest struct {
	BookID    string `json:"book_id"`
	Voice     string `json:"voice,omitempty"`
	ChunkSize *int   `json:"chunk_size,omitempty"`
}

type SetPositionRequest struct {
	BookID      string `json:"book_id"`
	CurrentByte *int64 `json:"current_byte"`
}

type ErrorResponse struct {
	Code     string            `json:"code"`
	Error    string            `json:"error"`
	Playback *PlaybackSnapshot `json:"playback,omitempty"`
}

type ProgressStore interface {
	Load(book Book, currentSize int64) (int64, error)
	Save(book Book, position int64) error
	Reset(book Book) error
}

type JSONProgressStore struct{}

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
	if !isLoopbackHost(cfg.Addr) {
		return ServeConfig{}, fmt.Errorf("значення -addr має бути loopback адресою, наприклад %s", defaultServeAddr)
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
	token, err := generateAPIToken()
	if err != nil {
		fmt.Fprintf(stderr, "Помилка: не вдалося створити API token: %v\n", err)
		return 1
	}
	api := NewLocalAPI(NewBookStore(), NewPlaybackManager(engines, cfg.TTSTimeout, events), engines, token)
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

	fmt.Fprintf(stdout, "Local TTS API listening on http://%s/?token=%s\n", cfg.Addr, token)
	if !enableSignals {
		return waitHTTPServer(stderr, errCh)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = api.playback.Stop(shutdownCtx)
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

func generateAPIToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if parsedHost, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isAllowedOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host) && isLoopbackHost(parsed.Host)
}

func (api *LocalAPI) requiresToken(r *http.Request) bool {
	if api.token == "" {
		return false
	}
	if r.URL.Path == "/api/v1/events" {
		return true
	}
	return r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete
}

func (api *LocalAPI) authorized(r *http.Request) bool {
	if api.token == "" {
		return true
	}
	token := r.Header.Get("X-TTS-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(api.token)) == 1
}

func NewBookStore() *BookStore {
	return &BookStore{books: make(map[string]Book)}
}

func (s *BookStore) Add(req AddBookRequest) (Book, error) {
	if strings.TrimSpace(req.Path) == "" {
		return Book{}, ErrPathRequired
	}

	absPath, err := filepath.Abs(req.Path)
	if err != nil {
		return Book{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
	}
	identity, err := inspectBookFile(absPath)
	if err != nil {
		return Book{}, err
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
		SaveFile:  defaultProgressPath(absPath),
		Size:      identity.Size,
		File:      identity,
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

func publicBook(book Book) PublicBook {
	return PublicBook{ID: book.ID, Title: book.Title, Size: book.Size}
}

func publicBooks(books []Book) []PublicBook {
	result := make([]PublicBook, 0, len(books))
	for _, book := range books {
		result = append(result, publicBook(book))
	}
	return result
}

func defaultProgressPath(bookPath string) string {
	ext := filepath.Ext(bookPath)
	if ext == "" {
		return bookPath + ".progress.json"
	}
	return strings.TrimSuffix(bookPath, ext) + ".progress.json"
}

func inspectBookFile(path string) (BookFileIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
	}
	if !info.Mode().IsRegular() {
		return BookFileIdentity{}, ErrBookNotRegular
	}

	hash := sha256.New()
	fmt.Fprintf(hash, "size:%d\n", info.Size())

	const sampleSize int64 = 64 << 10
	headSize := minInt64(info.Size(), sampleSize)
	if headSize > 0 {
		if _, err := io.CopyN(hash, file, headSize); err != nil {
			return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
		}
	}
	if info.Size() > sampleSize {
		if _, err := file.Seek(info.Size()-sampleSize, io.SeekStart); err != nil {
			return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
		}
		if _, err := io.CopyN(hash, file, sampleSize); err != nil {
			return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
		}
	}

	return BookFileIdentity{
		Size:        info.Size(),
		ModifiedAt:  info.ModTime().UTC(),
		Fingerprint: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func sameBookFile(registered BookFileIdentity, current BookFileIdentity) bool {
	return registered.Size == current.Size &&
		registered.ModifiedAt.Equal(current.ModifiedAt) &&
		registered.Fingerprint == current.Fingerprint
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func validateStartPlaybackRequest(req StartPlaybackRequest) (int, error) {
	if strings.TrimSpace(req.BookID) == "" {
		return 0, ErrBookIDRequired
	}
	if req.ChunkSize == nil {
		return defaultChunkSize, nil
	}
	if *req.ChunkSize < 1 || *req.ChunkSize > maxChunkSize {
		return 0, fmt.Errorf("%w: chunk_size must be between 1 and %d", ErrInvalidChunkSize, maxChunkSize)
	}
	return *req.ChunkSize, nil
}

func validateSetPositionRequest(req SetPositionRequest) (int64, error) {
	if strings.TrimSpace(req.BookID) == "" {
		return 0, ErrBookIDRequired
	}
	if req.CurrentByte == nil {
		return 0, ErrCurrentByteRequired
	}
	if *req.CurrentByte < 0 {
		return 0, ErrPositionOutsideBook
	}
	return *req.CurrentByte, nil
}

func requireJSONContentType(r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return ErrUnsupportedMedia
	}
	return nil
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
	return NewPlaybackManagerWithProgress(engines, ttsTimeout, events, JSONProgressStore{})
}

func NewPlaybackManagerWithProgress(engines engineFactory, ttsTimeout time.Duration, events *EventBroker, progress ProgressStore) *PlaybackManager {
	if progress == nil {
		progress = JSONProgressStore{}
	}
	m := &PlaybackManager{
		engines:    engines,
		ttsTimeout: ttsTimeout,
		events:     events,
		progress:   progress,
		state:      playbackStopped,
		chunkSize:  defaultChunkSize,
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *PlaybackManager) Start(book Book, req StartPlaybackRequest) (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	chunkSize, err := validateStartPlaybackRequest(req)
	if err != nil {
		return PlaybackSnapshot{}, err
	}

	currentFile, err := inspectBookFile(book.Path)
	if err != nil {
		return PlaybackSnapshot{}, fmt.Errorf("inspect current book file: %w", err)
	}
	if !sameBookFile(book.File, currentFile) {
		return PlaybackSnapshot{}, fmt.Errorf("%w: book file changed after registration", ErrBookModified)
	}
	book.Size = currentFile.Size
	book.File = currentFile

	startPos, err := m.progress.Load(book, currentFile.Size)
	if err != nil {
		return PlaybackSnapshot{}, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	engine := m.engines(Config{
		BookFile:   book.Path,
		SaveFile:   book.SaveFile,
		Voice:      req.Voice,
		ChunkSize:  chunkSize,
		TTSTimeout: m.ttsTimeout,
	})

	m.mu.Lock()
	if m.active != nil || m.state == playbackPlaying || m.state == playbackPaused {
		m.mu.Unlock()
		cancel()
		return PlaybackSnapshot{}, ErrPlaybackActive
	}
	m.nextID++
	session := &playbackSession{
		id:     m.nextID,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		engine: engine,
	}
	m.state = playbackPlaying
	m.book = book
	m.currentByte = startPos
	m.voice = req.Voice
	m.chunkSize = chunkSize
	m.errMessage = ""
	m.active = session
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.started", snapshot)
	go m.play(session, book, startPos, chunkSize)
	return snapshot, nil
}

func (m *PlaybackManager) Pause() (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	m.mu.Lock()
	if m.state != playbackPlaying {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrPlaybackNotPlaying
	}
	m.state = playbackPaused
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.paused", snapshot)
	return snapshot, nil
}

func (m *PlaybackManager) Resume() (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	m.mu.Lock()
	if m.state != playbackPaused {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrPlaybackNotPaused
	}
	m.state = playbackPlaying
	m.cond.Broadcast()
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.resumed", snapshot)
	return snapshot, nil
}

func (m *PlaybackManager) Stop(ctx context.Context) (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	var session *playbackSession
	var book Book
	var pos int64

	m.mu.Lock()
	session = m.active
	if m.book.ID != "" {
		book = m.book
		pos = m.currentByte
	}
	m.state = playbackStopped
	m.errMessage = ""
	m.cond.Broadcast()
	m.mu.Unlock()

	var stopErr error
	if session != nil {
		session.cancel()
		stopErr = errors.Join(stopErr, session.engine.Stop(ctx))
		select {
		case <-session.done:
		case <-ctx.Done():
			stopErr = errors.Join(stopErr, ctx.Err())
		}
	}
	if book.ID != "" {
		stopErr = errors.Join(stopErr, m.progress.Save(book, pos))
	}

	m.mu.Lock()
	if m.active == session {
		m.active = nil
	}
	m.state = playbackStopped
	if stopErr != nil {
		m.errMessage = stopErr.Error()
	} else {
		m.errMessage = ""
	}
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.stopped", snapshot)
	return snapshot, stopErr
}

func (m *PlaybackManager) SetPosition(book Book, pos int64) (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	m.mu.Lock()
	if m.active != nil || m.state == playbackPlaying || m.state == playbackPaused {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrPlaybackActive
	}
	m.mu.Unlock()

	if pos < 0 || pos > book.Size {
		return PlaybackSnapshot{}, ErrPositionOutsideBook
	}
	ok, err := isFileUTF8Boundary(book.Path, pos, book.Size)
	if err != nil {
		return PlaybackSnapshot{}, fmt.Errorf("check UTF-8 boundary: %w", err)
	}
	if !ok {
		return PlaybackSnapshot{}, ErrPositionInsideRune
	}
	if err := m.progress.Save(book, pos); err != nil {
		return PlaybackSnapshot{}, fmt.Errorf("save position: %w", err)
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

func (m *PlaybackManager) play(session *playbackSession, book Book, startPos int64, chunkSize int) {
	defer close(session.done)

	file, err := os.Open(book.Path)
	if err != nil {
		m.fail(session.id, book, startPos, err)
		return
	}
	defer file.Close()

	if _, err := file.Seek(startPos, io.SeekStart); err != nil {
		m.fail(session.id, book, startPos, err)
		return
	}

	reader, err := NewStreamingChunkReader(file, startPos, chunkSize)
	if err != nil {
		m.fail(session.id, book, startPos, err)
		return
	}

	for {
		if !m.waitUntilPlayable(session) {
			return
		}

		chunk, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				m.finish(session.id, book)
				return
			}
			m.fail(session.id, book, m.current(), err)
			return
		}

		m.updateProgress(session.id, "chunk.started", chunk.StartByte)
		if err := session.engine.Speak(session.ctx, chunk.Text); err != nil {
			if session.ctx.Err() != nil {
				return
			}
			m.fail(session.id, book, chunk.StartByte, err)
			return
		}
		if session.ctx.Err() != nil {
			return
		}
		if err := m.progress.Save(book, chunk.EndByte); err != nil {
			m.fail(session.id, book, chunk.StartByte, fmt.Errorf("save progress: %w", err))
			return
		}
		m.updateProgress(session.id, "progress.updated", chunk.EndByte)
	}
}

func (m *PlaybackManager) waitUntilPlayable(session *playbackSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.state == playbackPaused && session.ctx.Err() == nil && m.active == session {
		m.cond.Wait()
	}
	return session.ctx.Err() == nil && m.state == playbackPlaying && m.active == session
}

func (m *PlaybackManager) current() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentByte
}

func (m *PlaybackManager) updateProgress(sessionID uint64, eventType string, pos int64) {
	m.mu.Lock()
	if m.active == nil || m.active.id != sessionID {
		m.mu.Unlock()
		return
	}
	if m.state != playbackPlaying && m.state != playbackPaused {
		m.mu.Unlock()
		return
	}
	m.currentByte = pos
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish(eventType, snapshot)
}

func (m *PlaybackManager) finish(sessionID uint64, book Book) {
	if !m.isActiveSession(sessionID) {
		return
	}

	if err := m.progress.Reset(book); err != nil {
		m.completeWithPersistenceFailure(sessionID, book.Size, fmt.Errorf("playback completed but progress reset failed: %w", err))
		return
	}

	m.mu.Lock()
	if !m.sessionIsActiveLocked(sessionID) || m.state == playbackStopped {
		m.mu.Unlock()
		return
	}
	m.state = playbackFinished
	m.currentByte = book.Size
	m.errMessage = ""
	m.active = nil
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.finished", snapshot)
}

func (m *PlaybackManager) fail(sessionID uint64, book Book, pos int64, err error) {
	if !m.isActiveSession(sessionID) {
		return
	}

	finalErr := errors.Join(err, m.progress.Save(book, pos))

	m.mu.Lock()
	if !m.sessionIsActiveLocked(sessionID) || m.state == playbackStopped {
		m.mu.Unlock()
		return
	}
	m.state = playbackFailed
	m.currentByte = pos
	if finalErr != nil {
		m.errMessage = finalErr.Error()
	} else {
		m.errMessage = ""
	}
	m.active = nil
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.failed", snapshot)
}

func (m *PlaybackManager) completeWithPersistenceFailure(sessionID uint64, pos int64, err error) {
	m.mu.Lock()
	if !m.sessionIsActiveLocked(sessionID) || m.state == playbackStopped {
		m.mu.Unlock()
		return
	}
	m.state = playbackFailed
	m.currentByte = pos
	m.errMessage = err.Error()
	m.active = nil
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.failed", snapshot)
}

func (m *PlaybackManager) isActiveSession(sessionID uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionIsActiveLocked(sessionID) && m.state != playbackStopped
}

func (m *PlaybackManager) sessionIsActiveLocked(sessionID uint64) bool {
	return m.active != nil && m.active.id == sessionID
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

func (JSONProgressStore) Load(book Book, currentSize int64) (int64, error) {
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
	if progress.LastPosition < 0 || progress.LastPosition > currentSize {
		return 0, ErrPositionOutsideBook
	}
	ok, err := isFileUTF8Boundary(book.Path, progress.LastPosition, currentSize)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrPositionInsideRune
	}
	if progress.LastPosition == currentSize {
		return 0, nil
	}
	return progress.LastPosition, nil
}

func (JSONProgressStore) Save(book Book, pos int64) error {
	data, err := json.Marshal(Progress{LastPosition: pos, Unit: PositionUnit})
	if err != nil {
		return fmt.Errorf("marshal progress: %w", err)
	}
	if err := writeFileReplace(book.SaveFile, data, 0644); err != nil {
		return fmt.Errorf("replace progress file: %w", err)
	}
	return nil
}

func (s JSONProgressStore) Reset(book Book) error {
	return s.Save(book, 0)
}

func saveBookProgress(book Book, pos int64) error {
	return JSONProgressStore{}.Save(book, pos)
}

func NewLocalAPI(store *BookStore, playback *PlaybackManager, engines engineFactory, token string) *LocalAPI {
	return &LocalAPI{store: store, playback: playback, engines: engines, token: token}
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
		if !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "forbidden", "Host must be a loopback address")
			return
		}
		if !isAllowedOrigin(r) {
			writeError(w, http.StatusForbidden, "forbidden", "Origin is not allowed")
			return
		}
		if api.requiresToken(r) && !api.authorized(r) {
			writeError(w, http.StatusUnauthorized, "api_token_required", "API token is required")
			return
		}
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
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Voice{"voices": voices})
}

func (api *LocalAPI) handleAddBook(w http.ResponseWriter, r *http.Request) {
	var req AddBookRequest
	if err := readJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	book, err := api.store.Add(req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publicBook(book))
}

func (api *LocalAPI) handleListBooks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]PublicBook{"books": publicBooks(api.store.List())})
}

func (api *LocalAPI) handleStartPlayback(w http.ResponseWriter, r *http.Request) {
	var req StartPlaybackRequest
	if err := readJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if _, err := validateStartPlaybackRequest(req); err != nil {
		writeAPIError(w, err)
		return
	}
	book, ok := api.store.Get(req.BookID)
	if !ok {
		writeAPIError(w, ErrBookNotFound)
		return
	}
	snapshot, err := api.playback.Start(book, req)
	if err != nil {
		writeAPIError(w, err)
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
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleResumePlayback(w http.ResponseWriter, r *http.Request) {
	snapshot, err := api.playback.Resume()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleStopPlayback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	snapshot, err := api.playback.Stop(ctx)
	if err != nil {
		writePlaybackError(w, err, snapshot)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleSetPosition(w http.ResponseWriter, r *http.Request) {
	var req SetPositionRequest
	if err := readJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	pos, err := validateSetPositionRequest(req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	book, ok := api.store.Get(req.BookID)
	if !ok {
		writeAPIError(w, ErrBookNotFound)
		return
	}
	snapshot, err := api.playback.SetPosition(book, pos)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_not_supported", "streaming is not supported")
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
const apiToken = new URLSearchParams(location.search).get("token") || "";

async function api(path, options = {}) {
  const headers = { "Content-Type": "application/json", ...(options.headers || {}) };
  if (apiToken) headers["X-TTS-Token"] = apiToken;
  const response = await fetch(path, {
    ...options,
    headers
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

const source = new EventSource("/api/v1/events" + (apiToken ? "?token=" + encodeURIComponent(apiToken) : ""));
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
	if err := requireJSONContentType(r); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("%w: request body must contain a single JSON object", ErrInvalidJSON)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, ErrorResponse{Code: code, Error: message})
}

func writeAPIError(w http.ResponseWriter, err error) {
	writeJSON(w, statusForError(err), ErrorResponse{
		Code:  codeForError(err),
		Error: err.Error(),
	})
}

func writePlaybackError(w http.ResponseWriter, err error, snapshot PlaybackSnapshot) {
	writeJSON(w, statusForError(err), ErrorResponse{
		Code:     codeForError(err),
		Error:    err.Error(),
		Playback: &snapshot,
	})
}

func statusForError(err error) int {
	switch {
	case errors.Is(err, ErrPlaybackActive),
		errors.Is(err, ErrBookModified),
		errors.Is(err, ErrPlaybackNotPlaying),
		errors.Is(err, ErrPlaybackNotPaused):
		return http.StatusConflict
	case errors.Is(err, ErrBookNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrUnsupportedMedia):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, ErrPathRequired),
		errors.Is(err, ErrBookNotReadable),
		errors.Is(err, ErrBookNotRegular),
		errors.Is(err, ErrBookIDRequired),
		errors.Is(err, ErrCurrentByteRequired),
		errors.Is(err, ErrPositionOutsideBook),
		errors.Is(err, ErrPositionInsideRune),
		errors.Is(err, ErrInvalidChunkSize),
		errors.Is(err, ErrInvalidJSON):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func codeForError(err error) string {
	switch {
	case errors.Is(err, ErrPlaybackActive):
		return "playback_active"
	case errors.Is(err, ErrPlaybackNotPlaying):
		return "playback_not_playing"
	case errors.Is(err, ErrPlaybackNotPaused):
		return "playback_not_paused"
	case errors.Is(err, ErrBookModified):
		return "book_modified"
	case errors.Is(err, ErrBookNotFound):
		return "book_not_found"
	case errors.Is(err, ErrBookNotReadable):
		return "book_not_readable"
	case errors.Is(err, ErrBookNotRegular):
		return "book_not_regular"
	case errors.Is(err, ErrPathRequired):
		return "path_required"
	case errors.Is(err, ErrBookIDRequired):
		return "book_id_required"
	case errors.Is(err, ErrCurrentByteRequired):
		return "current_byte_required"
	case errors.Is(err, ErrPositionOutsideBook):
		return "position_outside_book"
	case errors.Is(err, ErrPositionInsideRune):
		return "position_inside_utf8_rune"
	case errors.Is(err, ErrInvalidChunkSize):
		return "invalid_chunk_size"
	case errors.Is(err, ErrUnsupportedMedia):
		return "unsupported_media_type"
	case errors.Is(err, ErrInvalidJSON):
		return "invalid_json"
	default:
		return "internal_error"
	}
}
