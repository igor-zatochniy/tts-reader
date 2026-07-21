package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
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
	ErrPlaybackActive       = errors.New("playback active")
	ErrPlaybackNotPlaying   = errors.New("playback not playing")
	ErrPlaybackNotPaused    = errors.New("playback not paused")
	ErrBookModified         = errors.New("book modified")
	ErrBookNotFound         = errors.New("book not found")
	ErrBookNotReadable      = errors.New("book not readable")
	ErrBookNotRegular       = errors.New("book must be a regular file")
	ErrPathRequired         = errors.New("path required")
	ErrBookIDRequired       = errors.New("book_id required")
	ErrCurrentByteRequired  = errors.New("current_byte required")
	ErrPositionOutsideBook  = errors.New("position outside book")
	ErrPositionInsideRune   = errors.New("position inside UTF-8 rune")
	ErrInvalidChunkSize     = errors.New("invalid chunk_size")
	ErrUnsupportedMedia     = errors.New("unsupported media type")
	ErrInvalidJSON          = errors.New("invalid JSON request")
	ErrProgressFormat       = errors.New("unsupported progress format")
	ErrProgressBookMismatch = errors.New("progress belongs to a different book")
)

type voiceProvider func() ([]string, error)

type ServeConfig struct {
	Addr       string
	TTSTimeout time.Duration
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
