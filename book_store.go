package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Book struct {
	ID        string           `json:"-"`
	Title     string           `json:"-"`
	Path      string           `json:"-"`
	SaveFile  string           `json:"-"`
	Size      int64            `json:"-"`
	File      BookFileIdentity `json:"-"`
	CreatedAt time.Time        `json:"-"`
}

type BookFileIdentity struct {
	Size        int64
	ModifiedAt  time.Time
	Fingerprint string
}

type PublicBook struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Size  int64  `json:"size"`
}

type BookStore struct {
	mu    sync.RWMutex
	next  int64
	books map[string]Book
}

func NewBookStore() *BookStore {
	return &BookStore{books: make(map[string]Book)}
}

func (s *BookStore) Add(req AddBookRequest) (Book, error) {
	if strings.TrimSpace(req.Path) == "" {
		return Book{}, ErrPathRequired
	}

	absPath, err := filepath.Abs(req.Path)
	if err != nil {
		return Book{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
	}
	identity, err := inspectBookFile(absPath)
	if err != nil {
		return Book{}, err
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	book := Book{
		ID:        fmt.Sprintf("book-%d", s.next),
		Title:     title,
		Path:      absPath,
		SaveFile:  defaultProgressPath(absPath),
		Size:      identity.Size,
		File:      identity,
		CreatedAt: time.Now().UTC(),
	}
	s.books[book.ID] = book
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

func publicBook(book Book) PublicBook {
	return PublicBook{ID: book.ID, Title: book.Title, Size: book.Size}
}

func publicBooks(books []Book) []PublicBook {
	result := make([]PublicBook, 0, len(books))
	for _, book := range books {
		result = append(result, publicBook(book))
	}
	return result
}

func defaultProgressPath(bookPath string) string {
	ext := filepath.Ext(bookPath)
	if ext == "" {
		return bookPath + ".progress.json"
	}
	return strings.TrimSuffix(bookPath, ext) + ".progress.json"
}

func inspectBookFile(path string) (BookFileIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
	}
	if !info.Mode().IsRegular() {
		return BookFileIdentity{}, ErrBookNotRegular
	}

	hash := sha256.New()
	fmt.Fprintf(hash, "size:%d\n", info.Size())

	const sampleSize int64 = 64 << 10
	headSize := minInt64(info.Size(), sampleSize)
	if headSize > 0 {
		if _, err := io.CopyN(hash, file, headSize); err != nil {
			return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
		}
	}
	if info.Size() > sampleSize {
		if _, err := file.Seek(info.Size()-sampleSize, io.SeekStart); err != nil {
			return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
		}
		if _, err := io.CopyN(hash, file, sampleSize); err != nil {
			return BookFileIdentity{}, fmt.Errorf("%w: %v", ErrBookNotReadable, err)
		}
	}

	return BookFileIdentity{
		Size:        info.Size(),
		ModifiedAt:  info.ModTime().UTC(),
		Fingerprint: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func sameBookFile(registered BookFileIdentity, current BookFileIdentity) bool {
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
