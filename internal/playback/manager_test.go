package playback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	firstBook := mustBook(t, writeTempBook(t, "Перша книга."))
	secondBook := mustBook(t, writeTempBook(t, "Друга книга."))
	firstPath = firstBook.Path
	secondPath = secondBook.Path

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
	oldSession := manager.active
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

	manager.finalizeSession(oldSession, firstBook, sessionResult{state: Failed, position: 0, err: context.Canceled})
	snapshot := manager.Snapshot()
	if snapshot.State == Failed || snapshot.BookID != secondBook.ID {
		t.Fatalf("stale session corrupted active playback: %#v", snapshot)
	}
	assertSavedPosition(t, firstBook.SaveFile, 7)

	close(secondRelease)
	waitState(t, manager, Finished)
}

func TestPlaybackRejectsTruncatedBookDuringPlayback(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var firstSpeak sync.Once
	store := &trackingProgressStore{}
	manager := NewManagerWithProgress(
		func(tts.Config) tts.Engine {
			return &testEngine{speakContext: func(ctx context.Context, text string) error {
				firstSpeak.Do(func() {
					started <- struct{}{}
					<-release
				})
				return nil
			}}
		},
		time.Second,
		NewEventBroker(),
		store,
	)
	registeredBook := mustBook(t, writeTempBook(t, strings.Repeat("Фрагмент тексту. ", 400)))

	if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID, ChunkSize: intPtr(32)}); err != nil {
		t.Fatalf("не вдалося запустити playback: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback не стартував")
	}
	if err := os.Truncate(registeredBook.Path, 0); err != nil {
		close(release)
		t.Fatalf("не вдалося скоротити книгу: %v", err)
	}
	close(release)

	snapshot := waitState(t, manager, Failed)
	if snapshot.ErrorCode != "book_modified" {
		t.Fatalf("очікував book_modified, отримав %#v", snapshot)
	}
	if saved := store.lastSavedPosition(); saved <= 0 {
		t.Fatalf("прогрес не має скидатися після скорочення книги: %d", saved)
	}
}

func TestPlaybackRejectsAppendedContentPastRegisteredSize(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var firstSpeak sync.Once
	store := &trackingProgressStore{}
	manager := NewManagerWithProgress(
		func(tts.Config) tts.Engine {
			return &testEngine{speakContext: func(ctx context.Context, text string) error {
				firstSpeak.Do(func() {
					started <- struct{}{}
					<-release
				})
				return nil
			}}
		},
		time.Second,
		NewEventBroker(),
		store,
	)
	registeredBook := mustBook(t, writeTempBook(t, strings.Repeat("Початковий текст. ", 20)))

	if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID, ChunkSize: intPtr(16)}); err != nil {
		t.Fatalf("не вдалося запустити playback: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback не стартував")
	}
	file, err := os.OpenFile(registeredBook.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		close(release)
		t.Fatalf("не вдалося відкрити книгу для доповнення: %v", err)
	}
	if _, err := file.WriteString(strings.Repeat(" Доданий текст.", 100)); err != nil {
		_ = file.Close()
		close(release)
		t.Fatalf("не вдалося доповнити книгу: %v", err)
	}
	if err := file.Close(); err != nil {
		close(release)
		t.Fatalf("не вдалося закрити доповнену книгу: %v", err)
	}
	close(release)

	snapshot := waitState(t, manager, Failed)
	if snapshot.ErrorCode != "book_modified" {
		t.Fatalf("очікував book_modified, отримав %#v", snapshot)
	}
	if snapshot.CurrentByte > registeredBook.Size || snapshot.ProgressPercent > 100 {
		t.Fatalf("progress вийшов за початковий розмір книги: %#v size=%d", snapshot, registeredBook.Size)
	}
}

