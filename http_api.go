package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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

func requireJSONContentType(r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return ErrUnsupportedMedia
	}
	return nil
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
