package progress

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/chunk"
)

const (
	// Позиція прогресу зберігається в байтах, бо рядки Go індексуються байтовими зміщеннями.
	Unit    = "bytes (UTF-8)"
	Version = 2
)

var (
	ErrFormat          = errors.New("unsupported progress format")
	ErrBookMismatch    = errors.New("progress belongs to a different book")
	ErrPositionOutside = errors.New("position outside book")
	ErrPositionInside  = errors.New("position inside UTF-8 rune")
)

type Progress struct {
	Version         int    `json:"version"`
	LastPosition    int64  `json:"last_position"`
	PositionUnit    string `json:"position_unit"`
	BookSize        int64  `json:"book_size"`
	BookFingerprint string `json:"book_fingerprint"`
}

type ProgressStore interface {
	Load(book book.Book, currentSize int64) (int64, error)
	Save(book book.Book, position int64) error
	Reset(book book.Book) error
}

type JSONProgressStore struct{}

func (JSONProgressStore) Load(book book.Book, currentSize int64) (int64, error) {
	data, err := os.ReadFile(book.SaveFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	var progress Progress
	if err := json.Unmarshal(data, &progress); err != nil {
		return 0, fmt.Errorf("invalid progress JSON: %w", err)
	}
	pos, err := ValidateForBook(book, progress, currentSize)
	if err != nil {
		return 0, err
	}
	ok, err := chunk.IsFileUTF8Boundary(book.Path, pos, currentSize)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrPositionInside
	}
	if pos == currentSize {
		return 0, nil
	}
	return pos, nil
}

func (JSONProgressStore) Save(book book.Book, pos int64) error {
	data, err := json.Marshal(ProgressForBook(book, pos))
	if err != nil {
		return fmt.Errorf("marshal progress: %w", err)
	}
	if err := WriteFileReplace(book.SaveFile, data, 0600); err != nil {
		return fmt.Errorf("replace progress file: %w", err)
	}
	return nil
}

func (s JSONProgressStore) Reset(book book.Book) error {
	return s.Save(book, 0)
}

func SaveBook(book book.Book, pos int64) error {
	return JSONProgressStore{}.Save(book, pos)
}

func BookForProgress(bookPath, saveFile string, identity book.FileIdentity) book.Book {
	return book.Book{
		Path:     bookPath,
		SaveFile: saveFile,
		Size:     identity.Size,
		File:     identity,
	}
}

func ProgressForBook(book book.Book, pos int64) Progress {
	return Progress{
		Version:         Version,
		LastPosition:    pos,
		PositionUnit:    Unit,
		BookSize:        book.File.Size,
		BookFingerprint: book.File.Fingerprint,
	}
}

func ValidateForBook(book book.Book, progress Progress, currentSize int64) (int64, error) {
	if progress.Version != Version {
		return 0, fmt.Errorf("%w: version %d", ErrFormat, progress.Version)
	}
	if progress.PositionUnit != Unit {
		return 0, fmt.Errorf("%w: position unit %q", ErrFormat, progress.PositionUnit)
	}
	if progress.BookSize != currentSize || progress.BookFingerprint != book.File.Fingerprint {
		return 0, ErrBookMismatch
	}
	if progress.LastPosition < 0 || progress.LastPosition > currentSize {
		return 0, ErrPositionOutside
	}
	return progress.LastPosition, nil
}
