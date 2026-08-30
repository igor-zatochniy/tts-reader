//go:build windows

package book

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestOpenStableReadRejectsExistingWriter(t *testing.T) {
	bookPath := writeBook(t, "Книга з відкритим writer handle.")
	writer, err := os.OpenFile(bookPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("не вдалося відкрити writer handle: %v", err)
	}

	stable, _, stableErr := OpenStableRead(bookPath)
	if stable != nil {
		_ = stable.Close()
	}
	if stableErr == nil {
		_ = writer.Close()
		t.Fatal("stable read не має відкриватися, поки активний writer handle")
	}
	if !errors.Is(stableErr, syscall.Errno(32)) {
		_ = writer.Close()
		t.Fatalf("очікувалася Windows sharing violation, отримано %v", stableErr)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("не вдалося закрити writer handle: %v", err)
	}

	stable, identity, err := OpenStableRead(bookPath)
	if err != nil {
		t.Fatalf("stable read не відновився після закриття writer: %v", err)
	}
	defer stable.Close()
	if identity.Size == 0 || identity.Fingerprint == "" {
		t.Fatalf("stable handle не отримав повну identity: %#v", identity)
	}
}

func TestCancelledOwnedInspectionReleasesStableHandle(t *testing.T) {
	bookPath := writeBook(t, "Книга для перевірки скасування.")
	ctx, cancel := context.WithCancel(context.Background())
	inspectionStarted := make(chan struct{})
	releaseInspection := make(chan struct{})

	result := make(chan error, 1)
	go func() {
		file, _, err := inspectOwnedFileContext(
			ctx,
			func() (*os.File, error) {
				return openStableRead(bookPath)
			},
			func(_ context.Context, _ *os.File) (FileIdentity, error) {
				close(inspectionStarted)
				<-releaseInspection
				return FileIdentity{}, nil
			},
			true,
		)
		if file != nil {
			_ = file.Close()
		}
		result <- err
	}()

	select {
	case <-inspectionStarted:
	case <-time.After(time.Second):
		t.Fatal("worker не розпочав перевірку stable handle")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("очікувалася context.Canceled, отримано %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("скасована перевірка чекає заблокований worker")
	}

	close(releaseInspection)
	deadline := time.Now().Add(2 * time.Second)
	for {
		writer, err := os.OpenFile(bookPath, os.O_WRONLY, 0)
		if err == nil {
			if closeErr := writer.Close(); closeErr != nil {
				t.Fatalf("не вдалося закрити writer: %v", closeErr)
			}
			break
		}
		if !errors.Is(err, syscall.Errno(32)) {
			t.Fatalf("неочікувана помилка відкриття writer: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("worker не звільнив stable handle після скасування")
		}
		time.Sleep(time.Millisecond)
	}
}
