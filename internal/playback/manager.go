package playback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bookpkg "github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/chunk"
	"github.com/igor-zatochniy/tts-reader/internal/events"
	"github.com/igor-zatochniy/tts-reader/internal/progress"
	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

const (
	Stopped  = "stopped"
	Stopping = "stopping"
	Playing  = "playing"
	Paused   = "paused"
	Finished = "finished"
	Failed   = "failed"
)

var (
	ErrActive              = errors.New("playback active")
	ErrStopping            = errors.New("playback stopping")
	ErrNotPlaying          = errors.New("playback not playing")
	ErrNotPaused           = errors.New("playback not paused")
	ErrShuttingDown        = errors.New("playback manager shutting down")
	ErrBookIDRequired      = errors.New("book_id required")
	ErrCurrentByteRequired = errors.New("current_byte required")
)

type PlaybackSnapshot struct {
	State           string  `json:"state"`
	BookID          string  `json:"book_id,omitempty"`
	ProgressPercent float64 `json:"progress_percent"`
	CurrentByte     int64   `json:"current_byte"`
	Voice           string  `json:"voice,omitempty"`
	ChunkSize       int     `json:"chunk_size,omitempty"`
	ErrorCode       string  `json:"error_code,omitempty"`
}

type StartRequest struct {
	BookID    string `json:"book_id"`
	Voice     string `json:"voice,omitempty"`
	ChunkSize *int   `json:"chunk_size,omitempty"`
}

type SetPositionRequest struct {
	BookID      string `json:"book_id"`
	CurrentByte *int64 `json:"current_byte"`
}

type PlaybackEvent struct {
	Seq      uint64           `json:"seq"`
	Type     string           `json:"type"`
	Time     time.Time        `json:"time"`
	Playback PlaybackSnapshot `json:"playback"`
}

type EventBroker struct {
	broker *events.Broker[PlaybackEvent]
}

type playbackEventStream interface {
	subscribeSnapshot(PlaybackSnapshot) (<-chan PlaybackEvent, func())
	PublishPlayback(string, PlaybackSnapshot)
}

type EngineFactory = tts.EngineFactory
type ProgressStore = progress.ProgressStore
type TTSEngine = tts.Engine

func NewEventBroker() *EventBroker {
	return &EventBroker{
		broker: events.NewBroker(events.Options[PlaybackEvent]{
			IsLossy: func(event PlaybackEvent) bool {
				return event.Type == "chunk.started" || event.Type == "progress.updated"
			},
			Sequence: func(event PlaybackEvent) uint64 {
				return event.Seq
			},
			WithMetadata: func(event PlaybackEvent, seq uint64, at time.Time) PlaybackEvent {
				event.Seq = seq
				event.Time = at
				return event
			},
		}),
	}
}

func (b *EventBroker) subscribeSnapshot(snapshot PlaybackSnapshot) (<-chan PlaybackEvent, func()) {
	return b.broker.SubscribeWithInitial(PlaybackEvent{
		Type:     "playback.snapshot",
		Playback: snapshot,
	})
}

func (b *EventBroker) Publish(event PlaybackEvent) {
	b.broker.Publish(event)
}

func (b *EventBroker) PublishPlayback(eventType string, snapshot PlaybackSnapshot) {
	b.broker.Publish(PlaybackEvent{Type: eventType, Playback: snapshot})
}

type playbackSession struct {
	id            uint64
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	engine        TTSEngine
	stopDone      chan struct{}
	stopStarted   bool
	stopFinished  bool
	engineStopErr error
	finalErr      error
}

type sessionResult struct {
	state    string
	position int64
	err      error
}

type PlaybackManager struct {
	controlMu  sync.Mutex
	mu         sync.Mutex
	cond       *sync.Cond
	engines    EngineFactory
	ttsTimeout time.Duration
	events     playbackEventStream
	progress   ProgressStore

	state             string
	book              bookpkg.Book
	currentByte       int64
	currentChunkStart int64
	durablePosition   int64
	voice             string
	chunkSize         int
	lastErr           error
	nextID            uint64
	active            *playbackSession
	shuttingDown      atomic.Bool
}

func ValidateStartRequest(req StartRequest) (int, error) {
	if strings.TrimSpace(req.BookID) == "" {
		return 0, ErrBookIDRequired
	}
	if req.ChunkSize == nil {
		return chunk.DefaultSize, nil
	}
	if err := chunk.ValidateSize(*req.ChunkSize); err != nil {
		return 0, err
	}
	return *req.ChunkSize, nil
}

