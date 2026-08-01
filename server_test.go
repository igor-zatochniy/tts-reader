package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bookpkg "github.com/igor-zatochniy/tts-reader/internal/book"
	apidto "github.com/igor-zatochniy/tts-reader/internal/httpapi"
	playbackpkg "github.com/igor-zatochniy/tts-reader/internal/playback"
	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

func TestLocalAPIRegistersBooksAndListsVoices(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	bookPath := writeTempBook(t, "Книга для API.")

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/books", apidto.AddBookRequest{
		Path:  bookPath,
		Title: "API Book",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("очікував 201, отримав %d: %s", rec.Code, rec.Body.String())
	}

	var book apidto.Book
	decodeJSON(t, rec, &book)
	if book.ID == "" || book.Title != "API Book" || book.Size == 0 {
		t.Fatalf("некоректна відповідь книги: %#v", book)
	}

	rec = performJSON(t, api.Routes(), http.MethodGet, "/api/v1/books", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200, отримав %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Books []apidto.Book `json:"books"`
	}
	decodeJSON(t, rec, &list)
	if len(list.Books) != 1 || list.Books[0].ID != book.ID {
		t.Fatalf("неочікуваний список книг: %#v", list.Books)
	}

	rec = performJSON(t, api.Routes(), http.MethodGet, "/api/v1/voices", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200, отримав %d: %s", rec.Code, rec.Body.String())
	}
	var voices struct {
		Voices []apidto.Voice `json:"voices"`
	}
	decodeJSON(t, rec, &voices)
	if len(voices.Voices) != 2 || voices.Voices[0].Name != "Microsoft Irina Desktop" {
		t.Fatalf("неочікуваний список голосів: %#v", voices.Voices)
	}
}

func TestLocalAPIServesDashboard(t *testing.T) {
	api := newTestLocalAPI(t, nil)

	rec := performJSON(t, api.Routes(), http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200, отримав %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Audiobook TTS Reader") {
		t.Fatalf("dashboard не містить назву застосунку")
	}
	if !strings.Contains(rec.Body.String(), `history.replaceState({}, document.title, "/")`) {
		t.Fatalf("dashboard не прибирає token з адресного рядка")
	}
	if strings.Contains(rec.Body.String(), ".innerHTML") || !strings.Contains(rec.Body.String(), "option.textContent = voice") {
		t.Fatalf("dashboard має створювати voice options через безпечний DOM API")
	}

	rec = performJSON(t, api.Routes(), http.MethodGet, "/api/openapi.yaml", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200 для OpenAPI, отримав %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "openapi: 3.0.3") {
		t.Fatalf("OpenAPI відповідь не схожа на YAML contract")
	}
}

func TestServerShutdownWithOpenSSE(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не вдалося відкрити test listener: %v", err)
	}

	server := newLocalHTTPServer(listener.Addr().String(), api.Routes(), serveCtx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	resp, err := http.Get("http://" + listener.Addr().String() + "/api/v1/events?token=test-token")
	if err != nil {
		t.Fatalf("не вдалося відкрити SSE stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("очікував 200 для SSE, отримав %d", resp.StatusCode)
	}

	done := make(chan error, 1)
	go func() {
		api.BeginShutdown()
		cancelServe()

		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancelShutdown()

		playbackCtx, cancelPlayback := context.WithTimeout(context.Background(), time.Second)
		_, playbackErr := api.playback.Stop(playbackCtx)
		cancelPlayback()

		done <- errors.Join(shutdownErr, playbackErr)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown заблокувався на відкритому SSE stream: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown заблокувався на відкритому SSE stream")
	}

	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("неочікувана помилка server.Serve: %v", err)
	}
}

func TestLocalAPIRejectsPlaybackStartDuringShutdown(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	registeredBook := addTestBook(t, api, writeTempBook(t, "Книга."))

	api.BeginShutdown()

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", apidto.StartPlaybackRequest{
		BookID: registeredBook.ID,
	})
	assertErrorCode(t, rec, http.StatusServiceUnavailable, "service_shutting_down")
	if snapshot := api.playback.Snapshot(); snapshot.State != playbackpkg.Stopped {
		t.Fatalf("shutdown request запустив playback: %#v", snapshot)
	}
}

