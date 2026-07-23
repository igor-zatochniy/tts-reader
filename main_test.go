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
	"time"
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
	if !strings.Contains(stderr.String(), "-chunk") || !strings.Contains(stderr.String(), "10000") {
		t.Fatalf("очікував помилку валідації chunk, stderr=%q", stderr.String())
	}
}

func TestRunRejectsChunkAboveMaximum(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := runWithOptions([]string{"-chunk", "10001"}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 2 {
		t.Fatalf("очікував exit code 2, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "-chunk") || !strings.Contains(stderr.String(), "10000") {
		t.Fatalf("очікував помилку валідації максимального chunk, stderr=%q", stderr.String())
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
	mustWriteProgress(t, save, progressForPath(t, book, save, -1))

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
	mustWriteProgress(t, save, progressForPath(t, book, save, 1))

	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", book, "-save", save}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 1 {
		t.Fatalf("очікував exit code 1, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "UTF-8") {
		t.Fatalf("очікував помилку UTF-8 межі, stderr=%q", stderr.String())
	}
}

func TestRunRejectsProgressFromDifferentBook(t *testing.T) {
	dir := t.TempDir()
	firstBook := filepath.Join(dir, "first.txt")
	secondBook := filepath.Join(dir, "second.txt")
	save := filepath.Join(dir, "shared.progress.json")
	mustWriteFile(t, firstBook, "abcdef")
	mustWriteFile(t, secondBook, "uvwxyz")
	mustWriteProgress(t, save, progressForPath(t, firstBook, save, 3))

	var stdout, stderr bytes.Buffer
	code := runWithOptions([]string{"-book", secondBook, "-save", save}, &stdout, &stderr, testSpeaker(nil), false)

	if code != 1 {
		t.Fatalf("очікував exit code 1, отримав %d", code)
	}
	if !strings.Contains(stderr.String(), "іншій книзі") {
		t.Fatalf("очікував помилку несумісного progress, stderr=%q", stderr.String())
	}
}

func TestDefaultProgressPathUsesFullBookFileName(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "novel.txt")
	md := filepath.Join(dir, "novel.md")

	if defaultProgressPath(txt) != txt+".progress.json" {
		t.Fatalf("неочікуваний progress path для txt: %q", defaultProgressPath(txt))
	}
	if defaultProgressPath(md) != md+".progress.json" {
		t.Fatalf("неочікуваний progress path для md: %q", defaultProgressPath(md))
	}
	if defaultProgressPath(txt) == defaultProgressPath(md) {
		t.Fatalf("progress paths collide: %q", defaultProgressPath(txt))
	}
}

func TestWriteFileReplaceReplacesProgressAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	save := filepath.Join(dir, "book_save.json")
	mustWriteFile(t, save, "old-progress")

	if err := writeFileReplace(save, []byte("new-progress"), 0644); err != nil {
		t.Fatalf("не вдалося замінити progress: %v", err)
	}

	data, err := os.ReadFile(save)
	if err != nil {
		t.Fatalf("не вдалося прочитати progress: %v", err)
	}
	if string(data) != "new-progress" {
		t.Fatalf("progress не оновився: %q", data)
	}
	assertNoProgressTemps(t, dir, save)
}

func TestWriteFileReplaceKeepsTargetWhenReplaceFails(t *testing.T) {
	dir := t.TempDir()
	save := filepath.Join(dir, "book_save.json")
	mustWriteFile(t, save, "old-progress")
	replaceErr := errors.New("replace failed")

	err := writeFileReplaceWith(save, []byte("new-progress"), 0644, func(tmpName string, targetName string) error {
		if targetName != save {
			t.Fatalf("неочікуваний target: %q", targetName)
		}
		if _, err := os.Stat(tmpName); err != nil {
			t.Fatalf("temp-файл має існувати перед replace: %v", err)
		}
		return replaceErr
	})
	if !errors.Is(err, replaceErr) {
		t.Fatalf("очікував replaceErr, отримав %v", err)
	}

	data, err := os.ReadFile(save)
	if err != nil {
		t.Fatalf("не вдалося прочитати старий progress: %v", err)
	}
	if string(data) != "old-progress" {
		t.Fatalf("старий progress не має втрачатися після помилки replace: %q", data)
	}
	assertNoProgressTemps(t, dir, save)
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

func TestFunctionEngineStopCancelsActiveSpeak(t *testing.T) {
	started := make(chan struct{}, 1)
	engine := newFunctionEngineFactory(
		func(cfg Config) speakFunc {
			return func(ctx context.Context, text string) error {
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			}
		},
		func() ([]string, error) { return nil, nil },
	)(Config{})

	speakDone := make(chan error, 1)
	go func() {
		speakDone <- engine.Speak(context.Background(), "Тест")
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Speak не стартував")
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- engine.Stop(context.Background())
	}()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("очікував успішний Stop, отримав %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop не завершився")
	}

	select {
	case err := <-speakDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("очікував context.Canceled від Speak, отримав %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Speak не завершився після Stop")
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

func progressForPath(t *testing.T, bookPath string, savePath string, pos int64) Progress {
	t.Helper()
	identity, err := inspectBookFile(bookPath)
	if err != nil {
		t.Fatalf("не вдалося отримати fingerprint книги: %v", err)
	}
	return progressForBook(progressBook(bookPath, savePath, identity), pos)
}

func assertNoProgressTemps(t *testing.T, dir string, target string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "."+filepath.Base(target)+".tmp-*"))
	if err != nil {
		t.Fatalf("не вдалося перевірити temp-файли: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("неочікувані temp-файли progress: %#v", matches)
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
	if got.Version != ProgressVersion {
		t.Fatalf("очікував version %d, отримав %d", ProgressVersion, got.Version)
	}
	if got.PositionUnit != PositionUnit {
		t.Fatalf("очікував position_unit %q, отримав %q", PositionUnit, got.PositionUnit)
	}
	if got.BookSize < 0 || got.BookFingerprint == "" {
		t.Fatalf("progress не прив'язаний до книги: %#v", got)
	}
}