func TestPlaybackRejectsSameSizeMiddleEditDuringPlayback(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var firstSpeak sync.Once
	store := &trackingProgressStore{}
	manager := NewManagerWithProgress(
		func(tts.Config) tts.Engine {
			return &testEngine{speakContext: func(ctx context.Context, text string) error {
				firstSpeak.Do(func() {
					started <- struct{}{}
					<-release
				})
				return nil
			}}
		},
		time.Second,
		NewEventBroker(),
		store,
	)
	registeredBook := mustBook(t, writeTempBook(t, strings.Repeat("a", 256<<10)))

	if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID, ChunkSize: intPtr(10000)}); err != nil {
		t.Fatalf("не вдалося запустити playback: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback не стартував")
	}
	file, err := os.OpenFile(registeredBook.Path, os.O_WRONLY, 0)
	if err != nil {
		close(release)
		t.Fatalf("не вдалося відкрити книгу для зміни: %v", err)
	}
	if _, err := file.WriteAt([]byte("changed-middle"), 128<<10); err != nil {
		_ = file.Close()
		close(release)
		t.Fatalf("не вдалося змінити середину книги: %v", err)
	}
	if err := file.Close(); err != nil {
		close(release)
		t.Fatalf("не вдалося закрити змінену книгу: %v", err)
	}
	changedTime := registeredBook.File.ModifiedAt.Add(2 * time.Second)
	if err := os.Chtimes(registeredBook.Path, changedTime, changedTime); err != nil {
		close(release)
		t.Fatalf("не вдалося змінити modification time книги: %v", err)
	}
	close(release)

	snapshot := waitState(t, manager, Failed)
	if snapshot.ErrorCode != "book_modified" {
		t.Fatalf("очікував book_modified, отримав %#v", snapshot)
	}
	if saved := store.lastSavedPosition(); saved <= 0 {
		t.Fatalf("progress не має скидатися після middle edit: %d", saved)
	}
}

func TestPlaybackLeaseBlocksSecondManagerUntilSessionStops(t *testing.T) {
	started := make(chan struct{}, 1)
	firstManager := NewManager(
		func(tts.Config) tts.Engine {
			return &testEngine{speakContext: func(ctx context.Context, text string) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}}
		},
		time.Second,
		NewEventBroker(),
	)
	secondManager := NewManager(
		func(tts.Config) tts.Engine { return &testEngine{} },
		time.Second,
		NewEventBroker(),
	)
	registeredBook := mustBook(t, writeTempBook(t, "Книга з одним фрагментом."))

	if _, err := firstManager.Start(registeredBook, StartRequest{BookID: registeredBook.ID}); err != nil {
		t.Fatalf("не вдалося запустити перший manager: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("перша playback-сесія не стартувала")
	}
	if _, err := secondManager.Start(registeredBook, StartRequest{BookID: registeredBook.ID}); !errors.Is(err, progress.ErrInUse) {
		t.Fatalf("очікував ErrInUse для другого manager, отримав %v", err)
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStop()
	if _, err := firstManager.Stop(stopCtx); err != nil {
		t.Fatalf("не вдалося зупинити перший manager: %v", err)
	}
	if _, err := secondManager.Start(registeredBook, StartRequest{BookID: registeredBook.ID}); err != nil {
		t.Fatalf("lease не звільнився після Stop: %v", err)
	}
	waitState(t, secondManager, Finished)
}

func TestFinishedStateIsPublishedAfterProgressLeaseRelease(t *testing.T) {
	manager := NewManager(
		func(tts.Config) tts.Engine { return &testEngine{} },
		time.Second,
		NewEventBroker(),
	)
	registeredBook := mustBook(t, writeTempBook(t, "Книга."))

	if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID}); err != nil {
		t.Fatalf("не вдалося запустити playback: %v", err)
	}
	waitState(t, manager, Finished)

	lease, err := progress.AcquireLease(registeredBook.SaveFile)
	if err != nil {
		t.Fatalf("finished опубліковано до звільнення progress lease: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("не вдалося звільнити контрольний lease: %v", err)
	}
}

