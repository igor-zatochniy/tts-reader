package progress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/chunk"
)

const (
	// Позиція прогресу зберігається в байтах, бо рядки Go індексуються байтовими зміщеннями.
	Unit                = "bytes (UTF-8)"
	Version             = 4
	MaxProgressFileSize = 64 << 10
)

var (
	ErrFormat          = errors.New("unsupported progress format")
	ErrBookMismatch    = errors.New("progress belongs to a different book")
	ErrPositionOutside = errors.New("position outside book")
	ErrPositionInside  = errors.New("position inside UTF-8 rune")
)

type Progress struct {
	Version                int    `json:"version"`
	LastPosition           int64  `json:"last_position"`
	PositionUnit           string `json:"position_unit"`
	BookSize               int64  `json:"book_size"`
	BookModifiedAtUnixNano int64  `json:"book_modified_at_unix_nano"`
	BookFingerprint        string `json:"book_fingerprint"`
}

type ProgressStore interface {
	Load(book book.Book, currentSize int64) (int64, error)
	Save(book book.Book, position int64) error
	Reset(book book.Book) error
}

type JSONProgressStore struct{}

func (JSONProgressStore) Load(book book.Book, currentSize int64) (int64, error) {
	data, err := ReadForBook(book)
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

func ReadFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, MaxProgressFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxProgressFileSize {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrFormat, MaxProgressFileSize)
	}
	return data, nil
}

// ReadForBook викликається під lease поточного SaveFile, як і Save.
// Перенесення залишає legacy-файл незмінним; наявний новий файл завжди пріоритетний.
func ReadForBook(b book.Book) (data []byte, err error) {
	data, err = ReadFile(b.SaveFile)
	if !errors.Is(err, os.ErrNotExist) {
		return data, err
	}
	legacyPath := book.LegacyProgressPath(b.Path, b.SaveFile)
	if legacyPath == "" {
		return nil, err
	}
	if _, err := os.Stat(legacyPath); err != nil {
		return nil, err
	}
	lease, err := AcquireLease(legacyPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			data, err = nil, closeErr
		}
	}()
	data, err = ReadFile(legacyPath)
	if err != nil {
		return nil, err
	}
	var saved Progress
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("%w: invalid legacy progress JSON: %v", ErrFormat, err)
	}
	pos, err := ValidateForBook(b, saved, b.Size)
	if errors.Is(err, ErrBookMismatch) {
		// Колізійний legacy-файл може належати іншій книзі: його не переносимо й не змінюємо.
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	boundary, err := chunk.IsFileUTF8Boundary(b.Path, pos, b.Size)
	if err != nil {
		return nil, err
	}
	if !boundary {
		return nil, ErrPositionInside
	}
	if err := WriteFileReplace(b.SaveFile, data, 0600); err != nil {
		return nil, fmt.Errorf("migrate legacy progress: %w", err)
	}
	return data, nil
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
		Version:                Version,
		LastPosition:           pos,
		PositionUnit:           Unit,
		BookSize:               book.File.Size,
		BookModifiedAtUnixNano: book.File.ModifiedAt.UnixNano(),
		BookFingerprint:        book.File.Fingerprint,
	}
}

func ValidateForBook(book book.Book, progress Progress, currentSize int64) (int64, error) {
	if progress.Version != Version {
		return 0, fmt.Errorf("%w: version %d", ErrFormat, progress.Version)
	}
	if progress.PositionUnit != Unit {
		return 0, fmt.Errorf("%w: position unit %q", ErrFormat, progress.PositionUnit)
	}
	if progress.BookSize != currentSize ||
		progress.BookModifiedAtUnixNano != book.File.ModifiedAt.UnixNano() ||
		progress.BookFingerprint != book.File.Fingerprint {
		return 0, ErrBookMismatch
	}
	if progress.LastPosition < 0 || progress.LastPosition > currentSize {
		return 0, ErrPositionOutside
	}
	return progress.LastPosition, nil
}
