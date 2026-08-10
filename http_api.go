package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/chunk"
	apidto "github.com/igor-zatochniy/tts-reader/internal/httpapi"
	"github.com/igor-zatochniy/tts-reader/internal/playback"
	"github.com/igor-zatochniy/tts-reader/internal/progress"
	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

type LocalAPI struct {
	store        *book.BookStore
	playback     *playback.PlaybackManager
	engines      tts.EngineFactory
	token        string
	shuttingDown atomic.Bool
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
	return changesState(r.Method)
}

func changesState(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func (api *LocalAPI) authorized(r *http.Request) bool {
	if api.token == "" {
		return true
	}
	token := r.Header.Get("X-TTS-Token")
	if token == "" && r.Method == http.MethodGet && r.URL.Path == "/api/v1/events" {
		token = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(api.token)) == 1
}

func requireJSONContentType(r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return ErrUnsupportedMedia
	}
	return nil
}

func NewLocalAPI(store *book.BookStore, playback *playback.PlaybackManager, engines tts.EngineFactory, token string) *LocalAPI {
	return &LocalAPI{store: store, playback: playback, engines: engines, token: token}
}

// BeginShutdown закриває API для нових запитів, які змінюють стан.
func (api *LocalAPI) BeginShutdown() {
	api.shuttingDown.Store(true)
	api.playback.BeginShutdown()
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
		if changesState(r.Method) && api.shuttingDown.Load() {
			writeError(w, http.StatusServiceUnavailable, "service_shutting_down", "service is shutting down")
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
	voices, err := api.engines(tts.Config{}).Voices(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	publicVoices := make([]apidto.Voice, 0, len(voices))
	for _, voice := range voices {
		publicVoices = append(publicVoices, apidto.Voice{Name: voice.Name})
	}
	writeJSON(w, http.StatusOK, apidto.VoicesResponse{Voices: publicVoices})
}

func (api *LocalAPI) handleAddBook(w http.ResponseWriter, r *http.Request) {
	var req apidto.AddBookRequest
	if err := readJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	registeredBook, err := api.store.Add(book.AddRequest{
		Path:  req.Path,
		Title: req.Title,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publicBook(registeredBook))
}

func (api *LocalAPI) handleListBooks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, apidto.BooksResponse{Books: publicBooks(api.store.List())})
}

func (api *LocalAPI) handleStartPlayback(w http.ResponseWriter, r *http.Request) {
	var request apidto.StartPlaybackRequest
	if err := readJSON(w, r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	req := playback.StartRequest{
		BookID:    request.BookID,
		Voice:     request.Voice,
		ChunkSize: request.ChunkSize,
	}
	if _, err := playback.ValidateStartRequest(req); err != nil {
		writeAPIError(w, err)
		return
	}
	registeredBook, ok := api.store.Get(req.BookID)
	if !ok {
		writeAPIError(w, book.ErrNotFound)
		return
	}
	snapshot, err := api.playback.Start(registeredBook, req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writePlaybackSnapshot(w, http.StatusAccepted, snapshot)
}

func (api *LocalAPI) handlePlaybackState(w http.ResponseWriter, r *http.Request) {
	writePlaybackSnapshot(w, http.StatusOK, api.playback.Snapshot())
}

func (api *LocalAPI) handlePausePlayback(w http.ResponseWriter, r *http.Request) {
	snapshot, err := api.playback.Pause()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writePlaybackSnapshot(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleResumePlayback(w http.ResponseWriter, r *http.Request) {
	snapshot, err := api.playback.Resume()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writePlaybackSnapshot(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleStopPlayback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	snapshot, err := api.playback.Stop(ctx)
	if err != nil {
		writePlaybackError(w, err, snapshot)
		return
	}
	writePlaybackSnapshot(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleSetPosition(w http.ResponseWriter, r *http.Request) {
	var request apidto.SetPositionRequest
	if err := readJSON(w, r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	req := playback.SetPositionRequest{
		BookID:      request.BookID,
		CurrentByte: request.CurrentByte,
	}
	pos, err := playback.ValidateSetPositionRequest(req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	registeredBook, ok := api.store.Get(req.BookID)
	if !ok {
		writeAPIError(w, book.ErrNotFound)
		return
	}
	snapshot, err := api.playback.SetPosition(registeredBook, pos)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writePlaybackSnapshot(w, http.StatusOK, snapshot)
}

func (api *LocalAPI) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_not_supported", "streaming is not supported")
		return
	}

	events, unsubscribe := api.playback.SubscribeEvents()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	if _, err := fmt.Fprint(w, "retry: 1000\n"); err != nil {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := writeSSEPlaybackEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

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

func writePlaybackSnapshot(w http.ResponseWriter, status int, snapshot playback.PlaybackSnapshot) {
	writeJSON(w, status, publicPlaybackSnapshot(snapshot))
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, apidto.ErrorResponse{Code: code, Error: message})
}

func writeAPIError(w http.ResponseWriter, err error) {
	logInternalError("http api", err)
	writeJSON(w, statusForError(err), apidto.ErrorResponse{
		Code:  codeForError(err),
		Error: publicErrorMessageForError(err),
	})
}

func writePlaybackError(w http.ResponseWriter, err error, snapshot playback.PlaybackSnapshot) {
	logInternalError("http api", err)
	writeJSON(w, statusForError(err), apidto.ErrorResponse{
		Code:     codeForError(err),
		Error:    publicErrorMessageForError(err),
		Playback: publicPlaybackSnapshot(snapshot),
	})
}

func publicPlaybackEvent(event playback.PlaybackEvent) apidto.PlaybackEvent {
	return apidto.PlaybackEvent{
		Seq:      int64(event.Seq),
		Type:     apidto.PlaybackEventType(event.Type),
		Time:     event.Time,
		Playback: publicPlaybackSnapshot(event.Playback),
	}
}

func writeSSEPlaybackEvent(w io.Writer, event playback.PlaybackEvent) error {
	data, err := json.Marshal(publicPlaybackEvent(event))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\n", event.Seq); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event.Type); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func publicPlaybackSnapshot(snapshot playback.PlaybackSnapshot) apidto.PlaybackState {
	public := apidto.PlaybackState{
		State:           apidto.PlaybackStateState(snapshot.State),
		BookID:          snapshot.BookID,
		ProgressPercent: snapshot.ProgressPercent,
		CurrentByte:     snapshot.CurrentByte,
		Voice:           snapshot.Voice,
		ChunkSize:       snapshot.ChunkSize,
		ErrorCode:       apidto.PlaybackStateErrorCode(snapshot.ErrorCode),
	}
	if public.ErrorCode == "" {
		return public
	}
	public.Error = publicErrorMessageForCode(string(public.ErrorCode))
	return public
}

func publicBook(book book.Book) apidto.Book {
	return apidto.Book{ID: book.ID, Title: book.Title, Size: book.Size}
}

func publicBooks(books []book.Book) []apidto.Book {
	result := make([]apidto.Book, 0, len(books))
	for _, book := range books {
		result = append(result, publicBook(book))
	}
	return result
}

func publicErrorMessageForCode(code string) string {
	switch code {
	case "playback_stopping":
		return "playback is still stopping"
	case "book_modified":
		return "book file changed during playback"
	case "internal_error":
		return "internal server error"
	default:
		return ""
	}
}

func publicErrorMessageForError(err error) string {
	switch {
	case errors.Is(err, playback.ErrActive):
		return "playback is already active"
	case errors.Is(err, playback.ErrStopping):
		return "playback is still stopping"
	case errors.Is(err, playback.ErrShuttingDown):
		return "service is shutting down"
	case errors.Is(err, playback.ErrNotPlaying):
		return "playback is not playing"
	case errors.Is(err, playback.ErrNotPaused):
		return "playback is not paused"
	case errors.Is(err, book.ErrModified):
		return "book file changed after registration"
	case errors.Is(err, progress.ErrBookMismatch):
		return "progress belongs to a different book"
	case errors.Is(err, progress.ErrFormat):
		return "unsupported progress format"
	case errors.Is(err, book.ErrNotFound):
		return "book not found"
	case errors.Is(err, book.ErrNotReadable):
		return "book is not readable"
	case errors.Is(err, book.ErrNotRegular):
		return "book must be a regular file"
	case errors.Is(err, book.ErrPathRequired):
		return "path is required"
	case errors.Is(err, playback.ErrBookIDRequired):
		return "book_id is required"
	case errors.Is(err, playback.ErrCurrentByteRequired):
		return "current_byte is required"
	case errors.Is(err, progress.ErrPositionOutside):
		return "position outside book"
	case errors.Is(err, progress.ErrPositionInside):
		return "position inside UTF-8 rune"
	case errors.Is(err, chunk.ErrInvalidSize):
		return "invalid chunk_size"
	case errors.Is(err, ErrUnsupportedMedia):
		return "unsupported media type"
	case errors.Is(err, ErrInvalidJSON):
		return "invalid JSON request"
	default:
		return "internal server error"
	}
}

func logInternalError(scope string, err error) {
	if err == nil || statusForError(err) != http.StatusInternalServerError {
		return
	}
	log.Printf("%s: %v", scope, err)
}

func statusForError(err error) int {
	switch {
	case errors.Is(err, playback.ErrActive),
		errors.Is(err, playback.ErrStopping),
		errors.Is(err, book.ErrModified),
		errors.Is(err, progress.ErrBookMismatch),
		errors.Is(err, progress.ErrFormat),
		errors.Is(err, playback.ErrNotPlaying),
		errors.Is(err, playback.ErrNotPaused):
		return http.StatusConflict
	case errors.Is(err, playback.ErrShuttingDown):
		return http.StatusServiceUnavailable
	case errors.Is(err, book.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrUnsupportedMedia):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, book.ErrPathRequired),
		errors.Is(err, book.ErrNotReadable),
		errors.Is(err, book.ErrNotRegular),
		errors.Is(err, playback.ErrBookIDRequired),
		errors.Is(err, playback.ErrCurrentByteRequired),
		errors.Is(err, progress.ErrPositionOutside),
		errors.Is(err, progress.ErrPositionInside),
		errors.Is(err, chunk.ErrInvalidSize),
		errors.Is(err, ErrInvalidJSON):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func codeForError(err error) string {
	switch {
	case errors.Is(err, playback.ErrActive):
		return "playback_active"
	case errors.Is(err, playback.ErrStopping):
		return "playback_stopping"
	case errors.Is(err, playback.ErrShuttingDown):
		return "service_shutting_down"
	case errors.Is(err, playback.ErrNotPlaying):
		return "playback_not_playing"
	case errors.Is(err, playback.ErrNotPaused):
		return "playback_not_paused"
	case errors.Is(err, book.ErrModified):
		return "book_modified"
	case errors.Is(err, progress.ErrBookMismatch):
		return "progress_book_mismatch"
	case errors.Is(err, progress.ErrFormat):
		return "progress_format_unsupported"
	case errors.Is(err, book.ErrNotFound):
		return "book_not_found"
	case errors.Is(err, book.ErrNotReadable):
		return "book_not_readable"
	case errors.Is(err, book.ErrNotRegular):
		return "book_not_regular"
	case errors.Is(err, book.ErrPathRequired):
		return "path_required"
	case errors.Is(err, playback.ErrBookIDRequired):
		return "book_id_required"
	case errors.Is(err, playback.ErrCurrentByteRequired):
		return "current_byte_required"
	case errors.Is(err, progress.ErrPositionOutside):
		return "position_outside_book"
	case errors.Is(err, progress.ErrPositionInside):
		return "position_inside_utf8_rune"
	case errors.Is(err, chunk.ErrInvalidSize):
		return "invalid_chunk_size"
	case errors.Is(err, ErrUnsupportedMedia):
		return "unsupported_media_type"
	case errors.Is(err, ErrInvalidJSON):
		return "invalid_json"
	default:
		return "internal_error"
	}
}
