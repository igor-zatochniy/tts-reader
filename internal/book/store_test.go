package book

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBookStoreDeduplicatesCanonicalPath(t *testing.T) {
	bookPath := writeBook(t, "Початковий текст")
	store := NewStoreWithProgressDir(filepath.Join(t.TempDir(), "progress"))

	first, err := store.Add(AddRequest{Path: bookPath})
	if err != nil {
		t.Fatalf("не вдалося додати книгу: %v", err)
	}
	second, err := store.Add(AddRequest{Path: filepath.Join(filepath.Dir(bookPath), ".", filepath.Base(bookPath)), Title: "Нова назва"})
	if err != nil {
		t.Fatalf("не вдалося повторно додати книгу: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("один файл отримав різні ID: %q і %q", first.ID, second.ID)
	}
	if second.Title != "Нова назва" {
		t.Fatalf("явно задана назва не оновилася: %#v", second)
	}
	if got := len(store.List()); got != 1 {
		t.Fatalf("очікувалася одна книга після deduplication, отримано %d", got)
	}
}

func TestBookStoreRefreshesMetadataForRepeatedPath(t *testing.T) {
	bookPath := writeBook(t, "Коротко")
	store := NewStoreWithProgressDir(filepath.Join(t.TempDir(), "progress"))
	first, err := store.Add(AddRequest{Path: bookPath})
	if err != nil {
		t.Fatalf("не вдалося додати книгу: %v", err)
	}

	if err := os.WriteFile(bookPath, []byte("Значно довший текст книги"), 0600); err != nil {
		t.Fatalf("не вдалося оновити книгу: %v", err)
	}
	second, err := store.Add(AddRequest{Path: bookPath})
	if err != nil {
		t.Fatalf("не вдалося оновити реєстрацію книги: %v", err)
	}

	if first.ID != second.ID || first.Size == second.Size || second.Size != int64(len("Значно довший текст книги")) {
		t.Fatalf("метадані повторно зареєстрованої книги не оновилися: first=%#v second=%#v", first, second)
	}
}

func TestBookStoreKeepsProgressOutsideBookDirectory(t *testing.T) {
	bookPath := writeBook(t, "Книга")
	progressDir := filepath.Join(t.TempDir(), "progress")
	store := NewStoreWithProgressDir(progressDir)

	registered, err := store.Add(AddRequest{Path: bookPath})
	if err != nil {
		t.Fatalf("не вдалося додати книгу: %v", err)
	}

	wantPrefix := filepath.Clean(progressDir) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(registered.SaveFile), wantPrefix) {
		t.Fatalf("progress має бути в окремому каталозі: %q", registered.SaveFile)
	}
	if filepath.Ext(registered.SaveFile) != ".json" {
		t.Fatalf("progress має бути JSON-файлом: %q", registered.SaveFile)
	}
}

func TestDefaultProgressPathUsesUserCache(t *testing.T) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("user cache недоступний: %v", err)
	}
	path := DefaultProgressPath(filepath.Join(t.TempDir(), "book.txt"))
	wantPrefix := filepath.Join(cacheDir, "tts-reader", "progress") + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(wantPrefix)) {
		t.Fatalf("default progress має бути в user cache: %q", path)
	}
}

func writeBook(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.txt")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("не вдалося записати книгу: %v", err)
	}
	return path
}
