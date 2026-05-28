package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	bookPath := flag.String("book", "book.txt", "Path to a UTF-8 text file")
	savePath := flag.String("save", "book_save.json", "Path to a JSON progress file")
	chunkSize := flag.Int("chunk", 400, "Maximum runes per TTS request")
	voice := flag.String("voice", "", "Optional Windows SAPI voice name")
	timeout := flag.Duration("timeout", 5*time.Minute, "Maximum time for one speech request")
	flag.Parse()

	text, err := os.ReadFile(*bookPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read book: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	progress := loadProgress(*savePath)
	chunks := splitTextSmart(string(text), *chunkSize)
	for i, chunk := range chunks {
		if int64(i) < progress.ChunkIndex {
			continue
		}
		if err := speakText(ctx, chunk, *voice, *timeout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		progress.ChunkIndex = int64(i + 1)
		if err := saveProgress(*savePath, progress); err != nil {
			fmt.Fprintf(os.Stderr, "save progress: %v\n", err)
			os.Exit(1)
		}
	}
}

type progressFile struct {
	ChunkIndex int64 `json:"chunk_index"`
}

func loadProgress(path string) progressFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return progressFile{}
	}

	var progress progressFile
	if err := json.Unmarshal(data, &progress); err != nil {
		return progressFile{}
	}
	return progress
}

func saveProgress(path string, progress progressFile) error {
	data, err := json.MarshalIndent(progress, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func splitTextSmart(text string, limit int) []string {
	if limit <= 0 {
		limit = 400
	}

	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	var chunks []string
	var current strings.Builder
	for _, word := range words {
		nextLen := current.Len() + len(word)
		if current.Len() > 0 {
			nextLen++
		}
		if current.Len() > 0 && nextLen > limit {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(word)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}
