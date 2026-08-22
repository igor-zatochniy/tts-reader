//go:build windows

package book

import (
	"errors"
	"os"
	"syscall"
	"testing"
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
