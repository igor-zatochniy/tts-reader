//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

func TestRunResumesLegacyDefaultProgressWithoutChangingBackup(t *testing.T) {
	t.Setenv("LocalAppData", t.TempDir())
	path := filepath.Join(t.TempDir(), "Book.txt")
	mustWriteFile(t, path, "First. Second part of the book.")
	save := book.DefaultProgressPath(path)
	legacy := book.LegacyProgressPath(path, save)
	if legacy == "" {
		t.Fatal("legacy path не визначено")
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	mustWriteProgress(t, legacy, progressForPath(t, path, legacy, 7))
	before, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	var spoken string
	code := runWithOptions([]string{"read", "-book", path}, &stdout, &stderr, func(tts.Config) tts.SpeakFunc {
		return func(_ context.Context, stringText string) error {
			spoken = stringText
			return errors.New("test TTS failure")
		}
	}, false)
	if code != 1 || !strings.HasPrefix(spoken, "Second part") {
		t.Fatalf("CLI не відновив legacy position: exit=%d text=%q stderr=%s", code, spoken, stderr.String())
	}
	for _, file := range []string{legacy, save} {
		after, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("CLI змінив збережену позицію: %q: %s, %v", file, after, err)
		}
	}
}
