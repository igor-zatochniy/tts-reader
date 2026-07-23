package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultTTSTimeout = 2 * time.Minute
	previewRuneLimit  = 70
)

type App struct {
	cfg     Config
	speaker speakFunc
	stdout  io.Writer
	stderr  io.Writer
	ctx     context.Context
	pos     atomic.Int64
	book    BookFileIdentity
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, newSpeaker))
}

func run(args []string, stdout, stderr io.Writer, makeSpeaker speakerFactory) int {
	return runWithOptions(args, stdout, stderr, makeSpeaker, true)
}

func runWithOptions(args []string, stdout, stderr io.Writer, makeSpeaker speakerFactory, enableSignals bool) (exitCode int) {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return runServe(args[1:], stdout, stderr, makeSpeaker, listVoices, enableSignals)
		case "read":
			args = args[1:]
		}
	}

	cfg, err := parseConfig(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "Помилка: %v\n", err)
		return 2
	}

	ctx := context.Background()
	cleanupSignals := func() {}
	if enableSignals {
		ctx, cleanupSignals = interruptContext()
	}
	defer cleanupSignals()

	app := &App{
		cfg:     cfg,
		speaker: makeSpeaker(cfg),
		stdout:  stdout,
		stderr:  stderr,
		ctx:     ctx,
	}

	// Останній запобіжник для CLI: зберігаємо прогрес навіть після неочікуваної panic.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "\n[КРИТИЧНА ПОМИЛКА] Паніка: %v\n", r)
			if err := app.saveProgress(app.pos.Load()); err != nil {
				fmt.Fprintf(stderr, "Помилка: не вдалося зберегти прогрес після паніки: %v\n", err)
			}
			exitCode = 1
		}
	}()

	bookIdentity, err := inspectBookFile(cfg.BookFile)
	if err != nil {
		fmt.Fprintf(stderr, "Помилка: не вдалося прочитати файл книги %q: %v\n", cfg.BookFile, err)
		return 1
	}
	app.book = bookIdentity

	bookSize := bookIdentity.Size
	if bookSize == 0 {
		fmt.Fprintln(stdout, "--- ПОРОЖНЯ КНИГА ---")
		fmt.Fprintf(stdout, "Книга: %s\n", cfg.BookFile)
		fmt.Fprintf(stdout, "Збереження: %s\n", cfg.SaveFile)
		fmt.Fprintln(stdout, "Прогрес: 100.00%")
		if err := app.saveProgress(0); err != nil {
			fmt.Fprintf(stderr, "Помилка: не вдалося записати прогрес: %v\n", err)
			return 1
		}
		return 0
	}

	startPos, err := app.resolveStartPosition(bookSize)
	if err != nil {
		fmt.Fprintf(stderr, "Помилка: %v\n", err)
		return 1
	}
	app.pos.Store(startPos)

	preview, err := previewTextFromFile(cfg.BookFile, startPos, previewRuneLimit)
	if err != nil {
		fmt.Fprintf(stderr, "Помилка: не вдалося прочитати попередній перегляд книги: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Книга: %s\n", cfg.BookFile)
	fmt.Fprintf(stdout, "Збереження: %s\n", cfg.SaveFile)
	fmt.Fprintf(stdout, "Розмір фрагмента: %d символів\n", cfg.ChunkSize)
	if cfg.Voice == "" {
		fmt.Fprintln(stdout, "Голос: системний за замовчуванням")
	} else {
		fmt.Fprintf(stdout, "Голос: %s\n", cfg.Voice)
	}
	fmt.Fprintf(stdout, "Прогрес: %.2f%%\n", progressPercent(startPos, bookSize))
	fmt.Fprintf(stdout, "Текст: \"...%s...\"\n", preview)
	fmt.Fprintln(stdout, "Ctrl+C для виходу.")
	fmt.Fprintln(stdout, "------------------------------------------------")

	book, err := os.Open(cfg.BookFile)
	if err != nil {
		fmt.Fprintf(stderr, "Помилка: не вдалося відкрити файл книги %q: %v\n", cfg.BookFile, err)
		return 1
	}
	defer book.Close()

	if _, err := book.Seek(startPos, io.SeekStart); err != nil {
		fmt.Fprintf(stderr, "Помилка: не вдалося перейти до позиції %d bytes у книзі: %v\n", startPos, err)
		return 1
	}

	reader, err := NewStreamingChunkReader(book, startPos, cfg.ChunkSize)
	if err != nil {
		fmt.Fprintf(stderr, "Помилка: %v\n", err)
		return 1
	}

	for {
		if app.ctx.Err() != nil {
			return app.persistInterruptedProgress()
		}

		chunk, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			pos := app.pos.Load()
			fmt.Fprintf(stderr, "\n[ПОМИЛКА ЧИТАННЯ] Не вдалося прочитати фрагмент на позиції %d bytes: %v\n", pos, err)
			if saveErr := app.saveProgress(pos); saveErr != nil {
				fmt.Fprintf(stderr, "Помилка: не вдалося зберегти прогрес після збою читання: %v\n", saveErr)
			}
			return 1
		}

		app.pos.Store(chunk.StartByte)
		if err := app.speaker(app.ctx, chunk.Text); err != nil {
			if app.ctx.Err() != nil {
				return app.persistInterruptedProgress()
			}
			pos := app.pos.Load()
			fmt.Fprintf(stderr, "\n[ПОМИЛКА TTS] PowerShell завершився з помилкою на позиції %d bytes: %v\n", pos, err)
			if saveErr := app.saveProgress(pos); saveErr != nil {
				fmt.Fprintf(stderr, "Помилка: не вдалося зберегти прогрес після збою TTS: %v\n", saveErr)
			}
			return 1
		}

		app.pos.Store(chunk.EndByte)
		if err := app.saveProgress(app.pos.Load()); err != nil {
			fmt.Fprintf(stderr, "Помилка: не вдалося записати прогрес: %v\n", err)
			return 1
		}
	}

	if err := app.saveProgress(0); err != nil {
		fmt.Fprintf(stderr, "Помилка: не вдалося скинути прогрес після завершення: %v\n", err)
		return 1
	}
	return 0
}

