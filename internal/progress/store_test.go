package progress

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/book"
)

func TestValidateForBookRejectsSameSizeMiddleEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.txt")
	content := make([]byte, 256<<10)
	for i := range content {
		content[i] = 'a'
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatalf("не вдалося створити книгу: %v", err)
	}

	original, err := book.InspectFile(path)
	if err != nil {
		t.Fatalf("не вдалося перевірити початкову книгу: %v", err)
	}
	saved := ProgressForBook(BookForProgress(path, filepath.Join(t.TempDir(), "progress.json"), original), 128<<10)

	file, err := os.OpenFile(path, os.O_WRONLY, 0)
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
	changedTime := original.ModifiedAt.Add(2 * time.Second)
	if err := os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatalf("не вдалося зафіксувати новий modification time: %v", err)
	}

	current, err := book.InspectFile(path)
	if err != nil {
		t.Fatalf("не вдалося перевірити змінену книгу: %v", err)
	}
	if original.Fingerprint != current.Fingerprint {
		t.Fatal("тест має змінювати лише область поза sampled fingerprint")
	}
	_, err = ValidateForBook(BookForProgress(path, "", current), saved, current.Size)
	if !errors.Is(err, ErrBookMismatch) {
		t.Fatalf("очікував ErrBookMismatch після middle edit, отримав %v", err)
	}
}

func TestLeasePreventsConcurrentAccessAndCanBeReacquired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	first, err := AcquireLease(path)
	if err != nil {
		t.Fatalf("не вдалося отримати перший lease: %v", err)
	}

	if _, err := AcquireLease(path); !errors.Is(err, ErrInUse) {
		_ = first.Close()
		t.Fatalf("очікував ErrInUse для другого lease, отримав %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("не вдалося звільнити lease: %v", err)
	}

	second, err := AcquireLease(path)
	if err != nil {
		t.Fatalf("lease не можна повторно отримати після Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("не вдалося звільнити повторний lease: %v", err)
	}
}

func TestLeaseBlocksAnotherProcess(t *testing.T) {
	if os.Getenv("TTS_READER_LEASE_HELPER") == "1" {
		lease, err := AcquireLease(os.Getenv("TTS_READER_LEASE_PATH"))
		if errors.Is(err, ErrInUse) {
			return
		}
		if err == nil {
			_ = lease.Close()
		}
		t.Fatalf("дочірній процес отримав lease: %v", err)
	}

	path := filepath.Join(t.TempDir(), "progress.json")
	lease, err := AcquireLease(path)
	if err != nil {
		t.Fatalf("не вдалося отримати lease: %v", err)
	}
	defer lease.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseBlocksAnotherProcess$")
	cmd.Env = append(os.Environ(),
		"TTS_READER_LEASE_HELPER=1",
		"TTS_READER_LEASE_PATH="+path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("дочірня перевірка lease завершилася помилкою: %v\n%s", err, output)
	}
}