func ValidateSetPositionRequest(req SetPositionRequest) (int64, error) {
	if strings.TrimSpace(req.BookID) == "" {
		return 0, ErrBookIDRequired
	}
	if req.CurrentByte == nil {
		return 0, ErrCurrentByteRequired
	}
	if *req.CurrentByte < 0 {
		return 0, progress.ErrPositionOutside
	}
	return *req.CurrentByte, nil
}

func NewManager(engines EngineFactory, ttsTimeout time.Duration, events *EventBroker) *PlaybackManager {
	return NewManagerWithProgress(engines, ttsTimeout, events, progress.JSONProgressStore{})
}

func NewManagerWithProgress(engines EngineFactory, ttsTimeout time.Duration, events *EventBroker, progressStore ProgressStore) *PlaybackManager {
	return newManagerWithEventStream(engines, ttsTimeout, events, progressStore)
}

func newManagerWithEventStream(engines EngineFactory, ttsTimeout time.Duration, events playbackEventStream, progressStore ProgressStore) *PlaybackManager {
	if progressStore == nil {
		progressStore = progress.JSONProgressStore{}
	}
	if events == nil {
		events = NewEventBroker()
	}
	m := &PlaybackManager{
		engines:    engines,
		ttsTimeout: ttsTimeout,
		events:     events,
		progress:   progressStore,
		state:      Stopped,
		chunkSize:  chunk.DefaultSize,
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *PlaybackManager) Start(book bookpkg.Book, req StartRequest) (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	if m.shuttingDown.Load() {
		return PlaybackSnapshot{}, ErrShuttingDown
	}

	chunkSize, err := ValidateStartRequest(req)
	if err != nil {
		return PlaybackSnapshot{}, err
	}

	currentFile, err := bookpkg.InspectFile(book.Path)
	if err != nil {
		return PlaybackSnapshot{}, fmt.Errorf("inspect current book file: %w", err)
	}
	if !bookpkg.SameFile(book.File, currentFile) {
		return PlaybackSnapshot{}, fmt.Errorf("%w: book file changed after registration", bookpkg.ErrModified)
	}
	book.Size = currentFile.Size
	book.File = currentFile

	startPos, err := m.progress.Load(book, currentFile.Size)
	if err != nil {
		return PlaybackSnapshot{}, err
	}
	if m.shuttingDown.Load() {
		return PlaybackSnapshot{}, ErrShuttingDown
	}

	ctx, cancel := context.WithCancel(context.Background())
	engine := m.engines(tts.Config{
		BookFile:   book.Path,
		SaveFile:   book.SaveFile,
		Voice:      req.Voice,
		ChunkSize:  chunkSize,
		TTSTimeout: m.ttsTimeout,
	})

	if m.shuttingDown.Load() {
		cancel()
		return PlaybackSnapshot{}, ErrShuttingDown
	}

	m.mu.Lock()
	if m.state == Stopping {
		m.mu.Unlock()
		cancel()
		return PlaybackSnapshot{}, ErrStopping
	}
	if m.active != nil || m.state == Playing || m.state == Paused {
		m.mu.Unlock()
		cancel()
		return PlaybackSnapshot{}, ErrActive
	}
	m.nextID++
	session := &playbackSession{
		id:       m.nextID,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		engine:   engine,
		stopDone: make(chan struct{}),
	}
	m.state = Playing
	m.book = book
	m.currentByte = startPos
	m.currentChunkStart = startPos
	m.durablePosition = startPos
	m.voice = req.Voice
	m.chunkSize = chunkSize
	m.lastErr = nil
	m.active = session
	snapshot := m.publishLocked("playback.started")
	m.mu.Unlock()

	go m.play(session, book, startPos, chunkSize)
	return snapshot, nil
}

func (m *PlaybackManager) Pause() (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	if m.shuttingDown.Load() {
		return m.Snapshot(), ErrShuttingDown
	}

	m.mu.Lock()
	if m.state != Playing {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrNotPlaying
	}
	m.state = Paused
	snapshot := m.publishLocked("playback.paused")
	m.mu.Unlock()

	return snapshot, nil
}

func (m *PlaybackManager) Resume() (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	if m.shuttingDown.Load() {
		return m.Snapshot(), ErrShuttingDown
	}

	m.mu.Lock()
	if m.state != Paused {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrNotPaused
	}
	m.state = Playing
	m.cond.Broadcast()
	snapshot := m.publishLocked("playback.resumed")
	m.mu.Unlock()

	return snapshot, nil
}

func (m *PlaybackManager) Stop(ctx context.Context) (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	session := m.active
	if session == nil {
		m.state = Stopped
		m.lastErr = nil
		snapshot := m.publishLocked("playback.stopped")
		m.mu.Unlock()
		return snapshot, nil
	}

	startStop := !session.stopStarted
	if startStop {
		session.stopStarted = true
		m.state = Stopping
		m.lastErr = nil
		m.cond.Broadcast()
		m.publishLocked("playback.stopping")
	}
	m.mu.Unlock()

	if startStop {
		session.cancel()
		engineStopErr := session.engine.Stop(ctx)
		m.mu.Lock()
		if !session.stopFinished {
			session.engineStopErr = engineStopErr
			session.stopFinished = true
			close(session.stopDone)
		}
		m.mu.Unlock()
	}

	return m.waitForSessionStop(ctx, session)
}

func (m *PlaybackManager) waitForSessionStop(ctx context.Context, session *playbackSession) (PlaybackSnapshot, error) {
	select {
	case <-session.done:
		m.mu.Lock()
		snapshot := m.snapshotLocked()
		finalErr := session.finalErr
		m.mu.Unlock()
		return snapshot, finalErr
	case <-ctx.Done():
		select {
		case <-session.done:
			m.mu.Lock()
			snapshot := m.snapshotLocked()
			finalErr := session.finalErr
			m.mu.Unlock()
			return snapshot, finalErr
		default:
		}
	}

	m.mu.Lock()
	snapshot := m.snapshotLocked()
	if m.active != session {
		finalErr := session.finalErr
		m.mu.Unlock()
		return snapshot, finalErr
	}
	engineStopErr := session.engineStopErr
	m.mu.Unlock()
	snapshot.ErrorCode = "playback_stopping"
	return snapshot, errors.Join(engineStopErr, ctx.Err(), ErrStopping)
}

func (m *PlaybackManager) SetPosition(book bookpkg.Book, pos int64) (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	if m.shuttingDown.Load() {
		return m.Snapshot(), ErrShuttingDown
	}

	m.mu.Lock()
	if m.state == Stopping {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrStopping
	}
	if m.active != nil || m.state == Playing || m.state == Paused {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrActive
	}
	m.mu.Unlock()

	currentFile, err := bookpkg.InspectFile(book.Path)
	if err != nil {
		return PlaybackSnapshot{}, fmt.Errorf("inspect current book file: %w", err)
	}
	if !bookpkg.SameFile(book.File, currentFile) {
		return PlaybackSnapshot{}, fmt.Errorf("%w: book file changed after registration", bookpkg.ErrModified)
	}
	book.Size = currentFile.Size
	book.File = currentFile

	if pos < 0 || pos > currentFile.Size {
		return PlaybackSnapshot{}, progress.ErrPositionOutside
	}
	ok, err := chunk.IsFileUTF8Boundary(book.Path, pos, currentFile.Size)
	if err != nil {
		return PlaybackSnapshot{}, fmt.Errorf("check UTF-8 boundary: %w", err)
	}
	if !ok {
		return PlaybackSnapshot{}, progress.ErrPositionInside
	}
	if err := m.progress.Save(book, pos); err != nil {
		return PlaybackSnapshot{}, fmt.Errorf("save position: %w", err)
	}

	m.mu.Lock()
	m.book = book
	m.currentByte = pos
	m.currentChunkStart = pos
	m.durablePosition = pos
	m.state = Stopped
	m.lastErr = nil
	snapshot := m.publishLocked("position.updated")
	m.mu.Unlock()

	return snapshot, nil
}

func (m *PlaybackManager) Snapshot() PlaybackSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// BeginShutdown забороняє запуск нових операцій відтворення перед фінальним Stop.
func (m *PlaybackManager) BeginShutdown() {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.shuttingDown.Store(true)
}

// SubscribeEvents атомарно підписує клієнта та ставить актуальний snapshot першим у чергу.
func (m *PlaybackManager) SubscribeEvents() (<-chan PlaybackEvent, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events.subscribeSnapshot(m.snapshotLocked())
}

func (m *PlaybackManager) play(session *playbackSession, book bookpkg.Book, startPos int64, chunkSize int) {
	result := m.runPlayback(session, book, startPos, chunkSize)
	m.finalizeSession(session, book, result)
	close(session.done)
}

func (m *PlaybackManager) runPlayback(session *playbackSession, book bookpkg.Book, startPos int64, chunkSize int) sessionResult {
	file, err := os.Open(book.Path)
	if err != nil {
		return sessionResult{state: Failed, position: startPos, err: err}
	}
	defer file.Close()

	if _, err := file.Seek(startPos, io.SeekStart); err != nil {
		return sessionResult{state: Failed, position: startPos, err: err}
	}

	reader, err := chunk.NewStreamingReader(file, startPos, chunkSize)
	if err != nil {
		return sessionResult{state: Failed, position: startPos, err: err}
	}

	for {
		if !m.waitUntilPlayable(session) {
			return sessionResult{state: Stopped}
		}

		chunk, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return sessionResult{state: Finished, position: book.Size}
			}
			return sessionResult{state: Failed, position: m.current(), err: err}
		}

		m.updateProgress(session.id, "chunk.started", chunk.StartByte)
		if err := session.engine.Speak(session.ctx, chunk.Text); err != nil {
			if session.ctx.Err() != nil {
				return sessionResult{state: Stopped}
			}
			return sessionResult{state: Failed, position: chunk.StartByte, err: err}
		}
		if session.ctx.Err() != nil {
			return sessionResult{state: Stopped}
		}
		if err := m.progress.Save(book, chunk.EndByte); err != nil {
			return sessionResult{state: Failed, position: chunk.StartByte, err: fmt.Errorf("save progress: %w", err)}
		}
		m.markDurablePosition(session.id, chunk.EndByte)
		m.updateProgress(session.id, "progress.updated", chunk.EndByte)
	}
}

