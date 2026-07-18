package main

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
	if progress.Unit != PositionUnit {
		return 0, fmt.Errorf("incompatible progress unit %q", progress.Unit)
	}
	if progress.LastPosition < 0 || progress.LastPosition > currentSize {
		return 0, ErrPositionOutsideBook
	}
	ok, err := isFileUTF8Boundary(book.Path, progress.LastPosition, currentSize)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrPositionInsideRune
	}
	if progress.LastPosition == currentSize {
		return 0, nil
	}
	return progress.LastPosition, nil
}

func (JSONProgressStore) Save(book Book, pos int64) error {
	data, err := json.Marshal(Progress{LastPosition: pos, Unit: PositionUnit})
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
