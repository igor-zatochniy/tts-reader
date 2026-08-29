package book

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectFileContextHonorsCancellation(t *testing.T) {
	bookPath := writeBook(t, strings.Repeat("Текст книги. ", 128))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := InspectFileContext(ctx, bookPath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("очікувалася context.Canceled, отримано %v", err)
	}
}

func TestOpenStableReadContextHonorsCancellation(t *testing.T) {
	bookPath := writeBook(t, strings.Repeat("Текст книги. ", 128))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	file, _, err := OpenStableReadContext(ctx, bookPath)
	if file != nil {
		_ = file.Close()
		t.Fatal("скасована операція не повинна повертати відкритий файл")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("очікувалася context.Canceled, отримано %v", err)
	}
}

func TestBookStoreAddContextHonorsCancellation(t *testing.T) {
	bookPath := writeBook(t, strings.Repeat("Текст книги. ", 128))
	store := NewStoreWithProgressDir(filepath.Join(t.TempDir(), "progress"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.AddContext(ctx, AddRequest{Path: bookPath})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("очікувалася context.Canceled, отримано %v", err)
	}
	if got := len(store.List()); got != 0 {
		t.Fatalf("скасована реєстрація змінила store: %d", got)
	}
}

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

func TestInspectFileDetectsSameSizeMiddleEditWithPreservedMtime(t *testing.T) {
	bookPath := writeBook(t, strings.Repeat("a", 256<<10))
	original, err := InspectFile(bookPath)
	if err != nil {
		t.Fatalf("не вдалося перевірити початкову книгу: %v", err)
	}

	file, err := os.OpenFile(bookPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("не вдалося відкрити книгу для зміни: %v", err)
	}
	if _, err := file.WriteAt([]byte("changed-middle"), 128<<10); err != nil {
		_ = file.Close()
		t.Fatalf("не вдалося змінити середину книги: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("не вдалося закрити змінену книгу: %v", err)
	}
	if err := os.Chtimes(bookPath, original.ModifiedAt, original.ModifiedAt); err != nil {
		t.Fatalf("не вдалося відновити modification time: %v", err)
	}

	current, err := InspectFile(bookPath)
	if err != nil {
		t.Fatalf("не вдалося перевірити змінену книгу: %v", err)
	}
	if !current.ModifiedAt.Equal(original.ModifiedAt) || current.Size != original.Size {
		t.Fatalf("умови regression test порушені: original=%#v current=%#v", original, current)
	}
	if current.Fingerprint == original.Fingerprint || SameFile(original, current) {
		t.Fatal("full fingerprint не виявив same-size middle edit зі збереженим mtime")
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