func TestLocalAPIPlaybackFinishesAndSavesProgress(t *testing.T) {
	var mu sync.Mutex
	var spoken []string
	api := newTestLocalAPI(t, func(text string) error {
		mu.Lock()
		spoken = append(spoken, text)
		mu.Unlock()
		return nil
	})
	bookPath := writeTempBook(t, "Перший. Другий.")
	book := addTestBook(t, api, bookPath)

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", apidto.StartPlaybackRequest{
		BookID:    book.ID,
		Voice:     "Microsoft Irina Desktop",
		ChunkSize: intPtr(8),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("очікував 202, отримав %d: %s", rec.Code, rec.Body.String())
	}

	snapshot := waitForPlaybackState(t, api, playbackpkg.Finished)
	if snapshot.ProgressPercent != 100 {
		t.Fatalf("очікував 100%% прогресу, отримав %.2f", snapshot.ProgressPercent)
	}
	assertSavedPosition(t, book.SaveFile, 0)

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(spoken, "") != "Перший. Другий." {
		t.Fatalf("неочікуваний озвучений текст: %#v", spoken)
	}
}

func TestLocalAPIPauseResumeAndStop(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	api := newTestLocalAPI(t, func(text string) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	book := addTestBook(t, api, writeTempBook(t, "Перший. Другий."))

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", apidto.StartPlaybackRequest{
		BookID:    book.ID,
		ChunkSize: intPtr(8),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("очікував 202, отримав %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("speaker не отримав перший фрагмент")
	}

	rec = performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback/pause", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200 pause, отримав %d: %s", rec.Code, rec.Body.String())
	}
	close(release)
	waitForPlaybackState(t, api, playbackpkg.Paused)

	rec = performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200 resume, отримав %d: %s", rec.Code, rec.Body.String())
	}

	waitForPlaybackState(t, api, playbackpkg.Finished)
	rec = performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback/stop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200 stop, отримав %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlaybackStopWaitsForEngineToStop(t *testing.T) {
	started := make(chan struct{}, 1)
	stopCalled := make(chan struct{}, 1)
	release := make(chan struct{})
	api := newTestLocalAPIWithEngineFactory(t, func(cfg tts.Config) tts.Engine {
		return &testEngine{
			speakContext: func(ctx context.Context, text string) error {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-release:
					return ctx.Err()
				case <-ctx.Done():
					<-release
					return ctx.Err()
				}
			},
			stop: func(ctx context.Context) error {
				select {
				case stopCalled <- struct{}{}:
				default:
				}
				close(release)
				return nil
			},
		}
	})
	book := addTestBook(t, api, writeTempBook(t, "Зупинка має чекати engine."))

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", apidto.StartPlaybackRequest{
		BookID:    book.ID,
		ChunkSize: intPtr(128),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("очікував 202, отримав %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("engine не стартував")
	}

	rec = performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback/stop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200 stop, отримав %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-stopCalled:
	default:
		t.Fatal("Stop не викликав engine.Stop")
	}
	if snapshot := api.playback.Snapshot(); snapshot.State != playbackpkg.Stopped {
		t.Fatalf("очікував stopped після Stop, отримав %#v", snapshot)
	}
}

