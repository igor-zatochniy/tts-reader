package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const maxFuzzInputSize = 64 << 10

func FuzzChunkReader(f *testing.F) {
	f.Add([]byte("Hello, world! Next sentence."), 16)
	f.Add([]byte("Привіт, світе! Наступне речення."), 16)
	f.Add([]byte("Emoji 😀 text. Кирилиця."), 12)
	f.Add([]byte{0xff, 0xfe, 0xfd}, 8)

	f.Fuzz(func(t *testing.T, data []byte, limit int) {
		if len(data) > maxFuzzInputSize {
			t.Skip("fuzz input is intentionally bounded for fast local runs")
		}
		limit = normalizeFuzzLimit(limit)

		reader, err := NewStreamingChunkReader(bytes.NewReader(data), 0, limit)
		if err != nil {
			t.Fatalf("NewStreamingChunkReader returned unexpected error: %v", err)
		}

		var joined bytes.Buffer
		wantStart := int64(0)
		for {
			chunk, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				if utf8.Valid(data) {
					t.Fatalf("valid UTF-8 input failed: %v", err)
				}
				return
			}
			if chunk.StartByte != wantStart {
				t.Fatalf("chunk start mismatch: got %d, want %d", chunk.StartByte, wantStart)
			}
			if chunk.EndByte <= chunk.StartByte {
				t.Fatalf("invalid chunk byte range: start=%d end=%d", chunk.StartByte, chunk.EndByte)
			}
			if !utf8.ValidString(chunk.Text) {
				t.Fatalf("chunk text is not valid UTF-8: %q", chunk.Text)
			}
			joined.WriteString(chunk.Text)
			wantStart = chunk.EndByte
		}

		if !utf8.Valid(data) {
			return
		}
		if !bytes.Equal(joined.Bytes(), data) {
			t.Fatalf("chunks do not reconstruct original input")
		}
		if wantStart != int64(len(data)) {
			t.Fatalf("final byte position mismatch: got %d, want %d", wantStart, len(data))
		}
	})
}

func FuzzUTF8Boundary(f *testing.F) {
	f.Add([]byte("Hello"), int64(0))
	f.Add([]byte("Аудіо"), int64(1))
	f.Add([]byte("Аудіо"), int64(2))
	f.Add([]byte("Emoji 😀 text"), int64(7))
	f.Add([]byte{0xff, 0x80, 'a'}, int64(1))

	f.Fuzz(func(t *testing.T, data []byte, pos int64) {
		if len(data) > maxFuzzInputSize {
			t.Skip("fuzz input is intentionally bounded for fast local runs")
		}
		path := writeFuzzFile(t, data)
		got, err := isFileUTF8Boundary(path, pos, int64(len(data)))
		if err != nil {
			t.Fatalf("isFileUTF8Boundary returned error: %v", err)
		}

		want := expectedUTF8Boundary(data, pos)
		if got != want {
			t.Fatalf("boundary mismatch for pos=%d: got %v, want %v", pos, got, want)
		}
	})
}