func parseConfig(args []string, output io.Writer) (Config, error) {
	fs := flag.NewFlagSet("audiobook", flag.ContinueOnError)
	fs.SetOutput(output)

	cfg := Config{}
	fs.StringVar(&cfg.BookFile, "book", "book.txt", "Шлях до текстового файлу книги")
	fs.StringVar(&cfg.SaveFile, "save", "", "Шлях до файлу прогресу")
	fs.StringVar(&cfg.StartPhrase, "start", "", "Фраза для старту, яка ігнорує збережений прогрес")
	fs.StringVar(&cfg.Voice, "voice", "", "Точна назва голосу Windows SAPI")
	fs.IntVar(&cfg.ChunkSize, "chunk", defaultChunkSize, "Розмір фрагмента для озвучення у символах")
	fs.DurationVar(&cfg.TTSTimeout, "tts-timeout", defaultTTSTimeout, "Максимальний час очікування одного TTS-фрагмента")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if err := validateChunkSize(cfg.ChunkSize); err != nil {
		return Config{}, fmt.Errorf("значення -chunk має бути між 1 і %d: %w", maxChunkSize, err)
	}
	if cfg.TTSTimeout <= 0 {
		return Config{}, fmt.Errorf("значення -tts-timeout має бути більшим за 0")
	}
	if strings.TrimSpace(cfg.SaveFile) == "" {
		cfg.SaveFile = defaultProgressPath(cfg.BookFile)
	}
	return cfg, nil
}

func (a *App) resolveStartPosition(bookSize int64) (int64, error) {
	if a.cfg.StartPhrase != "" {
		fmt.Fprintf(a.stdout, "--- ПОШУК ФРАЗИ: %q ---\n", a.cfg.StartPhrase)
		idx, found, err := findPhraseOffset(a.cfg.BookFile, a.cfg.StartPhrase)
		if err != nil {
			return 0, fmt.Errorf("не вдалося знайти стартову фразу: %w", err)
		}
		if !found {
			fmt.Fprintln(a.stdout, "Фразу не знайдено. Старт з початку.")
			return 0, nil
		}
		fmt.Fprintln(a.stdout, "Фразу знайдено. Починаю читання з неї.")
		return idx, nil
	}

	savedPos, hasSave, err := a.loadProgress(a.book)
	if err != nil {
		return 0, err
	}
	if hasSave {
		fmt.Fprintln(a.stdout, "--- ЗАВАНТАЖЕННЯ ПРОГРЕСУ ---")
		return savedPos, nil
	}

	fmt.Fprintln(a.stdout, "--- НОВЕ ЧИТАННЯ ---")
	return 0, nil
}