func TestPlaybackStopTimeoutLeavesSessionStopping(t *testing.T) {
	started := make(chan struct{}, 1)
	releaseSpeak := make(chan struct{})
	manager := playbackpkg.NewManager(
		func(cfg tts.Config) tts.Engine {
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
		playbackpkg.NewEventBroker(),
	)
	book := mustAddBook(t, writeTempBook(t, "Повільне завершення."))

	_, err := manager.Start(book, playbackpkg.StartRequest{
		BookID:    book.ID,
		ChunkSize: intPtr(128),
	})
	if err != nil {
		t.Fatalf("не очікував помилку Start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback не стартував")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	snapshot, err := manager.Stop(ctx)
	if !errors.Is(err, playbackpkg.ErrStopping) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("очікував ErrPlaybackStopping і deadline, отримав snapshot=%#v err=%v", snapshot, err)
	}
	if snapshot.State != playbackpkg.Stopping {
		t.Fatalf("очікував стан stopping після timeout, отримав %#v", snapshot)
	}
	if snapshot.ErrorCode != "playback_stopping" {
		t.Fatalf("domain snapshot має містити лише playback_stopping code: %#v", snapshot)
	}

	_, err = manager.Start(book, playbackpkg.StartRequest{
		BookID:    book.ID,
		ChunkSize: intPtr(128),
	})
	if !errors.Is(err, playbackpkg.ErrStopping) {
		t.Fatalf("очікував ErrPlaybackStopping для нового Start, отримав %v", err)
	}

	close(releaseSpeak)
	if snapshot := waitForManagerState(t, manager, playbackpkg.Stopped); snapshot.State != playbackpkg.Stopped || snapshot.ErrorCode != "" {
		t.Fatalf("після завершення goroutine очікував stopped без transient error, отримав %#v", snapshot)
	}
	if _, err := manager.Start(book, playbackpkg.StartRequest{
		BookID:    book.ID,
		ChunkSize: intPtr(128),
	}); err != nil {
		t.Fatalf("після завершення stopping очікував новий Start без помилки, отримав %v", err)
	}
	waitForManagerState(t, manager, playbackpkg.Finished)
}

func TestConcurrentStartAndSetPosition(t *testing.T) {
	book := mustAddBook(t, writeTempBook(t, "Перший. Другий."))

	for i := 0; i < 100; i++ {
		engines := func(cfg tts.Config) tts.Engine {
			return &testEngine{
				speakContext: func(ctx context.Context, text string) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}
		}
		manager := playbackpkg.NewManager(engines, time.Second, playbackpkg.NewEventBroker())

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = manager.Start(book, playbackpkg.StartRequest{
				BookID:    book.ID,
				ChunkSize: intPtr(64),
			})
		}()
		go func() {
			defer wg.Done()
			_, _ = manager.SetPosition(book, 0)
		}()
		wg.Wait()

		_ = manager.Snapshot()
		_, _ = manager.Stop(context.Background())
	}
}

func TestStopReturnsProgressSaveError(t *testing.T) {
	saveErr := errors.New("disk full")
	started := make(chan struct{}, 1)
	engines := func(cfg tts.Config) tts.Engine {
		return &testEngine{
			speakContext: func(ctx context.Context, text string) error {
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			},
		}
	}
	manager := playbackpkg.NewManagerWithProgress(engines, time.Second, playbackpkg.NewEventBroker(), &failingProgressStore{saveErr: saveErr})
	book := mustAddBook(t, writeTempBook(t, "Збереження падає."))

	_, err := manager.Start(book, playbackpkg.StartRequest{BookID: book.ID, ChunkSize: intPtr(128)})
	if err != nil {
		t.Fatalf("не очікував помилку Start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback не стартував")
	}

	snapshot, err := manager.Stop(context.Background())
	if !errors.Is(err, saveErr) {
		t.Fatalf("очікував saveErr, отримав %v", err)
	}
	if snapshot.State != playbackpkg.Stopped || snapshot.ErrorCode != "internal_error" {
		t.Fatalf("очікував stopped domain snapshot з error_code, отримав %#v", snapshot)
	}
}

