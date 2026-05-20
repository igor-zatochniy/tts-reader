package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	bookPath := flag.String("book", "book.txt", "Path to a UTF-8 text file")
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

	for _, chunk := range splitTextSmart(string(text), *chunkSize) {
		if err := speakText(ctx, chunk, *voice, *timeout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
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