func TestPlaybackStopTimeoutFinalizesWithoutTransientError(t *testing.T) {
	started := make(chan struct{}, 1)
	releaseSpeak := make(chan struct{})
	store := &trackingProgressStore{loadPosition: 4}
	manager := NewManagerWithProgress(
		func(tts.Config) tts.Engine {
			return &testEngine{
				speakContext: func(ctx context.Context, text string) error {
					select {
					case started <- struct{}{}:
					default:
					}
					<-releaseSpeak
					return nil
				},
				stop: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}
		},
		time.Second,
		NewEventBroker(),
		store,
	)
	registeredBook := mustBook(t, writeTempBook(t, "0123456789. Далі."))

	if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID, ChunkSize: intPtr(128)}); err != nil {
		t.Fatalf("не вдалося запустити playback: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback не стартував")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	snapshot, err := manager.Stop(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrStopping) {
		t.Fatalf("очікувалися deadline та ErrStopping, отримано snapshot=%#v err=%v", snapshot, err)
	}
	if snapshot.State != Stopping {
		t.Fatalf("після timeout очікувався stopping, отримано %#v", snapshot)
	}

	close(releaseSpeak)
	snapshot = waitState(t, manager, Stopped)
	if snapshot.ErrorCode != "" {
		t.Fatalf("transient timeout не має залишатися у terminal snapshot: %#v", snapshot)
	}

	manager.mu.Lock()
	active := manager.active
	durable := manager.durablePosition
	current := manager.currentByte
	manager.mu.Unlock()
	if active != nil {
		t.Fatalf("single-owner finalizer не очистив active session")
	}
	if current != durable || durable != store.lastSavedPosition() {
		t.Fatalf("terminal progress не збігається: current=%d durable=%d saved=%d", current, durable, store.lastSavedPosition())
	}
}

func TestPlaybackEventsPreserveTransitionOrder(t *testing.T) {
	events := newBlockingEventStream()
	manager := newManagerWithEventStream(
		func(tts.Config) tts.Engine {
			return &testEngine{speakContext: func(ctx context.Context, text string) error {
				<-ctx.Done()
				return ctx.Err()
			}}
		},
		time.Second,
		events,
		&trackingProgressStore{},
	)
	registeredBook := mustBook(t, writeTempBook(t, "Порядок подій."))
	if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID, ChunkSize: intPtr(128)}); err != nil {
		t.Fatalf("не вдалося запустити playback: %v", err)
	}

	manager.mu.Lock()
	sessionID := manager.active.id
	manager.mu.Unlock()
	progressDone := make(chan struct{})
	go func() {
		manager.updateProgress(sessionID, "progress.updated", 0)
		close(progressDone)
	}()

	select {
	case <-events.progressPrepared:
	case <-time.After(2 * time.Second):
		t.Fatal("progress event не дійшов до контрольованої затримки")
	}

	stopDone := make(chan error, 1)
	go func() {
		_, err := manager.Stop(context.Background())
		stopDone <- err
	}()

	select {
	case err := <-stopDone:
		t.Fatalf("Stop не має обігнати заблокований progress event: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(events.releaseProgress)
	<-progressDone
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop завершився з помилкою: %v", err)
	}

	assertNoActiveStateAfterStopping(t, events.snapshot())
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

func TestPlaybackManagerBeginShutdownDoesNotWaitForInFlightStartIO(t *testing.T) {
	loadStarted := make(chan struct{}, 1)
	releaseLoad := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseLoad) })
	}
	defer release()
	manager := NewManagerWithProgress(
		func(tts.Config) tts.Engine {
			return &testEngine{speakContext: func(ctx context.Context, text string) error {
				<-ctx.Done()
				return ctx.Err()
			}}
		},
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

	shutdownDone := make(chan struct{})
	go func() {
		manager.BeginShutdown()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("BeginShutdown заблокувався на файловому I/O операції Start")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelStop()
	stopDone := make(chan error, 1)
	go func() {
		_, err := manager.Stop(stopCtx)
		stopDone <- err
	}()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop під час shutdown не має чекати in-flight Start: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop під час shutdown заблокувався на файловому I/O операції Start")
	}

	release()

	select {
	case err := <-startErr:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("in-flight Start має відхилитися після shutdown boundary: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start не завершився після shutdown")
	}

	if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("нова операція після BeginShutdown має повертати ErrShuttingDown, отримано %v", err)
	}
	if snapshot := manager.Snapshot(); snapshot.State != Stopped {
		t.Fatalf("in-flight Start створив playback session після shutdown: %#v", snapshot)
	}
}