func TestStopReturnsEngineStopError(t *testing.T) {
	stopErr := errors.New("engine stop failed")
	started := make(chan struct{}, 1)
	engines := func(cfg tts.Config) tts.Engine {
		return &testEngine{
			speakContext: func(ctx context.Context, text string) error {
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			},
			stop: func(ctx context.Context) error {
				return stopErr
			},
		}
	}
	manager := playbackpkg.NewManagerWithProgress(engines, time.Second, playbackpkg.NewEventBroker(), &failingProgressStore{})
	book := mustAddBook(t, writeTempBook(t, "Engine stop падає."))

	_, err := manager.Start(book, playbackpkg.StartRequest{BookID: book.ID, ChunkSize: intPtr(128)})
	if err != nil {
		t.Fatalf("не очікував помилку Start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback не стартував")
	}

	_, err = manager.Stop(context.Background())
	if !errors.Is(err, stopErr) {
		t.Fatalf("очікував stopErr, отримав %v", err)
	}
}

func TestFinishFailsWhenProgressResetFails(t *testing.T) {
	resetErr := errors.New("reset denied")
	manager := playbackpkg.NewManagerWithProgress(
		func(cfg tts.Config) tts.Engine { return &testEngine{} },
		time.Second,
		playbackpkg.NewEventBroker(),
		&failingProgressStore{resetErr: resetErr},
	)
	book := mustAddBook(t, writeTempBook(t, "Кінець."))

	_, err := manager.Start(book, playbackpkg.StartRequest{BookID: book.ID, ChunkSize: intPtr(128)})
	if err != nil {
		t.Fatalf("не очікував помилку Start: %v", err)
	}

	snapshot := waitForManagerState(t, manager, playbackpkg.Failed)
	if snapshot.ErrorCode != "internal_error" {
		t.Fatalf("domain snapshot має містити лише error_code: %#v", snapshot)
	}
}

func TestPlaybackFailureSanitizesInternalErrors(t *testing.T) {
	playbackErr := errors.New("tts failed")
	saveErr := errors.New("save failed")
	manager := playbackpkg.NewManagerWithProgress(
		func(cfg tts.Config) tts.Engine {
			return &testEngine{
				speakContext: func(ctx context.Context, text string) error {
					return playbackErr
				},
			}
		},
		time.Second,
		playbackpkg.NewEventBroker(),
		&failingProgressStore{saveErr: saveErr},
	)
	book := mustAddBook(t, writeTempBook(t, "Помилка."))

	_, err := manager.Start(book, playbackpkg.StartRequest{BookID: book.ID, ChunkSize: intPtr(128)})
	if err != nil {
		t.Fatalf("не очікував помилку Start: %v", err)
	}

	snapshot := waitForManagerState(t, manager, playbackpkg.Failed)
	if snapshot.ErrorCode != "internal_error" ||
		strings.Contains(snapshot.ErrorCode, playbackErr.Error()) ||
		strings.Contains(snapshot.ErrorCode, saveErr.Error()) {
		t.Fatalf("snapshot leaked internal errors: %#v", snapshot)
	}
}

func TestPublicPlaybackSnapshotMapsErrorCodeToMessage(t *testing.T) {
	snapshot := publicPlaybackSnapshot(playbackpkg.PlaybackSnapshot{
		State:     playbackpkg.Failed,
		ErrorCode: "internal_error",
	})
	if snapshot.Error != "internal server error" || snapshot.ErrorCode != "internal_error" {
		t.Fatalf("неочікуваний public snapshot: %#v", snapshot)
	}
}

func TestLocalAPISanitizesInternalErrors(t *testing.T) {
	internalErr := errors.New(`C:\secret\book_save.json denied`)
	engines := func(cfg tts.Config) tts.Engine { return &testEngine{} }
	api := NewLocalAPI(
		bookpkg.NewStoreWithProgressDir(t.TempDir()),
		playbackpkg.NewManagerWithProgress(engines, time.Second, playbackpkg.NewEventBroker(), &failingProgressStore{loadErr: internalErr}),
		engines,
		"test-token",
	)
	book := addTestBook(t, api, writeTempBook(t, "Книга."))

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", apidto.StartPlaybackRequest{BookID: book.ID})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("очікував 500, отримав %d: %s", rec.Code, rec.Body.String())
	}
	var got apidto.ErrorResponse
	decodeJSON(t, rec, &got)
	if got.Code != "internal_error" || got.Error != "internal server error" {
		t.Fatalf("очікував безпечну помилку, отримав %#v", got)
	}
	if strings.Contains(got.Error, "secret") || strings.Contains(got.Error, "denied") {
		t.Fatalf("HTTP відповідь leaked internal error: %#v", got)
	}
}

