//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/playback"
	"github.com/igor-zatochniy/tts-reader/internal/progress"
	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

func TestWindowsSAPISpeakSmoke(t *testing.T) {
	if os.Getenv("RUN_WINDOWS_SAPI_SMOKE") != "1" {
		t.Skip("встановіть RUN_WINDOWS_SAPI_SMOKE=1; тест відтворює звук у Windows Desktop-сесії")
	}
	t.Run("Unicode", func(t *testing.T) {
		if err := speakWindows(context.Background(), "Перевірка звуку. Unicode text: café, 😀.", "", 30*time.Second); err != nil {
			t.Fatalf("SAPI не озвучив Unicode: %v", err)
		}
	})
	t.Run("MissingVoice", func(t *testing.T) {
		err := speakWindows(context.Background(), "Voice test.", "tts-reader-missing-voice-6b615f90", 15*time.Second)
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SAPI не відхилив невідомий голос: %v", err)
		}
	})
	t.Run("Timeout", func(t *testing.T) {
		started := time.Now()
		err := speakWindows(context.Background(), strings.Repeat("This is a long speech test. ", 300), "", 2*time.Second)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 10*time.Second {
			t.Fatalf("SAPI timeout не завершив процес вчасно: %v", err)
		}
	})
	t.Run("StopPreservesDurableProgress", testWindowsSAPIStopPreservesProgress)
}

func testWindowsSAPIStopPreservesProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "book.txt")
	text := "First chunk." + strings.Repeat(" ", 68) + strings.Repeat("Keep reading this long audiobook. ", 100)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := book.NewStoreWithProgressDir(filepath.Join(dir, "progress")).Add(book.AddRequest{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	ready := &sapiReadyWriter{ready: make(chan struct{})}
	processExited := make(chan bool, 1)
	factory := tts.NewFunctionEngineFactory(func(cfg tts.Config) tts.SpeakFunc {
		calls := 0
		return func(ctx context.Context, text string) error {
			calls++
			if calls == 1 {
				return newSpeaker(cfg)(ctx, text)
			}
			ctx, cancel := context.WithTimeout(ctx, cfg.TTSTimeout)
			defer cancel()
			cmd := newSpeakWindowsCommand(ctx, text, cfg.Voice)
			// Маркер у тесті підтверджує ініціалізацію SAPI перед скасуванням реального Speak.
			i := len(cmd.Args) - 1
			cmd.Args[i] = strings.Replace(cmd.Args[i], "$speak.Speak($rawText)",
				"[Console]::Out.WriteLine('SAPI_READY'); [Console]::Out.Flush(); $speak.Speak($rawText)", 1)
			var stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = ready, &stderr
			err := cmd.Run()
			select {
			case processExited <- cmd.ProcessState != nil && cmd.ProcessState.Exited():
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return fmt.Errorf("SAPI smoke: %w: %s", err, stderr.String())
			}
			return nil
		}
	}, listVoices)
	manager := playback.NewManager(factory, 30*time.Second, playback.NewEventBroker())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := manager.Stop(ctx); err != nil {
			t.Errorf("не вдалося завершити smoke session: %v", err)
		}
	})
	chunkSize := 80
	if _, err := manager.Start(b, playback.StartRequest{BookID: b.ID, ChunkSize: &chunkSize}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready.ready:
	case <-time.After(25 * time.Second):
		t.Fatalf("другий SAPI chunk не стартував: %+v", manager.Snapshot())
	}
	durable := manager.Snapshot().CurrentByte
	if durable <= 0 || durable >= b.Size {
		t.Fatalf("перший chunk не став durable: %d", durable)
	}
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := manager.Stop(ctx)
	if err != nil || snapshot.State != playback.Stopped || snapshot.ErrorCode != "" || snapshot.CurrentByte != durable {
		t.Fatalf("Stop не зберіг стан: %+v, %v", snapshot, err)
	}
	select {
	case exited := <-processExited:
		if !exited {
			t.Fatal("PowerShell не завершився після Stop")
		}
	default:
		t.Fatal("Stop повернувся до завершення PowerShell")
	}
	position, err := (progress.JSONProgressStore{}).Load(b, b.Size)
	if err != nil || position != durable {
		t.Fatalf("durable progress втрачено: %d, %v", position, err)
	}
}

type sapiReadyWriter struct {
	ready chan struct{}
	once  sync.Once
	text  bytes.Buffer
}

func (w *sapiReadyWriter) Write(p []byte) (int, error) {
	n, err := w.text.Write(p)
	if strings.Contains(w.text.String(), "SAPI_READY") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}
