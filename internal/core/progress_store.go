package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type ProgressStore interface {
	Load(book Book, currentSize int64) (int64, error)
	Save(book Book, position int64) error
	Reset(book Book) error
}

type JSONProgressStore struct{}

func (JSONProgressStore) Load(book Book, currentSize int64) (int64, error) {
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
	pos, err := validateProgressForBook(book, progress, currentSize)
	if err != nil {
		return 0, err
	}
	ok, err := isFileUTF8Boundary(book.Path, pos, currentSize)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrPositionInsideRune
	}
	if pos == currentSize {
		return 0, nil
	}
	return pos, nil
}

func (JSONProgressStore) Save(book Book, pos int64) error {
	data, err := json.Marshal(progressForBook(book, pos))
	if err != nil {
		return fmt.Errorf("marshal progress: %w", err)
	}
	if err := writeFileReplace(book.SaveFile, data, 0644); err != nil {
		return fmt.Errorf("replace progress file: %w", err)
	}
	return nil
}

func (s JSONProgressStore) Reset(book Book) error {
	return s.Save(book, 0)
}

func saveBookProgress(book Book, pos int64) error {
	return JSONProgressStore{}.Save(book, pos)
}

func progressBook(bookPath, saveFile string, identity BookFileIdentity) Book {
	return Book{
		Path:     bookPath,
		SaveFile: saveFile,
		Size:     identity.Size,
		File:     identity,
	}
}

func progressForBook(book Book, pos int64) Progress {
	return Progress{
		Version:         ProgressVersion,
		LastPosition:    pos,
		PositionUnit:    PositionUnit,
		BookSize:        book.File.Size,
		BookFingerprint: book.File.Fingerprint,
	}
}

func validateProgressForBook(book Book, progress Progress, currentSize int64) (int64, error) {
	if progress.Version != ProgressVersion {
		return 0, fmt.Errorf("%w: version %d", ErrProgressFormat, progress.Version)
	}
	if progress.PositionUnit != PositionUnit {
		return 0, fmt.Errorf("%w: position unit %q", ErrProgressFormat, progress.PositionUnit)
	}
	if progress.BookSize != currentSize || progress.BookFingerprint != book.File.Fingerprint {
		return 0, ErrProgressBookMismatch
	}
	if progress.LastPosition < 0 || progress.LastPosition > currentSize {
		return 0, ErrPositionOutsideBook
	}
	return progress.LastPosition, nil
}
