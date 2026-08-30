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
	_, identity, err := inspectOwnedFileContext(
		ctx,
		func() (*os.File, error) {
			file, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrNotReadable, err)
			}
			return file, nil
		},
		inspectOpenFileContext,
		false,
	)
	return identity, err
}

// OpenStableRead відкриває книгу для playback і перевіряє fingerprint саме цього handle.
func OpenStableRead(path string) (*os.File, FileIdentity, error) {
	return OpenStableReadContext(context.Background(), path)
}

// OpenStableReadContext повертається після скасування context, не закриваючи handle конкурентно з читанням.
func OpenStableReadContext(ctx context.Context, path string) (*os.File, FileIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, FileIdentity{}, err
	}
	return inspectOwnedFileContext(
		ctx,
		func() (*os.File, error) {
			file, err := openStableRead(path)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrNotReadable, err)
			}
			return file, nil
		},
		inspectOpenFileContext,
		true,
	)
}

type fileInspectionResult struct {
	file     *os.File
	identity FileIdentity
	err      error
}

// inspectOwnedFileContext залишає відкриття, читання та закриття handle одній goroutine.
func inspectOwnedFileContext(
	ctx context.Context,
	openFile func() (*os.File, error),
	inspectFile func(context.Context, *os.File) (FileIdentity, error),
	keepOpen bool,
) (*os.File, FileIdentity, error) {
	results := make(chan fileInspectionResult)
	go func() {
		result := fileInspectionResult{}
		result.file, result.err = openFile()
		if result.err == nil {
			if err := ctx.Err(); err != nil {
				result.err = err
			} else {
				result.identity, result.err = inspectFile(ctx, result.file)
			}
		}
		if result.err == nil && keepOpen {
			if _, err := result.file.Seek(0, io.SeekStart); err != nil {
				result.err = fmt.Errorf("%w: %v", ErrNotReadable, err)
			}
		}
		if result.err == nil {
			result.err = ctx.Err()
		}
		if result.err != nil || !keepOpen {
			if result.file != nil {
				_ = result.file.Close()
				result.file = nil
			}
		}

		select {
		case results <- result:
		case <-ctx.Done():
			if result.file != nil {
				_ = result.file.Close()
			}
		}
	}()

	select {
	case result := <-results:
		if err := ctx.Err(); err != nil {
			if result.file != nil {
				_ = result.file.Close()
			}
			return nil, FileIdentity{}, err
		}
		return result.file, result.identity, result.err
	case <-ctx.Done():
		return nil, FileIdentity{}, ctx.Err()
	}
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