func TestPauseBlocksAlreadyReadChunkUntilResume(t *testing.T) {
	spoken := make(chan string, 1)
	engineFactory := func(tts.Config) tts.Engine {
		return &testEngine{speakContext: func(ctx context.Context, text string) error {
			spoken <- text
			return nil
		}}
	}
	manager := NewManager(engineFactory, time.Second, NewEventBroker())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &playbackSession{id: 1, ctx: ctx, cancel: cancel, engine: engineFactory(tts.Config{})}

	manager.mu.Lock()
	manager.state = Playing
	manager.active = session
	manager.mu.Unlock()

	if !manager.waitUntilPlayable(session) {
		t.Fatal("worker не пройшов початкову перевірку Playing")
	}
	if snapshot, err := manager.Pause(); err != nil || snapshot.State != Paused {
		t.Fatalf("Pause не підтвердив paused: snapshot=%#v err=%v", snapshot, err)
	}

	done := make(chan bool, 1)
	go func() {
		if !manager.admitChunk(session, 10) {
			done <- false
			return
		}
		_ = session.engine.Speak(session.ctx, "Другий фрагмент")
		done <- true
	}()

	select {
	case text := <-spoken:
		t.Fatalf("новий фрагмент стартував після підтвердження Pause: %q", text)
	case <-time.After(50 * time.Millisecond):
	}
	if snapshot := manager.Snapshot(); snapshot.State != Paused || snapshot.CurrentByte == 10 {
		t.Fatalf("chunk був допущений під час Pause: %#v", snapshot)
	}

	if snapshot, err := manager.Resume(); err != nil || snapshot.State != Playing {
		t.Fatalf("Resume не відновив playback: snapshot=%#v err=%v", snapshot, err)
	}
	select {
	case text := <-spoken:
		if text != "Другий фрагмент" {
			t.Fatalf("озвучено не той збережений фрагмент: %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("збережений фрагмент не стартував після Resume")
	}
	if admitted := <-done; !admitted {
		t.Fatal("збережений фрагмент було втрачено після Resume")
	}
}

func TestPlaybackManagerSubscriptionStartsWithSnapshot(t *testing.T) {
	manager := NewManager(func(tts.Config) tts.Engine { return &testEngine{} }, time.Second, NewEventBroker())
	events, unsubscribe := manager.SubscribeEvents()
	defer unsubscribe()

	initial := receivePlaybackEvent(t, events)
	if initial.Type != "playback.snapshot" || initial.Playback.State != Stopped || initial.Seq == 0 {
		t.Fatalf("першою подією має бути актуальний snapshot: %#v", initial)
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

func TestPlaybackStateMachineStress(t *testing.T) {
	registeredBook := mustBook(t, writeTempBook(t, "Перший. Другий. Третій."))

	for iteration := 0; iteration < 10; iteration++ {
		started := make(chan struct{}, 1)
		manager := NewManagerWithProgress(
			func(tts.Config) tts.Engine {
				return &testEngine{speakContext: func(ctx context.Context, text string) error {
					select {
					case started <- struct{}{}:
					default:
					}
					<-ctx.Done()
					return ctx.Err()
				}}
			},
			time.Second,
			NewEventBroker(),
			&trackingProgressStore{},
		)

		eventChannel, unsubscribe := manager.SubscribeEvents()
		var eventMu sync.Mutex
		var received []PlaybackEvent
		collectorDone := make(chan struct{})
		go func() {
			defer close(collectorDone)
			for event := range eventChannel {
				eventMu.Lock()
				received = append(received, event)
				eventMu.Unlock()
			}
		}()

		if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID, ChunkSize: intPtr(8)}); err != nil {
			t.Fatalf("iteration %d: не вдалося запустити playback: %v", iteration, err)
		}
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: playback не стартував", iteration)
		}

		operations := []func(){
			func() { _, _ = manager.Pause() },
			func() { _, _ = manager.Resume() },
			func() { _, _ = manager.SetPosition(registeredBook, 0) },
			func() {
				events, cancel := manager.SubscribeEvents()
				select {
				case <-events:
				case <-time.After(time.Second):
					t.Errorf("iteration %d: atomic subscription не отримала snapshot", iteration)
				}
				cancel()
			},
			manager.BeginShutdown,
			func() { _, _ = manager.Stop(context.Background()) },
		}

		var wg sync.WaitGroup
		wg.Add(len(operations))
		for _, operation := range operations {
			operation := operation
			go func() {
				defer wg.Done()
				operation()
				assertPlaybackInvariants(t, manager)
			}()
		}
		wg.Wait()

		if snapshot := waitState(t, manager, Stopped); snapshot.ErrorCode != "" {
			t.Fatalf("iteration %d: stopped snapshot містить помилку: %#v", iteration, snapshot)
		}
		if _, err := manager.Start(registeredBook, StartRequest{BookID: registeredBook.ID}); !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("iteration %d: Start після BeginShutdown повернув %v", iteration, err)
		}
		assertPlaybackInvariants(t, manager)

		unsubscribe()
		<-collectorDone
		eventMu.Lock()
		events := append([]PlaybackEvent(nil), received...)
		eventMu.Unlock()
		assertMonotonicTerminalEvents(t, events)
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

type trackingProgressStore struct {
	mu           sync.Mutex
	loadPosition int64
	saved        []int64
}

func (s *trackingProgressStore) Load(book.Book, int64) (int64, error) {
	return s.loadPosition, nil
}

func (s *trackingProgressStore) Save(_ book.Book, position int64) error {
	s.mu.Lock()
	s.saved = append(s.saved, position)
	s.mu.Unlock()
	return nil
}

func (s *trackingProgressStore) Reset(book book.Book) error {
	return s.Save(book, 0)
}

func (s *trackingProgressStore) lastSavedPosition() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.saved) == 0 {
		return -1
	}
	return s.saved[len(s.saved)-1]
}