func (m *PlaybackManager) waitUntilPlayable(session *playbackSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.state == Paused && session.ctx.Err() == nil && m.active == session {
		m.cond.Wait()
	}
	return session.ctx.Err() == nil && m.state == Playing && m.active == session
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
	if m.state != Playing && m.state != Paused {
		m.mu.Unlock()
		return
	}
	if eventType == "chunk.started" {
		m.currentChunkStart = pos
	}
	m.currentByte = pos
	m.publishLocked(eventType)
	m.mu.Unlock()
}

func (m *PlaybackManager) finalizeSession(session *playbackSession, book bookpkg.Book, result sessionResult) {
	m.mu.Lock()
	if m.active != session {
		m.mu.Unlock()
		return
	}

	if result.state == Stopped || m.state == Stopping {
		result.state = Stopped
		for session.stopStarted && !session.stopFinished {
			m.mu.Unlock()
			<-session.stopDone
			m.mu.Lock()
			if m.active != session {
				m.mu.Unlock()
				return
			}
		}
	}

	var eventType string
	var finalErr error
	switch result.state {
	case Finished:
		if err := m.progress.Reset(book); err != nil {
			m.state = Failed
			m.currentByte = book.Size
			m.currentChunkStart = book.Size
			finalErr = fmt.Errorf("playback completed but progress reset failed: %w", err)
			eventType = "playback.failed"
		} else {
			m.state = Finished
			m.currentByte = book.Size
			m.currentChunkStart = book.Size
			m.durablePosition = 0
			eventType = "playback.finished"
		}
	case Failed:
		saveErr := m.progress.Save(book, result.position)
		finalErr = errors.Join(result.err, saveErr)
		m.state = Failed
		m.currentByte = result.position
		m.currentChunkStart = result.position
		if saveErr == nil {
			m.durablePosition = result.position
		}
		eventType = "playback.failed"
	default:
		position := m.durablePosition
		saveErr := m.progress.Save(book, position)
		finalErr = errors.Join(terminalEngineStopError(session.engineStopErr), saveErr)
		m.state = Stopped
		m.currentByte = position
		m.currentChunkStart = position
		eventType = "playback.stopped"
	}

	m.lastErr = finalErr
	m.active = nil
	session.finalErr = finalErr
	m.publishLocked(eventType)
	m.mu.Unlock()
}

func terminalEngineStopError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (m *PlaybackManager) sessionIsActiveLocked(sessionID uint64) bool {
	return m.active != nil && m.active.id == sessionID
}

func (m *PlaybackManager) markDurablePosition(sessionID uint64, pos int64) {
	m.mu.Lock()
	if m.sessionIsActiveLocked(sessionID) {
		m.durablePosition = pos
	}
	m.mu.Unlock()
}

func (m *PlaybackManager) snapshotLocked() PlaybackSnapshot {
	total := m.book.Size
	return PlaybackSnapshot{
		State:           m.state,
		BookID:          m.book.ID,
		ProgressPercent: progress.Percent(m.currentByte, total),
		CurrentByte:     m.currentByte,
		Voice:           m.voice,
		ChunkSize:       m.chunkSize,
		ErrorCode:       playbackErrorCode(m.lastErr),
	}
}

func playbackErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrStopping) {
		return "playback_stopping"
	}
	return "internal_error"
}

func (m *PlaybackManager) publishLocked(eventType string) PlaybackSnapshot {
	snapshot := m.snapshotLocked()
	m.events.PublishPlayback(eventType, snapshot)
	return snapshot
}
