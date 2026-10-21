//go:build windows

package playback

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

func TestPlaybackStartsDistinctUnicodeWindowsBooks(t *testing.T) {
	dir := t.TempDir()
	store := book.NewStoreWithProgressDir(filepath.Join(dir, "progress"))
	var books []book.Book
	for _, name := range []string{"\u0130stanbul.txt", "istanbul.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name+" independent book"), 0600); err != nil {
			t.Fatal(err)
		}
		b, err := store.Add(book.AddRequest{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		books = append(books, b)
	}
	manager := NewManager(func(tts.Config) tts.Engine {
		return &testEngine{speakContext: func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		}}
	}, time.Second, NewEventBroker())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := manager.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	for _, b := range books {
		if snapshot, err := manager.Start(b, StartRequest{BookID: b.ID}); err != nil || snapshot.BookID != b.ID {
			t.Fatalf("окрема книга не стартувала: %+v, %v", snapshot, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		snapshot, err := manager.Stop(ctx)
		cancel()
		if err != nil || snapshot.State != Stopped || snapshot.ErrorCode != "" {
			t.Fatalf("книга не зупинилася коректно: %+v, %v", snapshot, err)
		}
	}
}