func (a *App) loadProgress(bookIdentity BookFileIdentity) (int64, bool, error) {
	file, err := os.ReadFile(a.cfg.SaveFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("не вдалося прочитати файл прогресу %q: %w", a.cfg.SaveFile, err)
	}

	var p Progress
	if err := json.Unmarshal(file, &p); err != nil {
		return 0, false, fmt.Errorf("файл прогресу має некоректний JSON: %w", err)
	}
	pos, err := validateProgressForBook(progressBook(a.cfg.BookFile, a.cfg.SaveFile, bookIdentity), p, bookIdentity.Size)
	if err != nil {
		switch {
		case errors.Is(err, ErrProgressBookMismatch):
			return 0, false, fmt.Errorf("файл прогресу належить іншій книзі")
		case errors.Is(err, ErrPositionOutsideBook):
			return 0, false, fmt.Errorf("файл прогресу має позицію поза межами книги: %d", p.LastPosition)
		case errors.Is(err, ErrProgressFormat):
			return 0, false, fmt.Errorf("файл прогресу має непідтримуваний формат: %w", err)
		default:
			return 0, false, fmt.Errorf("файл прогресу несумісний з поточною книгою: %w", err)
		}
	}
	isBoundary, err := isFileUTF8Boundary(a.cfg.BookFile, pos, bookIdentity.Size)
	if err != nil {
		return 0, false, fmt.Errorf("не вдалося перевірити UTF-8 межу прогресу: %w", err)
	}
	if !isBoundary {
		return 0, false, fmt.Errorf("файл прогресу має позицію всередині UTF-8 символу: %d", pos)
	}
	if pos == bookIdentity.Size {
		return 0, false, nil
	}
	return pos, true, nil
}

func (a *App) saveProgress(pos int64) error {
	book := progressBook(a.cfg.BookFile, a.cfg.SaveFile, a.book)
	if book.File.Fingerprint == "" {
		identity, err := inspectBookFile(a.cfg.BookFile)
		if err != nil {
			return fmt.Errorf("не вдалося отримати ідентичність книги для прогресу: %w", err)
		}
		book = progressBook(a.cfg.BookFile, a.cfg.SaveFile, identity)
	}

	data, err := json.Marshal(progressForBook(book, pos))
	if err != nil {
		return fmt.Errorf("не вдалося серіалізувати прогрес: %w", err)
	}
	if err := writeFileReplace(a.cfg.SaveFile, data, 0644); err != nil {
		return fmt.Errorf("не вдалося записати файл %q: %w", a.cfg.SaveFile, err)
	}
	return nil
}

func interruptContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case <-signals:
			cancel()
		case <-ctx.Done():
			return
		}

		select {
		case <-signals:
			os.Exit(130)
		case <-ctx.Done():
			return
		}
	}()

	return ctx, func() {
		signal.Stop(signals)
		cancel()
	}
}

func (a *App) persistInterruptedProgress() int {
	fmt.Fprintln(a.stdout, "\n--- ЗБЕРЕЖЕННЯ ПЕРЕД ВИХОДОМ... ---")
	if err := a.saveProgress(a.pos.Load()); err != nil {
		fmt.Fprintf(a.stderr, "Помилка: не вдалося зберегти прогрес: %v\n", err)
		return 1
	}
	return 0
}

// Розбиваємо текст по rune, щоб не різати UTF-8 символи; позиція прогресу рахується у байтах.
func splitTextSmart(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if limit <= 0 {
		return []string{text}
	}

	var chunks []string
	runes := []rune(text)
	for len(runes) > 0 {
		if len(runes) <= limit {
			chunks = append(chunks, string(runes))
			break
		}

		cut := limit
		found := false
		for i := limit; i > limit/2; i-- {
			if runes[i] == '.' || runes[i] == '!' || runes[i] == '?' || runes[i] == '\n' {
				cut = i + 1
				found = true
				break
			}
		}
		if !found {
			for i := limit; i > limit/2; i-- {
				if runes[i] == ' ' {
					cut = i + 1
					found = true
					break
				}
			}
		}
		if !found {
			cut = limit
		}

		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	return chunks
}
