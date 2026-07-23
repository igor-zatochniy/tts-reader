package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPlaybackManagerIgnoresStaleSessionFailure(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	secondRelease := make(chan struct{})

	var firstPath string
	var secondPath string
	engines := func(cfg Config) TTSEngine {
		return &coreTestEngine{
			speakContext: func(ctx context.Context, text string) error {
				switch cfg.BookFile {
				case firstPath:
					select {
					case firstStarted <- struct{}{}:
					default:
					}
					<-ctx.Done()
					return ctx.Err()
				case secondPath:
					select {
					case secondStarted <- struct{}{}:
					default:
					}
					select {
					case <-secondRelease:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				default:
					return nil
				}
			},
		}
	}
	manager := NewPlaybackManager(engines, time.Second, NewEventBroker())

	firstPath = writeCoreTempBook(t, "Перша книга.")
	secondPath = writeCoreTempBook(t, "Друга книга.")
	firstBook := mustCoreBook(t, firstPath)
	secondBook := mustCoreBook(t, secondPath)

	if _, err := manager.Start(firstBook, StartPlaybackRequest{
		BookID:    firstBook.ID,
		ChunkSize: intPtrCore(64),
	}); err != nil {
		t.Fatalf("не очікував помилку Start для першої книги: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("перша сесія не стартувала")
	}

	manager.mu.Lock()
	oldSessionID := manager.active.id
	manager.mu.Unlock()

	if _, err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("не очікував помилку Stop: %v", err)
	}

	if _, err := manager.Start(secondBook, StartPlaybackRequest{
		BookID:    secondBook.ID,
		ChunkSize: intPtrCore(64),
	}); err != nil {
		t.Fatalf("не очікував помилку Start для другої книги: %v", err)
	}
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("друга сесія не стартувала")
	}

	if err := saveBookProgress(firstBook, 7); err != nil {
		t.Fatalf("не вдалося підготувати прогрес першої книги: %v", err)
	}

	manager.fail(oldSessionID, firstBook, 0, context.Canceled)
	snapshot := manager.Snapshot()
	if snapshot.State == playbackFailed || snapshot.BookID != secondBook.ID {
		t.Fatalf("stale session corrupted active playback: %#v", snapshot)
	}
	assertCoreSavedPosition(t, firstBook.SaveFile, 7)

	close(secondRelease)
	waitCoreState(t, manager, playbackFinished)
}

func TestPlaybackStopUsesDurablePositionAfterSavedChunk(t *testing.T) {
	chunkPersisted := make(chan int64, 1)
	releasePersisted := make(chan struct{})
	stopCalled := make(chan struct{}, 1)
	releaseEngineStop := make(chan struct{})
	progress := &blockingProgressStore{
		base:    JSONProgressStore{},
		saved:   chunkPersisted,
		release: releasePersisted,
	}

	manager := NewPlaybackManagerWithProgress(
		func(cfg Config) TTSEngine {
			return &coreTestEngine{
				stop: func(ctx context.Context) error {
					select {
					case stopCalled <- struct{}{}:
					default:
					}
					<-releaseEngineStop
					return nil
				},
			}
		},
		time.Second,
		NewEventBroker(),
		progress,
	)

	book := mustCoreBook(t, writeCoreTempBook(t, "Перший. Другий."))
	if _, err := manager.Start(book, StartPlaybackRequest{
		BookID:    book.ID,
		ChunkSize: intPtrCore(8),
	}); err != nil {
		t.Fatalf("не очікував помилку Start: %v", err)
	}

	var savedPosition int64
	select {
	case savedPosition = <-chunkPersisted:
	case <-time.After(2 * time.Second):
		t.Fatal("playback не дійшов до збереженого chunk")
	}

	stopDone := make(chan error, 1)
	go func() {
		_, err := manager.Stop(context.Background())
		stopDone <- err
	}()

	select {
	case <-stopCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop не викликав engine.Stop")
	}

	close(releasePersisted)
	close(releaseEngineStop)

	if err := <-stopDone; err != nil {
		t.Fatalf("очікував успішний Stop, отримав %v", err)
	}

	assertCoreSavedPosition(t, book.SaveFile, savedPosition)
	snapshot := manager.Snapshot()
	if snapshot.CurrentByte != savedPosition {
		t.Fatalf("очікував current byte %d, отримав %#v", savedPosition, snapshot)
	}
}

