package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
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

type playbackSession struct {
	id             uint64
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	engine         TTSEngine
	stopWaitActive bool
}

type PlaybackManager struct {
	controlMu  sync.Mutex
	mu         sync.Mutex
	cond       *sync.Cond
	engines    engineFactory
	ttsTimeout time.Duration
	events     *EventBroker
	progress   ProgressStore

	state             string
	book              Book
	currentByte       int64
	currentChunkStart int64
	durablePosition   int64
	voice             string
	chunkSize         int
	lastErr           error
	nextID            uint64
	active            *playbackSession
}

func validateStartPlaybackRequest(req StartPlaybackRequest) (int, error) {
	if strings.TrimSpace(req.BookID) == "" {
		return 0, ErrBookIDRequired
	}
	if req.ChunkSize == nil {
		return defaultChunkSize, nil
	}
	if err := validateChunkSize(*req.ChunkSize); err != nil {
		return 0, err
	}
	return *req.ChunkSize, nil
}

func validateChunkSize(size int) error {
	if size < 1 || size > maxChunkSize {
		return fmt.Errorf("%w: chunk_size must be between 1 and %d", ErrInvalidChunkSize, maxChunkSize)
	}
	return nil
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
	if m.state == playbackStopping {
		m.mu.Unlock()
		cancel()
		return PlaybackSnapshot{}, ErrPlaybackStopping
	}
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
	m.currentChunkStart = startPos
	m.durablePosition = startPos
	m.voice = req.Voice
	m.chunkSize = chunkSize
	m.lastErr = nil
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

	m.mu.Lock()
	session = m.active
	if m.book.ID != "" {
		book = m.book
	}
	if session != nil {
		m.state = playbackStopping
		session.stopWaitActive = true
	} else {
		m.state = playbackStopped
	}
	m.lastErr = nil
	snapshot := m.snapshotLocked()
	m.cond.Broadcast()
	m.mu.Unlock()

	if session != nil {
		m.publish("playback.stopping", snapshot)
	}

	var stopErr error
	waited := session == nil
	if session != nil {
		session.cancel()
		stopErr = errors.Join(stopErr, session.engine.Stop(ctx))
		select {
		case <-session.done:
			waited = true
		case <-ctx.Done():
			stopErr = errors.Join(stopErr, ctx.Err())
			select {
			case <-session.done:
				waited = true
			default:
			}
		}
	}
	if session != nil && !waited {
		m.mu.Lock()
		if m.active == session {
			session.stopWaitActive = false
		}
		m.mu.Unlock()
		select {
		case <-session.done:
			waited = true
		default:
		}
	}
	var pos int64
	if book.ID != "" {
		m.mu.Lock()
		pos = m.durablePosition
		if waited {
			m.currentByte = pos
			m.currentChunkStart = pos
		}
		m.mu.Unlock()
		if waited {
			stopErr = errors.Join(stopErr, m.progress.Save(book, pos))
		}
	}

	m.mu.Lock()
	publishStopped := false
	stillStopping := false
	if session != nil && m.active == session {
		session.stopWaitActive = false
	}
	if waited {
		if session == nil {
			publishStopped = true
		} else if m.active == session {
			m.active = nil
			publishStopped = true
		}
	}
	if publishStopped {
		m.state = playbackStopped
		if book.ID != "" {
			m.currentByte = pos
			m.currentChunkStart = pos
		}
	} else if session != nil && m.active == session {
		m.state = playbackStopping
		stillStopping = true
	}
	if stillStopping {
		stopErr = errors.Join(stopErr, ErrPlaybackStopping)
	}
	m.lastErr = stopErr
	snapshot = m.snapshotLocked()
	m.mu.Unlock()

	if publishStopped {
		m.publish("playback.stopped", snapshot)
	}
	return snapshot, stopErr
}

