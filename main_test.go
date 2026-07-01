package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunMissingBookReturnsFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := runWithOptions([]string{"-book", filepath.Join(t.TempDir(), "missing.txt")}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 1 {
		t.Fatalf("очікував exit code 1, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "не вдалося прочитати файл книги") {
		t.Fatalf("очікував помилку читання книги, stderr=%q", stderr.String())
	}
}

func TestRunRejectsInvalidChunk(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := runWithOptions([]string{"-chunk", "0"}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 2 {
		t.Fatalf("очікував exit code 2, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "більшим за 0") {
		t.Fatalf("очікував помилку валідації chunk, stderr=%q", stderr.String())
	}
}

func TestRunRejectsInvalidTTSTimeout(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := runWithOptions([]string{"-tts-timeout", "0s"}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 2 {
		t.Fatalf("очікував exit code 2, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "tts-timeout") {
		t.Fatalf("очікував помилку валідації tts-timeout, stderr=%q", stderr.String())
	}
}

func TestRunEmptyBookDoesNotPrintNaN(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.txt")
	save := filepath.Join(dir, "save.json")
	mustWriteFile(t, book, "")

	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", book, "-save", save}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 0 {
		t.Fatalf("очікував успішний запуск, отримав %d, stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Прогрес: NaN") {
		t.Fatalf("прогрес не має містити NaN, stdout=%q", stdout.String())
	}
	assertSavedPosition(t, save, 0)
}

func TestRunRejectsNegativeProgress(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.txt")
	save := filepath.Join(dir, "save.json")
	mustWriteFile(t, book, "Аудіокнига")
	mustWriteProgress(t, save, Progress{LastPosition: -1, Unit: PositionUnit})

	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", book, "-save", save}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 1 {
		t.Fatalf("очікував exit code 1, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "поза межами книги") {
		t.Fatalf("очікував помилку меж прогресу, stderr=%q", stderr.String())
	}
}

func TestRunRejectsProgressInsideUTF8Rune(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.txt")
	save := filepath.Join(dir, "save.json")
	mustWriteFile(t, book, "Аудіо")
	mustWriteProgress(t, save, Progress{LastPosition: 1, Unit: PositionUnit})

	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", book, "-save", save}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 1 {
		t.Fatalf("очікував exit code 1, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "UTF-8") {
		t.Fatalf("очікував помилку UTF-8 межі, stderr=%q", stderr.String())
	}
}

func TestRunUsesStartPhraseAndResetsProgressAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.txt")
	save := filepath.Join(dir, "save.json")
	mustWriteFile(t, book, "Перший розділ. Другий розділ.")

	var spoken []string
	speaker := testSpeaker(func(text string) error {
		spoken = append(spoken, text)
		return nil
	})

	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", book, "-save", save, "-start", "Другий", "-chunk", "100"}, &stdout, &stderr, speaker, false)

	if code != 0 {
		t.Fatalf("очікував успішний запуск, отримав %d, stderr=%q", code, stderr.String())
	}
	if len(spoken) != 1 || spoken[0] != "Другий розділ." {
		t.Fatalf("неочікуваний озвучений текст: %#v", spoken)
	}
	assertSavedPosition(t, save, 0)
}

func TestRunReadSubcommandUsesCLIReader(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.txt")
	save := filepath.Join(dir, "save.json")
	mustWriteFile(t, book, "Текст.")

	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"read", "-book", book, "-save", save}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 0 {
		t.Fatalf("очікував успішний запуск, отримав %d, stderr=%q", code, stderr.String())
	}
	assertSavedPosition(t, save, 0)
}

func TestRunPersistsPositionOnSpeakerFailure(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.txt")
	save := filepath.Join(dir, "save.json")
	mustWriteFile(t, book, "Текст для збою.")

	speakerErr := errors.New("tts failed")
	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", book, "-save", save, "-chunk", "100"}, &stdout, &stderr, testSpeaker(func(text string) error {
		return speakerErr
	}), false)

	if code != 1 {
		t.Fatalf("очікував exit code 1, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "ПОМИЛКА TTS") {
		t.Fatalf("очікував TTS помилку, stderr=%q", stderr.String())
	}
	assertSavedPosition(t, save, 0)
}

func TestRunPersistsCompletedStreamingPositionOnSecondChunkFailure(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.txt")
	save := filepath.Join(dir, "save.json")
	mustWriteFile(t, book, "Перший. Другий.")

	speakerErr := errors.New("tts failed")
	call := 0
	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", book, "-save", save, "-chunk", "8"}, &stdout, &stderr, testSpeaker(func(text string) error {
		call++
		if call == 2 {
			return speakerErr
		}
		return nil
	}), false)

	if code != 1 {
		t.Fatalf("очікував exit code 1, отримав %d", code)
	}
	assertSavedPosition(t, save, int64(len("Перший.")))
}