func TestConcurrentStartAndSetPositionMaintainsConsistentState(t *testing.T) {
	book := mustCoreBook(t, writeCoreTempBook(t, "Перший. Другий."))

	for i := 0; i < 100; i++ {
		engines := func(cfg Config) TTSEngine {
			return &coreTestEngine{
				speakContext: func(ctx context.Context, text string) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}
		}
		manager := NewPlaybackManager(engines, time.Second, NewEventBroker())

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = manager.Start(book, StartPlaybackRequest{
				BookID:    book.ID,
				ChunkSize: intPtrCore(64),
			})
		}()
		go func() {
			defer wg.Done()
			_, _ = manager.SetPosition(book, 0)
		}()
		wg.Wait()

		manager.mu.Lock()
		active := manager.active
		state := manager.state
		manager.mu.Unlock()
		if active != nil && state == playbackStopped {
			t.Fatalf("invalid state: active session with stopped state")
		}

		_, _ = manager.Stop(context.Background())
	}
}

func TestFinishFailsWhenProgressResetClearsSession(t *testing.T) {
	resetErr := errors.New("reset denied")
	manager := NewPlaybackManagerWithProgress(
		func(cfg Config) TTSEngine { return &coreTestEngine{} },
		time.Second,
		NewEventBroker(),
		&coreFailingProgressStore{resetErr: resetErr},
	)
	book := mustCoreBook(t, writeCoreTempBook(t, "Кінець."))

	if _, err := manager.Start(book, StartPlaybackRequest{BookID: book.ID, ChunkSize: intPtrCore(128)}); err != nil {
		t.Fatalf("не очікував помилку Start: %v", err)
	}

	snapshot := waitCoreState(t, manager, playbackFailed)
	if snapshot.ErrorCode != "internal_error" {
		t.Fatalf("domain snapshot має містити лише error_code: %#v", snapshot)
	}
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatalf("persistence failure left active session")
	}
}

type coreTestEngine struct {
	speakContext func(ctx context.Context, text string) error
	stop         func(ctx context.Context) error
}

func (e *coreTestEngine) Speak(ctx context.Context, text string) error {
	if e.speakContext == nil {
		return nil
	}
	return e.speakContext(ctx, text)
}

func (e *coreTestEngine) Voices(ctx context.Context) ([]Voice, error) {
	return []Voice{{Name: "Microsoft Irina Desktop"}, {Name: "Microsoft David Desktop"}}, nil
}

func (e *coreTestEngine) Stop(ctx context.Context) error {
	if e.stop != nil {
		return e.stop(ctx)
	}
	return nil
}

type coreFailingProgressStore struct {
	loadErr  error
	saveErr  error
	resetErr error
}

func (s *coreFailingProgressStore) Load(book Book, currentSize int64) (int64, error) {
	if s.loadErr != nil {
		return 0, s.loadErr
	}
	return 0, nil
}

func (s *coreFailingProgressStore) Save(book Book, position int64) error {
	return s.saveErr
}

func (s *coreFailingProgressStore) Reset(book Book) error {
	return s.resetErr
}

type blockingProgressStore struct {
	base    ProgressStore
	saved   chan<- int64
	release <-chan struct{}
	once    sync.Once
}

func (s *blockingProgressStore) Load(book Book, currentSize int64) (int64, error) {
	return s.base.Load(book, currentSize)
}

func (s *blockingProgressStore) Save(book Book, position int64) error {
	if err := s.base.Save(book, position); err != nil {
		return err
	}
	if position <= 0 {
		return nil
	}
	s.once.Do(func() {
		select {
		case s.saved <- position:
		default:
		}
		<-s.release
	})
	return nil
}

func (s *blockingProgressStore) Reset(book Book) error {
	return s.base.Reset(book)
}

func writeCoreTempBook(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.txt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("не вдалося записати книгу: %v", err)
	}
	return path
}

func mustCoreBook(t *testing.T, path string) Book {
	t.Helper()
	book, err := NewBookStore().Add(AddBookRequest{Path: path})
	if err != nil {
		t.Fatalf("не вдалося додати книгу: %v", err)
	}
	return book
}

func waitCoreState(t *testing.T, manager *PlaybackManager, want string) PlaybackSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := manager.Snapshot()
		if snapshot.State == want {
			return snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("стан playback не став %q, останній snapshot: %#v", want, manager.Snapshot())
	return PlaybackSnapshot{}
}

func assertCoreSavedPosition(t *testing.T, path string, want int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не вдалося прочитати прогрес: %v", err)
	}

	var got Progress
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("не вдалося розібрати прогрес: %v", err)
	}
	if got.LastPosition != want {
		t.Fatalf("очікував позицію %d, отримав %d", want, got.LastPosition)
	}
	if got.Version != ProgressVersion {
		t.Fatalf("очікував version %d, отримав %d", ProgressVersion, got.Version)
	}
	if got.PositionUnit != PositionUnit {
		t.Fatalf("очікував position_unit %q, отримав %q", PositionUnit, got.PositionUnit)
	}
	if got.BookSize < 0 || got.BookFingerprint == "" {
		t.Fatalf("progress не прив'язаний до книги: %#v", got)
	}
}

func intPtrCore(v int) *int {
	return &v
}