func (m *PlaybackManager) SetPosition(book Book, pos int64) (PlaybackSnapshot, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()

	m.mu.Lock()
	if m.state == playbackStopping {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrPlaybackStopping
	}
	if m.active != nil || m.state == playbackPlaying || m.state == playbackPaused {
		snapshot := m.snapshotLocked()
		m.mu.Unlock()
		return snapshot, ErrPlaybackActive
	}
	m.mu.Unlock()

	currentFile, err := inspectBookFile(book.Path)
	if err != nil {
		return PlaybackSnapshot{}, fmt.Errorf("inspect current book file: %w", err)
	}
	if !sameBookFile(book.File, currentFile) {
		return PlaybackSnapshot{}, fmt.Errorf("%w: book file changed after registration", ErrBookModified)
	}
	book.Size = currentFile.Size
	book.File = currentFile

	if pos < 0 || pos > currentFile.Size {
		return PlaybackSnapshot{}, ErrPositionOutsideBook
	}
	ok, err := isFileUTF8Boundary(book.Path, pos, currentFile.Size)
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
	m.currentChunkStart = pos
	m.durablePosition = pos
	m.state = playbackStopped
	m.lastErr = nil
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

func (m *PlaybackManager) Events() *EventBroker {
	return m.events
}

func (m *PlaybackManager) play(session *playbackSession, book Book, startPos int64, chunkSize int) {
	defer m.clearActiveSession(session.id)
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
		m.markDurablePosition(session.id, chunk.EndByte)
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
	if eventType == "chunk.started" {
		m.currentChunkStart = pos
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
	if !m.sessionIsActiveLocked(sessionID) || m.state == playbackStopped || m.state == playbackStopping {
		m.mu.Unlock()
		return
	}
	m.state = playbackFinished
	m.currentByte = book.Size
	m.currentChunkStart = book.Size
	m.durablePosition = 0
	m.lastErr = nil
	m.active = nil
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.finished", snapshot)
}

func (m *PlaybackManager) fail(sessionID uint64, book Book, pos int64, err error) {
	if !m.isActiveSession(sessionID) {
		return
	}

	saveErr := m.progress.Save(book, pos)
	finalErr := errors.Join(err, saveErr)

	m.mu.Lock()
	if !m.sessionIsActiveLocked(sessionID) || m.state == playbackStopped || m.state == playbackStopping {
		m.mu.Unlock()
		return
	}
	m.state = playbackFailed
	m.currentByte = pos
	m.currentChunkStart = pos
	m.lastErr = finalErr
	m.active = nil
	if saveErr == nil {
		m.durablePosition = pos
	}
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.failed", snapshot)
}

func (m *PlaybackManager) completeWithPersistenceFailure(sessionID uint64, pos int64, err error) {
	m.mu.Lock()
	if !m.sessionIsActiveLocked(sessionID) || m.state == playbackStopped || m.state == playbackStopping {
		m.mu.Unlock()
		return
	}
	m.state = playbackFailed
	m.currentByte = pos
	m.currentChunkStart = pos
	m.lastErr = err
	m.active = nil
	snapshot := m.snapshotLocked()
	m.mu.Unlock()

	m.publish("playback.failed", snapshot)
}

func (m *PlaybackManager) isActiveSession(sessionID uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionIsActiveLocked(sessionID) && m.state != playbackStopped && m.state != playbackStopping
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

func (m *PlaybackManager) clearActiveSession(sessionID uint64) {
	m.mu.Lock()
	var snapshot PlaybackSnapshot
	publishStopped := false
	if m.active != nil && m.active.id == sessionID && m.state == playbackStopping && !m.active.stopWaitActive {
		m.active = nil
		m.state = playbackStopped
		m.currentByte = m.durablePosition
		m.currentChunkStart = m.durablePosition
		m.lastErr = nil
		snapshot = m.snapshotLocked()
		publishStopped = true
	}
	m.mu.Unlock()

	if publishStopped {
		m.publish("playback.stopped", snapshot)
	}
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
		ErrorCode:       playbackErrorCode(m.lastErr),
	}
}

func playbackErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrPlaybackStopping) {
		return "playback_stopping"
	}
	return "internal_error"
}

func (m *PlaybackManager) publish(eventType string, snapshot PlaybackSnapshot) {
	m.events.PublishPlayback(eventType, snapshot)
}