func FuzzProgressLoad(f *testing.F) {
	f.Add([]byte("Hello, world!"), mustProgressJSON(f, Progress{LastPosition: 0, Unit: PositionUnit}))
	f.Add([]byte("Аудіо"), mustProgressJSON(f, Progress{LastPosition: 2, Unit: PositionUnit}))
	f.Add([]byte("Аудіо"), mustProgressJSON(f, Progress{LastPosition: 1, Unit: PositionUnit}))
	f.Add([]byte("Hello"), []byte(`{"last_position":-1,"unit":"bytes (UTF-8)"}`))
	f.Add([]byte("Hello"), []byte(`not-json`))

	f.Fuzz(func(t *testing.T, bookData []byte, progressData []byte) {
		if len(bookData) > maxFuzzInputSize || len(progressData) > maxFuzzInputSize {
			t.Skip("fuzz input is intentionally bounded for fast local runs")
		}

		dir := t.TempDir()
		bookPath := filepath.Join(dir, "book.txt")
		savePath := filepath.Join(dir, "progress.json")
		if err := os.WriteFile(bookPath, bookData, 0644); err != nil {
			t.Fatalf("failed to write fuzz book: %v", err)
		}
		if err := os.WriteFile(savePath, progressData, 0644); err != nil {
			t.Fatalf("failed to write fuzz progress: %v", err)
		}

		app := &App{
			cfg:    Config{BookFile: bookPath, SaveFile: savePath},
			stdout: io.Discard,
			stderr: io.Discard,
		}
		pos, hasSave, err := app.loadProgress(int64(len(bookData)))
		if err != nil {
			return
		}
		if !hasSave {
			if pos != 0 {
				t.Fatalf("progress without save must return zero position, got %d", pos)
			}
			return
		}
		if pos < 0 || pos >= int64(len(bookData)) {
			t.Fatalf("loaded position is outside readable range: %d of %d", pos, len(bookData))
		}
		ok, err := isFileUTF8Boundary(bookPath, pos, int64(len(bookData)))
		if err != nil {
			t.Fatalf("failed to recheck loaded boundary: %v", err)
		}
		if !ok {
			t.Fatalf("loaded position is inside UTF-8 rune: %d", pos)
		}
	})
}

func FuzzStartPosition(f *testing.F) {
	f.Add([]byte("Chapter one. Chapter two."), "Chapter two")
	f.Add([]byte("Перший розділ. Другий розділ."), "Другий")
	f.Add([]byte("Emoji 😀 chapter."), "😀")
	f.Add([]byte{0xff, 0xfe, 0xfd}, "bad")
	f.Add([]byte("Hello"), string([]byte{0xff}))

	f.Fuzz(func(t *testing.T, bookData []byte, phrase string) {
		if len(bookData) > maxFuzzInputSize || len(phrase) > 1024 {
			t.Skip("fuzz input is intentionally bounded for fast local runs")
		}

		path := writeFuzzFile(t, bookData)
		pos, found, err := findPhraseOffset(path, phrase)
		if err != nil {
			if utf8.Valid(bookData) && utf8.ValidString(phrase) {
				t.Fatalf("valid inputs returned error: %v", err)
			}
			return
		}
		if !found {
			if utf8.Valid(bookData) && utf8.ValidString(phrase) && strings.Contains(string(bookData), phrase) {
				t.Fatalf("phrase exists but was not found: %q", phrase)
			}
			return
		}

		if pos < 0 || pos > int64(len(bookData)) {
			t.Fatalf("found position is outside book: %d of %d", pos, len(bookData))
		}
		ok, err := isFileUTF8Boundary(path, pos, int64(len(bookData)))
		if err != nil {
			t.Fatalf("failed to validate found position boundary: %v", err)
		}
		if !ok {
			t.Fatalf("found position is inside UTF-8 rune: %d", pos)
		}
		if phrase == "" {
			if pos != 0 {
				t.Fatalf("empty phrase must resolve to zero, got %d", pos)
			}
			return
		}
		if utf8.Valid(bookData) && utf8.ValidString(phrase) {
			start := int(pos)
			end := start + len(phrase)
			if end > len(bookData) || string(bookData[start:end]) != phrase {
				t.Fatalf("found position does not point to phrase: pos=%d phrase=%q", pos, phrase)
			}
		}
	})
}

func normalizeFuzzLimit(limit int) int {
	if limit < 0 {
		limit = -limit
	}
	return limit%512 + 1
}

func expectedUTF8Boundary(data []byte, pos int64) bool {
	if pos < 0 || pos > int64(len(data)) {
		return false
	}
	if pos == 0 || pos == int64(len(data)) {
		return true
	}
	return utf8.RuneStart(data[pos])
}

func writeFuzzFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.txt")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write fuzz file: %v", err)
	}
	return path
}

func mustProgressJSON(t testing.TB, progress Progress) []byte {
	t.Helper()
	data, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("failed to marshal progress seed: %v", err)
	}
	return data
}
