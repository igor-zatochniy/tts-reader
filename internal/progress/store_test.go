package progress

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/igor-zatochniy/tts-reader/internal/book"
)

func TestJSONProgressStoreRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	bookPath := filepath.Join(dir, "book.txt")
	savePath := filepath.Join(dir, "progress.json")
	if err := os.WriteFile(bookPath, []byte("Книга"), 0600); err != nil {
		t.Fatalf("не вдалося створити книгу: %v", err)
	}
	if err := os.WriteFile(savePath, nil, 0600); err != nil {
		t.Fatalf("не вдалося створити progress-файл: %v", err)
	}
	if err := os.Truncate(savePath, MaxProgressFileSize+1); err != nil {
		t.Fatalf("не вдалося збільшити progress-файл: %v", err)
	}

	identity, err := book.InspectFile(bookPath)
	if err != nil {
		t.Fatalf("не вдалося перевірити книгу: %v", err)
	}
	registered := BookForProgress(bookPath, savePath, identity)
	if _, err := (JSONProgressStore{}).Load(registered, identity.Size); !errors.Is(err, ErrFormat) {
		t.Fatalf("очікував ErrFormat для завеликого progress-файлу, отримав %v", err)
	}
}

func TestValidateForBookRejectsSameSizeMiddleEditWithPreservedMtime(t *testing.T) {
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
	if err := os.Chtimes(path, original.ModifiedAt, original.ModifiedAt); err != nil {
		t.Fatalf("не вдалося відновити modification time: %v", err)
	}

	current, err := book.InspectFile(path)
	if err != nil {
		t.Fatalf("не вдалося перевірити змінену книгу: %v", err)
	}
	if !current.ModifiedAt.Equal(original.ModifiedAt) || current.Size != original.Size {
		t.Fatalf("умови regression test порушені: original=%#v current=%#v", original, current)
	}
	if original.Fingerprint == current.Fingerprint {
		t.Fatal("full fingerprint має виявляти зміну середини книги")
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

func TestLeaseCloseFailureReleasesProcessMarker(t *testing.T) {
	closeErr := errors.New("close failed")
	platform := &failingPlatformLease{err: closeErr}
	key := filepath.Join(t.TempDir(), "progress.json.lock")

	activeLeases.Lock()
	activeLeases.paths[key] = struct{}{}
	activeLeases.Unlock()
	t.Cleanup(func() {
		activeLeases.Lock()
		delete(activeLeases.paths, key)
		activeLeases.Unlock()
	})

	lease := &Lease{key: key, platform: platform}
	if err := lease.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close має повернути platform error, отримано %v", err)
	}

	activeLeases.Lock()
	_, exists := activeLeases.paths[key]
	activeLeases.Unlock()
	if exists {
		t.Fatal("невдалий platform Close залишив in-process lease marker")
	}

	if err := lease.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("повторний Close має повернути початкову помилку, отримано %v", err)
	}
	if calls := platform.calls.Load(); calls != 1 {
		t.Fatalf("platform Close викликано %d разів, очікувався один виклик", calls)
	}
}

type failingPlatformLease struct {
	err   error
	calls atomic.Int32
}

func (l *failingPlatformLease) Close() error {
	l.calls.Add(1)
	return l.err
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
