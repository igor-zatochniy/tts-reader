package book

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrModified     = errors.New("book modified")
	ErrNotFound     = errors.New("book not found")
	ErrNotReadable  = errors.New("book not readable")
	ErrNotRegular   = errors.New("book must be a regular file")
	ErrPathRequired = errors.New("path required")
)

type Book struct {
	ID        string       `json:"-"`
	Title     string       `json:"-"`
	Path      string       `json:"-"`
	SaveFile  string       `json:"-"`
	Size      int64        `json:"-"`
	File      FileIdentity `json:"-"`
	CreatedAt time.Time    `json:"-"`
}

type FileIdentity struct {
	Size        int64
	ModifiedAt  time.Time
	Fingerprint string
}

type AddRequest struct {
	Path  string `json:"path"`
	Title string `json:"title"`
}

type BookStore struct {
	mu          sync.RWMutex
	next        int64
	books       map[string]Book
	byPath      map[string]string
	progressDir string
}

func NewStore() *BookStore {
	return NewStoreWithProgressDir(defaultProgressDir())
}

// NewStoreWithProgressDir створює store з окремим каталогом progress-файлів.
func NewStoreWithProgressDir(progressDir string) *BookStore {
	if strings.TrimSpace(progressDir) == "" {
		progressDir = defaultProgressDir()
	}
	return &BookStore{
		books:       make(map[string]Book),
		byPath:      make(map[string]string),
		progressDir: progressDir,
	}
}

func (s *BookStore) Add(req AddRequest) (Book, error) {
	return s.AddContext(context.Background(), req)
}

func (s *BookStore) AddContext(ctx context.Context, req AddRequest) (Book, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Book{}, err
	}
	if strings.TrimSpace(req.Path) == "" {
		return Book{}, ErrPathRequired
	}

	absPath, pathKey, err := canonicalBookPath(req.Path)
	if err != nil {
		return Book{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	identity, err := InspectFileContext(ctx, absPath)
	if err != nil {
		return Book{}, err
	}
	if err := ctx.Err(); err != nil {
		return Book{}, err
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byPath[pathKey]; ok {
		existing := s.books[id]
		existing.Size = identity.Size
		existing.File = identity
		if strings.TrimSpace(req.Title) != "" {
			existing.Title = title
		}
		s.books[id] = existing
		return existing, nil
	}

	s.next++
	book := Book{
		ID:        fmt.Sprintf("book-%d", s.next),
		Title:     title,
		Path:      absPath,
		SaveFile:  progressPathInDir(s.progressDir, absPath),
		Size:      identity.Size,
		File:      identity,
		CreatedAt: time.Now().UTC(),
	}
	s.books[book.ID] = book
	s.byPath[pathKey] = book.ID
	return book, nil
}

func (s *BookStore) List() []Book {
	s.mu.RLock()
	defer s.mu.RUnlock()

	books := make([]Book, 0, len(s.books))
	for _, book := range s.books {
		books = append(books, book)
	}
	return books
}

func (s *BookStore) Get(id string) (Book, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	book, ok := s.books[id]
	return book, ok
}

func DefaultProgressPath(bookPath string) string {
	return progressPathInDir(defaultProgressDir(), bookPath)
}

func defaultProgressDir() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(cacheDir) == "" {
		cacheDir = os.TempDir()
	}
	return filepath.Join(cacheDir, "tts-reader", "progress")
}

func progressPathInDir(dir string, bookPath string) string {
	_, key, err := canonicalBookPath(bookPath)
	if err != nil {
		key = filepath.Clean(bookPath)
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

func canonicalBookPath(path string) (string, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	absPath = filepath.Clean(absPath)
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = filepath.Clean(resolved)
	}
	key := canonicalPathKey(absPath)
	return absPath, key, nil
}

// PathsReferToSameFile перевіряє як канонічні шляхи, так і файлову ідентичність.
func PathsReferToSameFile(firstPath string, secondPath string) (bool, error) {
	firstAbs, firstKey, err := canonicalBookPath(firstPath)
	if err != nil {
		return false, err
	}
	secondAbs, secondKey, err := canonicalBookPath(secondPath)
	if err != nil {
		return false, err
	}
	if firstKey == secondKey {
		return true, nil
	}

	firstInfo, err := os.Stat(firstAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	secondInfo, err := os.Stat(secondAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(firstInfo, secondInfo), nil
}

func InspectFile(path string) (FileIdentity, error) {
	return InspectFileContext(context.Background(), path)
}

func InspectFileContext(ctx context.Context, path string) (FileIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return FileIdentity{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	defer file.Close()
	stopInterrupt := interruptFileOnCancel(ctx, file)
	defer stopInterrupt()
	return inspectOpenFileContext(ctx, file)
}

// OpenStableRead відкриває книгу для playback і перевіряє fingerprint саме цього handle.
func OpenStableRead(path string) (*os.File, FileIdentity, error) {
	return OpenStableReadContext(context.Background(), path)
}

// OpenStableReadContext перериває обчислення fingerprint після скасування context.
func OpenStableReadContext(ctx context.Context, path string) (*os.File, FileIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, FileIdentity{}, err
	}
	file, err := openStableRead(path)
	if err != nil {
		return nil, FileIdentity{}, fmt.Errorf("%w: %w", ErrNotReadable, err)
	}

	stopInterrupt := interruptFileOnCancel(ctx, file)
	identity, err := inspectOpenFileContext(ctx, file)
	stopInterrupt()
	if err != nil {
		_ = file.Close()
		return nil, FileIdentity{}, err
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return nil, FileIdentity{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	return file, identity, nil
}

func inspectOpenFileContext(ctx context.Context, file *os.File) (FileIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return FileIdentity{}, err
	}
	info, err := file.Stat()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return FileIdentity{}, ctxErr
		}
		return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	if !info.Mode().IsRegular() {
		return FileIdentity{}, ErrNotRegular
	}

	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var bytesRead int64
	for {
		if err := ctx.Err(); err != nil {
			return FileIdentity{}, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			bytesRead += int64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return FileIdentity{}, ctxErr
			}
			return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, readErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return FileIdentity{}, err
	}
	afterRead, err := file.Stat()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return FileIdentity{}, ctxErr
		}
		return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	if bytesRead != info.Size() || afterRead.Size() != info.Size() || !afterRead.ModTime().Equal(info.ModTime()) {
		return FileIdentity{}, fmt.Errorf("%w: file changed while calculating fingerprint", ErrModified)
	}

	return FileIdentity{
		Size:        info.Size(),
		ModifiedAt:  afterRead.ModTime().UTC(),
		Fingerprint: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

// interruptFileOnCancel закриває handle, щоб розблокувати активне файлове читання.
func interruptFileOnCancel(ctx context.Context, file *os.File) func() {
	if ctx.Done() == nil {
		return func() {}
	}

	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			_ = file.Close()
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-stopped
	}
}

func SameFile(registered FileIdentity, current FileIdentity) bool {
	return registered.Size == current.Size &&
		registered.ModifiedAt.Equal(current.ModifiedAt) &&
		registered.Fingerprint == current.Fingerprint
}

func SameFileMetadata(registered FileIdentity, info os.FileInfo) bool {
	if info == nil {
		return false
	}

	return registered.Size == info.Size() &&
		registered.ModifiedAt.Equal(info.ModTime().UTC())
}