func TestLocalAPIRejectsInvalidPositionInsideUTF8Rune(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	book := addTestBook(t, api, writeTempBook(t, "Аудіо"))

	rec := performJSON(t, api.Routes(), http.MethodPut, "/api/v1/playback/position", apidto.SetPositionRequest{
		BookID:      book.ID,
		CurrentByte: int64Ptr(1),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("очікував 400, отримав %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLocalAPIRejectsUnknownJSONFields(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	rec := performRawJSON(t, api.Routes(), http.MethodPost, "/api/v1/books", `{"path":"book.txt","unknown":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("очікував 400, отримав %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLocalAPISecurityRejectsBadHostOriginAndMissingToken(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	bookPath := writeTempBook(t, "Книга.")

	rec := performJSONWithoutToken(t, api.Routes(), http.MethodPost, "/api/v1/books", apidto.AddBookRequest{Path: bookPath})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("очікував 401 без token, отримав %d: %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/books", nil)
	req.Host = "0.0.0.0:8080"
	rec = httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("очікував 403 для bad Host, отримав %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/books", strings.NewReader(`{"path":"`+strings.ReplaceAll(bookPath, `\`, `\\`)+`"}`))
	req.Host = defaultServeAddr
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TTS-Token", "test-token")
	rec = httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("очікував 403 для bad Origin, отримав %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLocalAPIRejectsQueryTokenForMutation(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	bookPath := writeTempBook(t, "Книга.")
	rec := performRawJSONWithoutHeader(
		t,
		api.Routes(),
		http.MethodPost,
		"/api/v1/books?token=test-token",
		`{"path":"`+strings.ReplaceAll(bookPath, `\`, `\\`)+`"}`,
	)
	assertErrorCode(t, rec, http.StatusUnauthorized, "api_token_required")
}

func TestLocalAPIDoesNotExposeInternalBookPaths(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	bookPath := writeTempBook(t, "Книга.")

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/books", apidto.AddBookRequest{Path: bookPath})
	if rec.Code != http.StatusCreated {
		t.Fatalf("очікував 201, отримав %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, bookPath) || strings.Contains(body, "save_file") || strings.Contains(body, "path") {
		t.Fatalf("public book response leaked internal paths: %s", body)
	}
}

func TestStartRejectsExtendedBook(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	bookPath := writeTempBook(t, "Книга.")
	book := addTestBook(t, api, bookPath)
	if err := os.WriteFile(bookPath, []byte("Книга. Новий текст."), 0644); err != nil {
		t.Fatalf("не вдалося змінити книгу: %v", err)
	}

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", apidto.StartPlaybackRequest{BookID: book.ID})
	assertErrorCode(t, rec, http.StatusConflict, "book_modified")
}

func TestStartRejectsTruncatedBook(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	bookPath := writeTempBook(t, "Довга книга.")
	book := addTestBook(t, api, bookPath)
	if err := os.WriteFile(bookPath, []byte("Книга."), 0644); err != nil {
		t.Fatalf("не вдалося змінити книгу: %v", err)
	}

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", apidto.StartPlaybackRequest{BookID: book.ID})
	assertErrorCode(t, rec, http.StatusConflict, "book_modified")
}

func TestStartRejectsBookWithSameSizeButChangedContent(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	bookPath := writeTempBook(t, "ABCDEF")
	book := addTestBook(t, api, bookPath)
	if err := os.WriteFile(bookPath, []byte("UVWXYZ"), 0644); err != nil {
		t.Fatalf("не вдалося змінити книгу: %v", err)
	}
	if err := os.Chtimes(bookPath, book.File.ModifiedAt, book.File.ModifiedAt); err != nil {
		t.Fatalf("не вдалося повернути mtime книги: %v", err)
	}

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", apidto.StartPlaybackRequest{BookID: book.ID})
	assertErrorCode(t, rec, http.StatusConflict, "book_modified")
}

func TestSetPositionRejectsTruncatedBook(t *testing.T) {
	bookPath := writeTempBook(t, "ABCDEF")
	book := mustAddBook(t, bookPath)
	if err := os.WriteFile(bookPath, []byte("ABC"), 0644); err != nil {
		t.Fatalf("не вдалося скоротити книгу: %v", err)
	}

	manager := playbackpkg.NewManager(func(cfg tts.Config) tts.Engine { return &testEngine{} }, time.Second, playbackpkg.NewEventBroker())
	_, err := manager.SetPosition(book, 6)
	if !errors.Is(err, bookpkg.ErrModified) {
		t.Fatalf("очікував ErrBookModified, отримав %v", err)
	}
}

func TestSetPositionRejectsExtendedBook(t *testing.T) {
	bookPath := writeTempBook(t, "ABC")
	book := mustAddBook(t, bookPath)
	if err := os.WriteFile(bookPath, []byte("ABCDEF"), 0644); err != nil {
		t.Fatalf("не вдалося розширити книгу: %v", err)
	}

	manager := playbackpkg.NewManager(func(cfg tts.Config) tts.Engine { return &testEngine{} }, time.Second, playbackpkg.NewEventBroker())
	_, err := manager.SetPosition(book, 3)
	if !errors.Is(err, bookpkg.ErrModified) {
		t.Fatalf("очікував ErrBookModified, отримав %v", err)
	}
}

func TestSetPositionRejectsSameSizeModifiedBook(t *testing.T) {
	bookPath := writeTempBook(t, "ABCDEF")
	book := mustAddBook(t, bookPath)
	if err := os.WriteFile(bookPath, []byte("UVWXYZ"), 0644); err != nil {
		t.Fatalf("не вдалося змінити книгу: %v", err)
	}
	if err := os.Chtimes(bookPath, book.File.ModifiedAt, book.File.ModifiedAt); err != nil {
		t.Fatalf("не вдалося повернути mtime книги: %v", err)
	}

	manager := playbackpkg.NewManager(func(cfg tts.Config) tts.Engine { return &testEngine{} }, time.Second, playbackpkg.NewEventBroker())
	_, err := manager.SetPosition(book, 3)
	if !errors.Is(err, bookpkg.ErrModified) {
		t.Fatalf("очікував ErrBookModified, отримав %v", err)
	}
}

func TestBookStoreRejectsDirectory(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/books", apidto.AddBookRequest{Path: t.TempDir()})
	assertErrorCode(t, rec, http.StatusBadRequest, "book_not_regular")
}

func TestMissingCurrentByteRejected(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	rec := performRawJSON(t, api.Routes(), http.MethodPut, "/api/v1/playback/position", `{"book_id":"book-1"}`)
	assertErrorCode(t, rec, http.StatusBadRequest, "current_byte_required")
}

func TestExplicitZeroChunkSizeRejected(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	book := addTestBook(t, api, writeTempBook(t, "Книга."))

	rec := performRawJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", `{"book_id":"`+book.ID+`","chunk_size":0}`)
	assertErrorCode(t, rec, http.StatusBadRequest, "invalid_chunk_size")
}

func TestChunkSizeAboveMaximumRejected(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	book := addTestBook(t, api, writeTempBook(t, "Книга."))

	rec := performRawJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", `{"book_id":"`+book.ID+`","chunk_size":10001}`)
	assertErrorCode(t, rec, http.StatusBadRequest, "invalid_chunk_size")
}

func TestWrongContentTypeRejected(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/books", strings.NewReader(`{"path":"book.txt"}`))
	req.Host = defaultServeAddr
	req.Header.Set("X-TTS-Token", "test-token")
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)

	assertErrorCode(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
}

func newTestLocalAPI(t *testing.T, speak func(text string) error) *LocalAPI {
	t.Helper()
	if speak == nil {
		speak = func(text string) error { return nil }
	}
	return newTestLocalAPIWithEngineFactory(t, func(cfg tts.Config) tts.Engine {
		return &testEngine{
			speakContext: func(ctx context.Context, text string) error {
				return speak(text)
			},
		}
	})
}

func newTestLocalAPIWithEngineFactory(t *testing.T, engines tts.EngineFactory) *LocalAPI {
	t.Helper()
	events := playbackpkg.NewEventBroker()
	return NewLocalAPI(
		bookpkg.NewStoreWithProgressDir(filepath.Join(t.TempDir(), "progress")),
		playbackpkg.NewManager(engines, time.Second, events),
		engines,
		"test-token",
	)
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

func writeTempBook(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.txt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("не вдалося записати книгу: %v", err)
	}
	return path
}

func mustAddBook(t *testing.T, path string) bookpkg.Book {
	t.Helper()
	book, err := bookpkg.NewStoreWithProgressDir(filepath.Join(t.TempDir(), "progress")).Add(bookpkg.AddRequest{Path: path})
	if err != nil {
		t.Fatalf("не вдалося додати книгу: %v", err)
	}
	return book
}

func addTestBook(t *testing.T, api *LocalAPI, path string) bookpkg.Book {
	t.Helper()
	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/books", apidto.AddBookRequest{Path: path})
	if rec.Code != http.StatusCreated {
		t.Fatalf("очікував 201, отримав %d: %s", rec.Code, rec.Body.String())
	}
	var public apidto.Book
	decodeJSON(t, rec, &public)
	book, ok := api.store.Get(public.ID)
	if !ok {
		t.Fatalf("книга %q не знайдена у store", public.ID)
	}
	return book
}

type failingProgressStore struct {
	loadErr  error
	saveErr  error
	resetErr error
}

func (s *failingProgressStore) Load(book bookpkg.Book, currentSize int64) (int64, error) {
	if s.loadErr != nil {
		return 0, s.loadErr
	}
	return 0, nil
}

func (s *failingProgressStore) Save(book bookpkg.Book, position int64) error {
	return s.saveErr
}

func (s *failingProgressStore) Reset(book bookpkg.Book) error {
	return s.resetErr
}

func performJSON(t *testing.T, handler http.Handler, method string, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatalf("не вдалося серіалізувати payload: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &body)
	req.Host = defaultServeAddr
	req.Header.Set("X-TTS-Token", "test-token")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func performRawJSON(t *testing.T, handler http.Handler, method string, path string, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	req.Host = defaultServeAddr
	req.Header.Set("X-TTS-Token", "test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func performRawJSONWithoutHeader(t *testing.T, handler http.Handler, method string, path string, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	req.Host = defaultServeAddr
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func performJSONWithoutToken(t *testing.T, handler http.Handler, method string, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatalf("не вдалося серіалізувати payload: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &body)
	req.Host = defaultServeAddr
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(target); err != nil {
		t.Fatalf("не вдалося розібрати JSON відповідь: %v; body=%q", err, rec.Body.String())
	}
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("очікував HTTP %d, отримав %d: %s", wantStatus, rec.Code, rec.Body.String())
	}
	var got apidto.ErrorResponse
	decodeJSON(t, rec, &got)
	if got.Code != wantCode {
		t.Fatalf("очікував code %q, отримав %#v", wantCode, got)
	}
}

func waitForPlaybackState(t *testing.T, api *LocalAPI, want string) playbackpkg.PlaybackSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := api.playback.Snapshot()
		if snapshot.State == want {
			return snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("стан playback не став %q, останній snapshot: %#v", want, api.playback.Snapshot())
	return playbackpkg.PlaybackSnapshot{}
}

func waitForManagerState(t *testing.T, manager *playbackpkg.PlaybackManager, want string) playbackpkg.PlaybackSnapshot {
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
	return playbackpkg.PlaybackSnapshot{}
}

func intPtr(v int) *int {
	return &v
}

func int64Ptr(v int64) *int64 {
	return &v
}
