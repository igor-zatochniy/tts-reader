package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	bookPath := flag.String("book", "book.txt", "Path to a UTF-8 text file")
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

	if err := speakText(ctx, string(text), *voice, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
