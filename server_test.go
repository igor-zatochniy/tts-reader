package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalAPIRegistersBooksAndListsVoices(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	bookPath := writeTempBook(t, "Книга для API.")

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/books", AddBookRequest{
		Path:  bookPath,
		Title: "API Book",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("очікував 201, отримав %d: %s", rec.Code, rec.Body.String())
	}

	var book Book
	decodeJSON(t, rec, &book)
	if book.ID == "" || book.Title != "API Book" || book.Size == 0 {
		t.Fatalf("некоректна відповідь книги: %#v", book)
	}

	rec = performJSON(t, api.Routes(), http.MethodGet, "/api/v1/books", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200, отримав %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Books []Book `json:"books"`
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
		Voices []Voice `json:"voices"`
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

	rec = performJSON(t, api.Routes(), http.MethodGet, "/api/openapi.yaml", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200 для OpenAPI, отримав %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "openapi: 3.1.0") {
		t.Fatalf("OpenAPI відповідь не схожа на YAML contract")
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

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", StartPlaybackRequest{
		BookID:    book.ID,
		Voice:     "Microsoft Irina Desktop",
		ChunkSize: 8,
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("очікував 202, отримав %d: %s", rec.Code, rec.Body.String())
	}

	snapshot := waitForPlaybackState(t, api, playbackFinished)
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

	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback", StartPlaybackRequest{
		BookID:    book.ID,
		ChunkSize: 8,
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
	waitForPlaybackState(t, api, playbackPaused)

	rec = performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200 resume, отримав %d: %s", rec.Code, rec.Body.String())
	}

	waitForPlaybackState(t, api, playbackFinished)
	rec = performJSON(t, api.Routes(), http.MethodPost, "/api/v1/playback/stop", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("очікував 200 stop, отримав %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLocalAPIRejectsInvalidPositionInsideUTF8Rune(t *testing.T) {
	api := newTestLocalAPI(t, nil)
	book := addTestBook(t, api, writeTempBook(t, "Аудіо"))

	rec := performJSON(t, api.Routes(), http.MethodPut, "/api/v1/playback/position", SetPositionRequest{
		BookID:      book.ID,
		CurrentByte: 1,
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

func newTestLocalAPI(t *testing.T, speak func(text string) error) *LocalAPI {
	t.Helper()
	if speak == nil {
		speak = func(text string) error { return nil }
	}
	events := NewEventBroker()
	engines := func(cfg Config) TTSEngine {
		return &testEngine{speak: speak}
	}
	return NewLocalAPI(
		NewBookStore(),
		NewPlaybackManager(engines, time.Second, events),
		engines,
	)
}

type testEngine struct {
	speak func(text string) error
}

func (e *testEngine) Speak(ctx context.Context, text string) error {
	return e.speak(text)
}

func (e *testEngine) Voices(ctx context.Context) ([]Voice, error) {
	return []Voice{{Name: "Microsoft Irina Desktop"}, {Name: "Microsoft David Desktop"}}, nil
}

func (e *testEngine) Stop(ctx context.Context) error {
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

func addTestBook(t *testing.T, api *LocalAPI, path string) Book {
	t.Helper()
	rec := performJSON(t, api.Routes(), http.MethodPost, "/api/v1/books", AddBookRequest{Path: path})
	if rec.Code != http.StatusCreated {
		t.Fatalf("очікував 201, отримав %d: %s", rec.Code, rec.Body.String())
	}
	var book Book
	decodeJSON(t, rec, &book)
	return book
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
	req.Header.Set("Content-Type", "application/json")
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

func waitForPlaybackState(t *testing.T, api *LocalAPI, want string) PlaybackSnapshot {
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
	return PlaybackSnapshot{}
}
