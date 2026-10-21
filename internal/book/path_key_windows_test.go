//go:build windows

package book

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPathKeyPreservesNormalizedUnicodeNames(t *testing.T) {
	for _, path := range []string{`C:\Books\Book.txt`, "C:\\Books\\\u0130stanbul.txt", `C:\Books\istanbul.txt`} {
		if got := canonicalPathKey(path); got != path {
			t.Fatalf("ключ змінив нормалізований шлях: %q -> %q", path, got)
		}
	}
}

func TestBookStoreDistinguishesUnicodeNamesOnWindows(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "\u0130stanbul.txt"), filepath.Join(dir, "istanbul.txt")}
	store := NewStoreWithProgressDir(filepath.Join(dir, "progress"))
	var registered []Book
	for i, path := range paths {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString([]string{"First book.", "Second independent book."}[i])
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			t.Fatal(err)
		}
		b, err := store.Add(AddRequest{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		registered = append(registered, b)
	}
	if registered[0].ID == registered[1].ID || registered[0].SaveFile == registered[1].SaveFile || len(store.List()) != 2 {
		t.Fatalf("різні Unicode-файли об'єднано: %+v", registered)
	}
	for _, b := range registered {
		current, err := InspectFile(b.Path)
		if err != nil || !SameFile(b.File, current) {
			t.Fatalf("шлях книги не відповідає identity: %+v, %v", b, err)
		}
	}
	alias, err := store.Add(AddRequest{Path: filepath.Join(dir, "ISTANBUL.TXT")})
	if err != nil || alias.ID != registered[1].ID || alias.SaveFile != registered[1].SaveFile {
		t.Fatalf("ASCII-аліас того самого файлу не розпізнано: %+v, %v", alias, err)
	}
}

func TestBookStoreRejectsConflictingPathIndex(t *testing.T) {
	dir := t.TempDir()
	firstPath, secondPath := filepath.Join(dir, "first.txt"), filepath.Join(dir, "second.txt")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := NewStoreWithProgressDir(filepath.Join(dir, "progress"))
	first, err := store.Add(AddRequest{Path: firstPath})
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := canonicalBookPath(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	store.byPath[key] = first.ID
	if _, err := store.Add(AddRequest{Path: secondPath}); !errors.Is(err, ErrModified) {
		t.Fatalf("колізія індексу не відхилена: %v", err)
	}
	if got, _ := store.Get(first.ID); got != first {
		t.Fatal("колізія індексу змінила початкову книгу")
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
	if filepath.Base(first.Path) != "Book.txt" || filepath.Base(second.Path) != "book.txt" {
		t.Fatalf("store втратив регістр шляхів: first=%q second=%q", first.Path, second.Path)
	}
	if first.File.Fingerprint == second.File.Fingerprint {
		t.Fatal("умови regression test порушені: fingerprints збігаються")
	}
	for _, registered := range []Book{first, second} {
		current, err := InspectFile(registered.Path)
		if err != nil {
			t.Fatalf("не вдалося перевірити шлях %q: %v", registered.Path, err)
		}
		if !SameFile(registered.File, current) {
			t.Fatalf("шлях не відповідає identity книги: %#v current=%#v", registered, current)
		}
	}
	if got := len(store.List()); got != 2 {
		t.Fatalf("очікувалися дві окремі книги, отримано %d", got)
	}
}
