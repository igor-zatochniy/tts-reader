package playback

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/progress"
	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

func TestPlaybackManagerIgnoresStaleSessionFailure(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	secondRelease := make(chan struct{})

	var firstPath string
	var secondPath string
	engines := func(cfg tts.Config) tts.Engine {
		return &testEngine{
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
	manager := NewManager(engines, time.Second, NewEventBroker())

	firstPath = writeTempBook(t, "Перша книга.")
	secondPath = writeTempBook(t, "Друга книга.")
	firstBook := mustBook(t, firstPath)
	secondBook := mustBook(t, secondPath)

	if _, err := manager.Start(firstBook, StartRequest{
		BookID:    firstBook.ID,
		ChunkSize: intPtr(64),
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

	if _, err := manager.Start(secondBook, StartRequest{
		BookID:    secondBook.ID,
		ChunkSize: intPtr(64),
	}); err != nil {
		t.Fatalf("не очікував помилку Start для другої книги: %v", err)
	}
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("друга сесія не стартувала")
	}

	if err := progress.SaveBook(firstBook, 7); err != nil {
		t.Fatalf("не вдалося підготувати прогрес першої книги: %v", err)
	}

	manager.fail(oldSessionID, firstBook, 0, context.Canceled)
	snapshot := manager.Snapshot()
	if snapshot.State == Failed || snapshot.BookID != secondBook.ID {
		t.Fatalf("stale session corrupted active playback: %#v", snapshot)
	}
	assertSavedPosition(t, firstBook.SaveFile, 7)

	close(secondRelease)
	waitState(t, manager, Finished)
}

func TestPlaybackManagerRejectsStartAfterBeginShutdown(t *testing.T) {
	manager := NewManager(func(tts.Config) tts.Engine {
		return &testEngine{}
	}, time.Second, NewEventBroker())
	registeredBook := mustBook(t, writeTempBook(t, "Книга."))

	manager.BeginShutdown()

	if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("очікував ErrShuttingDown, отримав %v", err)
	}
	if snapshot := manager.Snapshot(); snapshot.State != Stopped {
		t.Fatalf("shutdown не має запускати playback: %#v", snapshot)
	}
}

func TestPlaybackManagerRejectsStartThatWasLoadingDuringShutdown(t *testing.T) {
	loadStarted := make(chan struct{}, 1)
	releaseLoad := make(chan struct{})
	manager := NewManagerWithProgress(
		func(tts.Config) tts.Engine { return &testEngine{} },
		time.Second,
		NewEventBroker(),
		&blockingLoadProgressStore{
			started: loadStarted,
			release: releaseLoad,
		},
	)
	registeredBook := mustBook(t, writeTempBook(t, "Книга."))

	startErr := make(chan error, 1)
	go func() {
		_, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID})
		startErr <- err
	}()

	select {
	case <-loadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Start не дійшов до завантаження прогресу")
	}

	manager.BeginShutdown()
	close(releaseLoad)

	select {
	case err := <-startErr:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("очікував ErrShuttingDown, отримав %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start не завершився після shutdown")
	}
	if snapshot := manager.Snapshot(); snapshot.State != Stopped {
		t.Fatalf("shutdown дозволив запустити playback: %#v", snapshot)
	}
}