type blockingEventStream struct {
	mu               sync.Mutex
	nextSeq          uint64
	events           []PlaybackEvent
	progressPrepared chan struct{}
	releaseProgress  chan struct{}
	blockProgress    sync.Once
}

func newBlockingEventStream() *blockingEventStream {
	return &blockingEventStream{
		progressPrepared: make(chan struct{}),
		releaseProgress:  make(chan struct{}),
	}
}

func (s *blockingEventStream) subscribeSnapshot(snapshot PlaybackSnapshot) (<-chan PlaybackEvent, func()) {
	ch := make(chan PlaybackEvent, 1)
	s.mu.Lock()
	s.nextSeq++
	event := PlaybackEvent{Seq: s.nextSeq, Type: "playback.snapshot", Playback: snapshot}
	s.events = append(s.events, event)
	s.mu.Unlock()
	ch <- event
	var once sync.Once
	return ch, func() {
		once.Do(func() { close(ch) })
	}
}

func (s *blockingEventStream) PublishPlayback(eventType string, snapshot PlaybackSnapshot) {
	if eventType == "progress.updated" {
		s.blockProgress.Do(func() {
			close(s.progressPrepared)
			<-s.releaseProgress
		})
	}
	s.mu.Lock()
	s.nextSeq++
	s.events = append(s.events, PlaybackEvent{
		Seq:      s.nextSeq,
		Type:     eventType,
		Playback: snapshot,
	})
	s.mu.Unlock()
}

