//go:build windows

package book

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPathKeyUsesDirectorySemantics(t *testing.T) {
	path := `C:\Books\Book.txt`
	if got := windowsPathKey(path, true); got != path {
		t.Fatalf("case-sensitive ключ змінив шлях: %q", got)
	}
	if got, want := windowsPathKey(path, false), strings.ToLower(path); got != want {
		t.Fatalf("case-insensitive ключ = %q, очікувалося %q", got, want)
	}
}

func TestBookStoreDistinguishesFilesInCaseSensitiveDirectory(t *testing.T) {
	dir := t.TempDir()
	output, err := exec.Command("fsutil.exe", "file", "setCaseSensitiveInfo", dir, "enable").CombinedOutput()
	if err != nil {
		t.Skipf("середовище не дозволяє ввімкнути case sensitivity: %v (%s)", err, strings.TrimSpace(string(output)))
	}

	firstPath := filepath.Join(dir, "Book.txt")
	secondPath := filepath.Join(dir, "book.txt")
	if err := os.WriteFile(firstPath, []byte("Перша книга"), 0600); err != nil {
		t.Fatalf("не вдалося створити першу книгу: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte("Інша книга"), 0600); err != nil {
		t.Fatalf("не вдалося створити другу книгу: %v", err)
	}

	store := NewStoreWithProgressDir(filepath.Join(t.TempDir(), "progress"))
	first, err := store.Add(AddRequest{Path: firstPath})
	if err != nil {
		t.Fatalf("не вдалося додати першу книгу: %v", err)
	}
	second, err := store.Add(AddRequest{Path: secondPath})
	if err != nil {
		t.Fatalf("не вдалося додати другу книгу: %v", err)
	}

	if first.ID == second.ID {
		t.Fatalf("різні файли отримали спільний ID %q", first.ID)
	}
	if first.SaveFile == second.SaveFile {
		t.Fatalf("різні файли отримали спільний progress-файл %q", first.SaveFile)
	}
	if first.Path != firstPath || second.Path != secondPath {
		t.Fatalf("store змішав шляхи книг: first=%q second=%q", first.Path, second.Path)
	}
	if first.File.Fingerprint == second.File.Fingerprint {
		t.Fatal("умови regression test порушені: fingerprints збігаються")
	}
	if got := len(store.List()); got != 2 {
		t.Fatalf("очікувалися дві окремі книги, отримано %d", got)
	}
}
