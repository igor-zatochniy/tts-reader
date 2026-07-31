package book

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	if strings.TrimSpace(req.Path) == "" {
		return Book{}, ErrPathRequired
	}

	absPath, pathKey, err := canonicalBookPath(req.Path)
	if err != nil {
		return Book{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	identity, err := InspectFile(absPath)
	if err != nil {
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
	key := absPath
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return absPath, key, nil
}

func InspectFile(path string) (FileIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	if !info.Mode().IsRegular() {
		return FileIdentity{}, ErrNotRegular
	}

	hash := sha256.New()
	fmt.Fprintf(hash, "size:%d\n", info.Size())

	const sampleSize int64 = 64 << 10
	headSize := minInt64(info.Size(), sampleSize)
	if headSize > 0 {
		if _, err := io.CopyN(hash, file, headSize); err != nil {
			return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
		}
	}
	if info.Size() > sampleSize {
		if _, err := file.Seek(info.Size()-sampleSize, io.SeekStart); err != nil {
			return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
		}
		if _, err := io.CopyN(hash, file, sampleSize); err != nil {
			return FileIdentity{}, fmt.Errorf("%w: %v", ErrNotReadable, err)
		}
	}

	return FileIdentity{
		Size:        info.Size(),
		ModifiedAt:  info.ModTime().UTC(),
		Fingerprint: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func SameFile(registered FileIdentity, current FileIdentity) bool {
	return registered.Size == current.Size &&
		registered.ModifiedAt.Equal(current.ModifiedAt) &&
		registered.Fingerprint == current.Fingerprint
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