func TestPlaybackStopUsesDurablePositionAfterSavedChunk(t *testing.T) {
	chunkPersisted := make(chan int64, 1)
	releasePersisted := make(chan struct{})
	stopCalled := make(chan struct{}, 1)
	releaseEngineStop := make(chan struct{})
	progressStore := &blockingProgressStore{
		base:    progress.JSONProgressStore{},
		saved:   chunkPersisted,
		release: releasePersisted,
	}

	manager := NewManagerWithProgress(
		func(cfg tts.Config) tts.Engine {
			return &testEngine{
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
		progressStore,
	)

	book := mustBook(t, writeTempBook(t, "Перший. Другий."))
	if _, err := manager.Start(book, StartRequest{
		BookID:    book.ID,
		ChunkSize: intPtr(8),
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

	assertSavedPosition(t, book.SaveFile, savedPosition)
	snapshot := manager.Snapshot()
	if snapshot.CurrentByte != savedPosition {
		t.Fatalf("очікував current byte %d, отримав %#v", savedPosition, snapshot)
	}
}

func TestConcurrentStartAndSetPositionMaintainsConsistentState(t *testing.T) {
	book := mustBook(t, writeTempBook(t, "Перший. Другий."))

	for i := 0; i < 100; i++ {
		engines := func(cfg tts.Config) tts.Engine {
			return &testEngine{
				speakContext: func(ctx context.Context, text string) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}
		}
		manager := NewManager(engines, time.Second, NewEventBroker())

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = manager.Start(book, StartRequest{
				BookID:    book.ID,
				ChunkSize: intPtr(64),
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
		if active != nil && state == Stopped {
			t.Fatalf("invalid state: active session with stopped state")
		}

		_, _ = manager.Stop(context.Background())
	}
}

func TestFinishFailsWhenProgressResetClearsSession(t *testing.T) {
	resetErr := errors.New("reset denied")
	manager := NewManagerWithProgress(
		func(cfg tts.Config) tts.Engine { return &testEngine{} },
		time.Second,
		NewEventBroker(),
		&failingProgressStore{resetErr: resetErr},
	)
	book := mustBook(t, writeTempBook(t, "Кінець."))

	if _, err := manager.Start(book, StartRequest{BookID: book.ID, ChunkSize: intPtr(128)}); err != nil {
		t.Fatalf("не очікував помилку Start: %v", err)
	}

	snapshot := waitState(t, manager, Failed)
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

type testEngine struct {
	speakContext func(ctx context.Context, text string) error
	stop         func(ctx context.Context) error
}

func (e *testEngine) Speak(ctx context.Context, text string) error {
	if e.speakContext == nil {
		return nil
	}
	return e.speakContext(ctx, text)
}

func (e *testEngine) Voices(ctx context.Context) ([]tts.Voice, error) {
	return []tts.Voice{{Name: "Microsoft Irina Desktop"}, {Name: "Microsoft David Desktop"}}, nil
}

func (e *testEngine) Stop(ctx context.Context) error {
	if e.stop != nil {
		return e.stop(ctx)
	}
	return nil
}

type failingProgressStore struct {
	loadErr  error
	saveErr  error
	resetErr error
}

func (s *failingProgressStore) Load(book book.Book, currentSize int64) (int64, error) {
	if s.loadErr != nil {
		return 0, s.loadErr
	}
	return 0, nil
}

func (s *failingProgressStore) Save(book book.Book, position int64) error {
	return s.saveErr
}

func (s *failingProgressStore) Reset(book book.Book) error {
	return s.resetErr
}

type blockingProgressStore struct {
	base    progress.ProgressStore
	saved   chan<- int64
	release <-chan struct{}
	once    sync.Once
}

type blockingLoadProgressStore struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (s *blockingLoadProgressStore) Load(book.Book, int64) (int64, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	return 0, nil
}

func (s *blockingLoadProgressStore) Save(book.Book, int64) error {
	return nil
}

func (s *blockingLoadProgressStore) Reset(book.Book) error {
	return nil
}

func (s *blockingProgressStore) Load(book book.Book, currentSize int64) (int64, error) {
	return s.base.Load(book, currentSize)
}

func (s *blockingProgressStore) Save(book book.Book, position int64) error {
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

func (s *blockingProgressStore) Reset(book book.Book) error {
	return s.base.Reset(book)
}

func writeTempBook(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.txt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("не вдалося записати книгу: %v", err)
	}
	return path
}

func mustBook(t *testing.T, path string) book.Book {
	t.Helper()
	registered, err := book.NewStore().Add(book.AddRequest{Path: path})
	if err != nil {
		t.Fatalf("не вдалося додати книгу: %v", err)
	}
	return registered
}

func waitState(t *testing.T, manager *PlaybackManager, want string) PlaybackSnapshot {
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

func assertSavedPosition(t *testing.T, path string, want int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не вдалося прочитати прогрес: %v", err)
	}

	var got progress.Progress
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("не вдалося розібрати прогрес: %v", err)
	}
	if got.LastPosition != want {
		t.Fatalf("очікував позицію %d, отримав %d", want, got.LastPosition)
	}
	if got.Version != progress.Version {
		t.Fatalf("очікував version %d, отримав %d", progress.Version, got.Version)
	}
	if got.PositionUnit != progress.Unit {
		t.Fatalf("очікував position_unit %q, отримав %q", progress.Unit, got.PositionUnit)
	}
	if got.BookSize < 0 || got.BookFingerprint == "" {
		t.Fatalf("progress не прив'язаний до книги: %#v", got)
	}
}

func intPtr(v int) *int {
	return &v
}
