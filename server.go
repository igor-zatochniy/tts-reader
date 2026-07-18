package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
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
	"strings"
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
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	server := newLocalHTTPServer(cfg.Addr, api.Routes(), serveCtx)

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
		cancelServe()

		playbackCtx, cancelPlayback := context.WithTimeout(context.Background(), 10*time.Second)
		_, playbackErr := api.playback.Stop(playbackCtx)
		cancelPlayback()

		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancelShutdown()

		if err := errors.Join(playbackErr, shutdownErr); err != nil {
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

func newLocalHTTPServer(addr string, handler http.Handler, baseCtx context.Context) *http.Server {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if baseCtx != nil {
		server.BaseContext = func(net.Listener) context.Context {
			return baseCtx
		}
	}
	return server
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