func TestRunRejectsInvalidUTF8Book(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "book.txt")
	save := filepath.Join(dir, "save.json")
	if err := os.WriteFile(book, []byte{0xff}, 0644); err != nil {
		t.Fatalf("не вдалося записати тестовий файл: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", book, "-save", save}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 1 {
		t.Fatalf("очікував exit code 1, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "UTF-8") {
		t.Fatalf("очікував помилку UTF-8, stderr=%q", stderr.String())
	}
}

func TestSplitTextSmartKeepsUnicodeText(t *testing.T) {
	chunks := splitTextSmart("Привіт, світе! Наступне речення.", 16)

	if len(chunks) < 2 {
		t.Fatalf("очікував кілька фрагментів, отримав %#v", chunks)
	}
	if strings.Join(chunks, "") != "Привіт, світе! Наступне речення." {
		t.Fatalf("фрагменти не відновлюють початковий текст: %#v", chunks)
	}
}

func TestStreamingChunkReaderKeepsUnicodeTextAndByteOffsets(t *testing.T) {
	text := "Привіт, світе! Наступне речення."
	reader, err := NewStreamingChunkReader(strings.NewReader(text), 0, 16)
	if err != nil {
		t.Fatalf("не вдалося створити streaming reader: %v", err)
	}

	var joined strings.Builder
	wantStart := int64(0)
	for {
		chunk, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("неочікувана помилка reader.Next: %v", err)
		}
		if chunk.StartByte != wantStart {
			t.Fatalf("очікував start byte %d, отримав %d", wantStart, chunk.StartByte)
		}
		if chunk.EndByte <= chunk.StartByte {
			t.Fatalf("некоректні межі фрагмента: %#v", chunk)
		}
		joined.WriteString(chunk.Text)
		wantStart = chunk.EndByte
	}

	if joined.String() != text {
		t.Fatalf("фрагменти не відновлюють початковий текст: %q", joined.String())
	}
	if wantStart != int64(len(text)) {
		t.Fatalf("очікував фінальний byte offset %d, отримав %d", len(text), wantStart)
	}
}

func testSpeaker(fn func(text string) error) speakerFactory {
	return func(cfg Config) speakFunc {
		if fn != nil {
			return func(ctx context.Context, text string) error {
				return fn(text)
			}
		}
		return func(ctx context.Context, text string) error {
			return nil
		}
	}
}

func mustWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("не вдалося записати тестовий файл: %v", err)
	}
}

func mustWriteProgress(t *testing.T, path string, progress Progress) {
	t.Helper()
	data, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("не вдалося серіалізувати прогрес: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("не вдалося записати прогрес: %v", err)
	}
}

func assertSavedPosition(t *testing.T, path string, want int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не вдалося прочитати прогрес: %v", err)
	}

	var got Progress
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("не вдалося розібрати прогрес: %v", err)
	}
	if got.LastPosition != want {
		t.Fatalf("очікував позицію %d, отримав %d", want, got.LastPosition)
	}
	if got.Unit != PositionUnit {
		t.Fatalf("очікував unit %q, отримав %q", PositionUnit, got.Unit)
	}
}
