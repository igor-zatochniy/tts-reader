//go:build windows

package progress

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/igor-zatochniy/tts-reader/internal/book"
)

func legacyFixture(t *testing.T) (book.Book, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Book.txt")
	if err := os.WriteFile(path, []byte("First. Українська книга."), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := book.NewStoreWithProgressDir(filepath.Join(dir, "progress")).Add(book.AddRequest{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.ToLower(canonical)))
	legacy := filepath.Join(filepath.Dir(b.SaveFile), hex.EncodeToString(sum[:])+".json")
	if got := book.LegacyProgressPath(b.Path, b.SaveFile); got != legacy || got == b.SaveFile {
		t.Fatalf("неправильний legacy path: %q, очікував %q", got, legacy)
	}
	lease, err := AcquireLease(b.SaveFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	return b, legacy
}

func writeLegacy(t *testing.T, path string, saved Progress) []byte {
	t.Helper()
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileReplace(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLegacyProgressMigrationPreservesPositionAndOriginal(t *testing.T) {
	b, legacy := legacyFixture(t)
	before := writeLegacy(t, legacy, ProgressForBook(b, 7))
	pos, err := (JSONProgressStore{}).Load(b, b.Size)
	if err != nil || pos != 7 {
		t.Fatalf("legacy progress не відновлено: %d, %v", pos, err)
	}
	for _, path := range []string{legacy, b.SaveFile} {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("міграція змінила збереження %q: %s, %v", path, after, err)
		}
	}
	if err := (JSONProgressStore{}).Save(b, 9); err != nil {
		t.Fatal(err)
	}
	pos, err = (JSONProgressStore{}).Load(b, b.Size)
	if err != nil || pos != 9 {
		t.Fatalf("legacy-файл відновив застарілу позицію: %d, %v", pos, err)
	}
}

func TestLegacyProgressDoesNotReplaceCurrentFile(t *testing.T) {
	for _, current := range []string{"valid", "corrupt"} {
		t.Run(current, func(t *testing.T) {
			b, legacy := legacyFixture(t)
			writeLegacy(t, legacy, ProgressForBook(b, 7))
			if current == "valid" {
				if err := (JSONProgressStore{}).Save(b, 9); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(b.SaveFile, []byte("{broken"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(b.SaveFile)
			if err != nil {
				t.Fatal(err)
			}
			pos, err := (JSONProgressStore{}).Load(b, b.Size)
			if current == "valid" && (err != nil || pos != 9) || current == "corrupt" && err == nil {
				t.Fatalf("неправильний результат Load: %d, %v", pos, err)
			}
			after, err := os.ReadFile(b.SaveFile)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("наявний progress було замінено під час Load")
			}
		})
	}
}

func TestLegacyProgressRejectsInvalidDataWithoutMigration(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Progress)
		want error
	}{
		{"format", func(p *Progress) { p.Version-- }, ErrFormat},
		{"outside", func(p *Progress) { p.LastPosition = p.BookSize + 1 }, ErrPositionOutside},
		{"utf8", func(p *Progress) { p.LastPosition = 8 }, ErrPositionInside},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, legacy := legacyFixture(t)
			saved := ProgressForBook(b, 7)
			test.edit(&saved)
			before := writeLegacy(t, legacy, saved)
			if _, err := (JSONProgressStore{}).Load(b, b.Size); !errors.Is(err, test.want) {
				t.Fatalf("очікував %v, отримав %v", test.want, err)
			}
			if _, err := os.Stat(b.SaveFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("невалідний progress було перенесено: %v", err)
			}
			after, err := os.ReadFile(legacy)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("невалідний legacy-файл було змінено")
			}
		})
	}
}

func TestLegacyProgressMigrationHonorsLease(t *testing.T) {
	b, legacy := legacyFixture(t)
	writeLegacy(t, legacy, ProgressForBook(b, 7))
	lease, err := AcquireLease(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := (JSONProgressStore{}).Load(b, b.Size); !errors.Is(err, ErrInUse) {
		t.Fatalf("міграція проігнорувала legacy lease: %v", err)
	}
	if _, err := os.Stat(b.SaveFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("заблокований legacy progress було перенесено: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if pos, err := (JSONProgressStore{}).Load(b, b.Size); err != nil || pos != 7 {
		t.Fatalf("міграція не відновилася після звільнення lease: %d, %v", pos, err)
	}
}

func TestLegacyProgressRejectsMalformedAndOversizedJSON(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{"malformed", "{broken"},
		{"oversized", strings.Repeat(" ", MaxProgressFileSize+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, legacy := legacyFixture(t)
			if err := os.WriteFile(legacy, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := (JSONProgressStore{}).Load(b, b.Size); !errors.Is(err, ErrFormat) {
				t.Fatalf("невалідний legacy JSON прийнято: %v", err)
			}
			if _, err := os.Stat(b.SaveFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("невалідний legacy JSON перенесено: %v", err)
			}
		})
	}
}

func TestCustomProgressPathDoesNotImportLegacyDefault(t *testing.T) {
	b, legacy := legacyFixture(t)
	before := writeLegacy(t, legacy, ProgressForBook(b, 7))
	b.SaveFile = filepath.Join(filepath.Dir(b.SaveFile), "custom.json")
	if got := book.LegacyProgressPath(b.Path, b.SaveFile); got != "" {
		t.Fatalf("явний save path має legacy fallback: %q", got)
	}
	if pos, err := (JSONProgressStore{}).Load(b, b.Size); err != nil || pos != 0 {
		t.Fatalf("явний save path успадкував default progress: %d, %v", pos, err)
	}
	after, err := os.ReadFile(legacy)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("явний save path змінив default progress")
	}
}

func TestLegacyProgressCollisionDoesNotImportAnotherBook(t *testing.T) {
	dir := t.TempDir()
	store := book.NewStoreWithProgressDir(filepath.Join(dir, "progress"))
	var books []book.Book
	for _, name := range []string{"\u0130stanbul.txt", "istanbul.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name+" independent content"), 0600); err != nil {
			t.Fatal(err)
		}
		b, err := store.Add(book.AddRequest{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		lease, err := AcquireLease(b.SaveFile)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lease.Close() })
		books = append(books, b)
	}
	legacy := book.LegacyProgressPath(books[0].Path, books[0].SaveFile)
	if legacy == "" || legacy != book.LegacyProgressPath(books[1].Path, books[1].SaveFile) {
		t.Fatal("умови тесту не відтворюють стару колізію")
	}
	before := writeLegacy(t, legacy, ProgressForBook(books[0], 2))
	if pos, err := (JSONProgressStore{}).Load(books[1], books[1].Size); err != nil || pos != 0 {
		t.Fatalf("інша книга успадкувала чужий progress: %d, %v", pos, err)
	}
	if _, err := os.Stat(books[1].SaveFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("чужий progress було скопійовано: %v", err)
	}
	if pos, err := (JSONProgressStore{}).Load(books[0], books[0].Size); err != nil || pos != 2 {
		t.Fatalf("власний progress не відновлено: %d, %v", pos, err)
	}
	after, err := os.ReadFile(legacy)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("колізія змінила legacy progress")
	}
}

func TestProgressLeasesDistinguishUnicodeNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"\u0130.json", "i.json"} {
		lease, err := AcquireLease(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
	}
	lease, err := AcquireLease(filepath.Join(dir, "I.JSON"))
	if lease != nil {
		_ = lease.Close()
	}
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("ASCII-аліас обійшов lease: %v", err)
	}
}
