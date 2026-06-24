package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

var benchmarkChunkSink struct {
	chunks int
	bytes  int64
}

func BenchmarkChunker_ASCII_1MB(b *testing.B) {
	benchmarkChunkers(b, makeBenchmarkBook(1<<20, "The quick brown fox jumps over the lazy dog. "))
}

func BenchmarkChunker_UTF8_1MB(b *testing.B) {
	benchmarkChunkers(b, makeBenchmarkBook(1<<20, "Привіт, світе! Наступне речення українською мовою. "))
}

func BenchmarkChunker_ASCII_10MB(b *testing.B) {
	benchmarkChunkers(b, makeBenchmarkBook(10<<20, "The quick brown fox jumps over the lazy dog. "))
}

func BenchmarkChunker_UTF8_10MB(b *testing.B) {
	benchmarkChunkers(b, makeBenchmarkBook(10<<20, "Привіт, світе! Наступне речення українською мовою. "))
}

func BenchmarkChunker_ASCII_100MB(b *testing.B) {
	benchmarkChunkers(b, makeBenchmarkBook(100<<20, "The quick brown fox jumps over the lazy dog. "))
}

func BenchmarkChunker_UTF8_100MB(b *testing.B) {
	benchmarkChunkers(b, makeBenchmarkBook(100<<20, "Привіт, світе! Наступне речення українською мовою. "))
}

func benchmarkChunkers(b *testing.B, text string) {
	const chunkSize = 400

	b.Run("Original", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(text)))
		var chunksCount int
		var bytesCount int64
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			chunks := splitTextSmart(text, chunkSize)
			chunksCount += len(chunks)
			for _, chunk := range chunks {
				bytesCount += int64(len(chunk))
			}
		}
		benchmarkChunkSink.chunks = chunksCount
		benchmarkChunkSink.bytes = bytesCount
	})

	b.Run("Streaming", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(text)))
		var chunksCount int
		var bytesCount int64
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			reader, err := NewStreamingChunkReader(strings.NewReader(text), 0, chunkSize)
			if err != nil {
				b.Fatalf("не вдалося створити streaming reader: %v", err)
			}
			for {
				chunk, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					b.Fatalf("неочікувана помилка streaming reader: %v", err)
				}
				chunksCount++
				bytesCount += int64(len(chunk.Text))
			}
		}
		benchmarkChunkSink.chunks = chunksCount
		benchmarkChunkSink.bytes = bytesCount
	})
}

func makeBenchmarkBook(size int, seed string) string {
	var builder strings.Builder
	builder.Grow(size + len(seed))
	for builder.Len()+len(seed) <= size {
		builder.WriteString(seed)
	}
	for builder.Len() < size {
		builder.WriteByte('x')
	}
	return builder.String()
}