func (s *blockingEventStream) snapshot() []PlaybackEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PlaybackEvent(nil), s.events...)
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
	registered, err := book.NewStoreWithProgressDir(filepath.Join(t.TempDir(), "progress")).Add(book.AddRequest{Path: path})
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

func receivePlaybackEvent(t *testing.T, events <-chan PlaybackEvent) PlaybackEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("канал playback events закритий")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("playback event не надійшов")
		return PlaybackEvent{}
	}
}

func assertNoActiveStateAfterStopping(t *testing.T, events []PlaybackEvent) {
	t.Helper()
	progressIndex := -1
	stoppingIndex := -1
	stoppedIndex := -1
	for index, event := range events {
		switch event.Type {
		case "progress.updated":
			if progressIndex == -1 {
				progressIndex = index
			}
		case "playback.stopping":
			if stoppingIndex == -1 {
				stoppingIndex = index
			}
		case "playback.stopped":
			stoppedIndex = index
		}
		if stoppingIndex != -1 && (event.Playback.State == Playing || event.Playback.State == Paused) {
			t.Fatalf("active state з'явився після stopping: %#v", events)
		}
	}
	if progressIndex == -1 || stoppingIndex == -1 || stoppedIndex == -1 {
		t.Fatalf("бракує обов'язкових подій: %#v", events)
	}
	if progressIndex > stoppingIndex || stoppingIndex > stoppedIndex {
		t.Fatalf("порушено порядок progress -> stopping -> stopped: %#v", events)
	}
}

func assertPlaybackInvariants(t *testing.T, manager *PlaybackManager) {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()

	activeExpected := manager.state == Playing || manager.state == Paused || manager.state == Stopping
	if activeExpected != (manager.active != nil) {
		t.Errorf("state/active invariant порушено: state=%q active=%v", manager.state, manager.active != nil)
	}
	if manager.currentByte < 0 || manager.currentByte > manager.book.Size {
		t.Errorf("currentByte поза межами книги: current=%d size=%d", manager.currentByte, manager.book.Size)
	}
	if manager.durablePosition < 0 || manager.durablePosition > manager.book.Size {
		t.Errorf("durablePosition поза межами книги: durable=%d size=%d", manager.durablePosition, manager.book.Size)
	}
}

func assertMonotonicTerminalEvents(t *testing.T, events []PlaybackEvent) {
	t.Helper()
	var previousSeq uint64
	activeSeen := false
	terminalSeen := false
	for _, event := range events {
		if event.Seq <= previousSeq {
			t.Fatalf("sequence має строго зростати: previous=%d event=%#v", previousSeq, event)
		}
		previousSeq = event.Seq
		if event.Playback.State == Playing || event.Playback.State == Paused {
			activeSeen = true
		}
		if activeSeen && (event.Playback.State == Stopping || event.Playback.State == Stopped ||
			event.Playback.State == Finished || event.Playback.State == Failed) {
			terminalSeen = true
		}
		if terminalSeen && (event.Playback.State == Playing || event.Playback.State == Paused) {
			t.Fatalf("застарілий active state після terminal transition: %s", fmt.Sprint(events))
		}
	}
	if !terminalSeen {
		t.Fatalf("stress run не містить terminal transition: %#v", events)
	}
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
	if got.BookSize < 0 || got.BookModifiedAtUnixNano == 0 || got.BookFingerprint == "" {
		t.Fatalf("progress не прив'язаний до книги: %#v", got)
	}
}

func intPtr(v int) *int {
	return &v
}
