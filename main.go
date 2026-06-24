package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// Позиція прогресу зберігається в байтах, бо рядки Go індексуються байтовими зміщеннями.
	PositionUnit      = "bytes (UTF-8)"
	defaultChunkSize  = 250
	defaultTTSTimeout = 2 * time.Minute
	previewRuneLimit  = 70
)

type Progress struct {
	LastPosition int64  `json:"last_position"`
	Unit         string `json:"unit"`
}

type Config struct {
	BookFile    string
	SaveFile    string
	StartPhrase string
	Voice       string
	ChunkSize   int
	TTSTimeout  time.Duration
}

type speakFunc func(text string) error
type speakerFactory func(cfg Config) speakFunc

type App struct {
	cfg     Config
	speaker speakFunc
	stdout  io.Writer
	stderr  io.Writer
	pos     atomic.Int64
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

	app := &App{
		cfg:     cfg,
		speaker: makeSpeaker(cfg),
		stdout:  stdout,
		stderr:  stderr,
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

	if enableSignals {
		app.setupGracefulShutdown()
	}

	info, err := os.Stat(cfg.BookFile)
	if err != nil {
		fmt.Fprintf(stderr, "Помилка: не вдалося прочитати файл книги %q: %v\n", cfg.BookFile, err)
		return 1
	}
	if info.IsDir() {
		fmt.Fprintf(stderr, "Помилка: шлях книги %q є директорією\n", cfg.BookFile)
		return 1
	}

	bookSize := info.Size()
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
		if err := app.speaker(chunk.Text); err != nil {
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
	fs.StringVar(&cfg.SaveFile, "save", "book_save.json", "Шлях до файлу прогресу")
	fs.StringVar(&cfg.StartPhrase, "start", "", "Фраза для старту, яка ігнорує збережений прогрес")
	fs.StringVar(&cfg.Voice, "voice", "", "Точна назва голосу Windows SAPI")
	fs.IntVar(&cfg.ChunkSize, "chunk", defaultChunkSize, "Розмір фрагмента для озвучення у символах")
	fs.DurationVar(&cfg.TTSTimeout, "tts-timeout", defaultTTSTimeout, "Максимальний час очікування одного TTS-фрагмента")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if cfg.ChunkSize <= 0 {
		return Config{}, fmt.Errorf("значення -chunk має бути більшим за 0")
	}
	if cfg.TTSTimeout <= 0 {
		return Config{}, fmt.Errorf("значення -tts-timeout має бути більшим за 0")
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

	savedPos, hasSave, err := a.loadProgress(bookSize)
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

func (a *App) loadProgress(bookSize int64) (int64, bool, error) {
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
	if p.Unit != PositionUnit {
		return 0, false, fmt.Errorf("файл прогресу має несумісну одиницю позиції %q", p.Unit)
	}
	if p.LastPosition < 0 || p.LastPosition > bookSize {
		return 0, false, fmt.Errorf("файл прогресу має позицію поза межами книги: %d", p.LastPosition)
	}
	// Файл збереження може бути змінений вручну, тому позицію перевіряємо до потокового читання.
	isBoundary, err := isFileUTF8Boundary(a.cfg.BookFile, p.LastPosition, bookSize)
	if err != nil {
		return 0, false, fmt.Errorf("не вдалося перевірити UTF-8 межу прогресу: %w", err)
	}
	if !isBoundary {
		return 0, false, fmt.Errorf("файл прогресу має позицію всередині UTF-8 символу: %d", p.LastPosition)
	}
	if p.LastPosition == bookSize {
		return 0, false, nil
	}
	return p.LastPosition, true, nil
}

func (a *App) saveProgress(pos int64) error {
	data, err := json.Marshal(Progress{
		LastPosition: pos,
		Unit:         PositionUnit,
	})
	if err != nil {
		return fmt.Errorf("не вдалося серіалізувати прогрес: %w", err)
	}
	if err := writeFileReplace(a.cfg.SaveFile, data, 0644); err != nil {
		return fmt.Errorf("не вдалося записати файл %q: %w", a.cfg.SaveFile, err)
	}
	return nil
}

// Запис через тимчасовий файл зменшує ризик пошкодити JSON прогресу під час збою процесу.
func writeFileReplace(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}

	tmpName := tmp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		if retryErr := os.Rename(tmpName, path); retryErr != nil {
			return retryErr
		}
	}

	keepTemp = false
	return nil
}

func (a *App) setupGracefulShutdown() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		// Обробник сигналу читає лише атомарну позицію й завершує процес контрольовано.
		fmt.Fprintln(a.stdout, "\n--- ЗБЕРЕЖЕННЯ ПЕРЕД ВИХОДОМ... ---")

		pos := a.pos.Load()
		if err := a.saveProgress(pos); err != nil {
			fmt.Fprintf(a.stderr, "Помилка: не вдалося зберегти прогрес перед виходом: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
}

func progressPercent(pos int64, total int64) float64 {
	if total == 0 {
		return 100
	}
	return (float64(pos) / float64(total)) * 100
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
