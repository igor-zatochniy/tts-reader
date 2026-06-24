package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

type Chunk struct {
	Text      string
	StartByte int64
	EndByte   int64
}

type ChunkReader interface {
	Next() (Chunk, error)
}

type chunkRune struct {
	value     rune
	size      int
	startByte int64
}

type StreamingChunkReader struct {
	reader   *bufio.Reader
	limit    int
	nextByte int64
	buffer   []chunkRune
	pending  []chunkRune
	pendingI int
	eof      bool
}

func NewStreamingChunkReader(reader io.Reader, startByte int64, limit int) (*StreamingChunkReader, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("розмір фрагмента має бути більшим за 0")
	}
	return &StreamingChunkReader{
		reader:   bufio.NewReader(reader),
		limit:    limit,
		nextByte: startByte,
		buffer:   make([]chunkRune, 0, limit),
	}, nil
}

func (r *StreamingChunkReader) Next() (Chunk, error) {
	items := r.buffer[:0]
	for len(items) < r.limit {
		if r.pendingI < len(r.pending) {
			items = append(items, r.pending[r.pendingI])
			r.pendingI++
			if r.pendingI == len(r.pending) {
				r.pending = r.pending[:0]
				r.pendingI = 0
			}
			continue
		}
		if r.eof {
			break
		}

		item, err := readChunkRune(r.reader, r.nextByte)
		if err != nil {
			if err == io.EOF {
				r.eof = true
				break
			}
			return Chunk{}, err
		}
		r.nextByte += int64(item.size)
		items = append(items, item)
	}

	if len(items) == 0 {
		return Chunk{}, io.EOF
	}

	cut := smartChunkCut(items)
	if cut < len(items) {
		r.pending = append(r.pending[:0], items[cut:]...)
		r.pendingI = 0
		items = items[:cut]
	}

	chunk := Chunk{
		Text:      chunkRunesToString(items),
		StartByte: items[0].startByte,
		EndByte:   items[len(items)-1].startByte + int64(items[len(items)-1].size),
	}
	r.buffer = items[:0]
	return chunk, nil
}

func readChunkRune(reader *bufio.Reader, startByte int64) (chunkRune, error) {
	value, size, err := reader.ReadRune()
	if err != nil {
		return chunkRune{}, err
	}
	if value == utf8.RuneError && size == 1 {
		return chunkRune{}, fmt.Errorf("книга містить некоректний UTF-8 на byte offset %d", startByte)
	}
	return chunkRune{value: value, size: size, startByte: startByte}, nil
}

func smartChunkCut(items []chunkRune) int {
	if len(items) <= 1 {
		return len(items)
	}

	minCut := len(items) / 2
	if minCut < 1 {
		minCut = 1
	}

	for i := len(items) - 1; i >= minCut; i-- {
		if isSentenceBoundary(items[i].value) {
			return i + 1
		}
	}
	for i := len(items) - 1; i >= minCut; i-- {
		if items[i].value == ' ' {
			return i + 1
		}
	}
	return len(items)
}

func isSentenceBoundary(value rune) bool {
	return value == '.' || value == '!' || value == '?' || value == '\n'
}

func chunkRunesToString(items []chunkRune) string {
	var builder strings.Builder
	totalSize := 0
	for _, item := range items {
		totalSize += item.size
	}
	builder.Grow(totalSize)
	for _, item := range items {
		builder.WriteRune(item.value)
	}
	return builder.String()
}

func findPhraseOffset(path string, phrase string) (int64, bool, error) {
	if phrase == "" {
		return 0, true, nil
	}
	if !utf8.ValidString(phrase) {
		return 0, false, fmt.Errorf("стартова фраза має некоректний UTF-8")
	}

	file, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()

	want := []rune(phrase)
	window := make([]rune, 0, len(want))
	starts := make([]int64, 0, len(want))
	reader := bufio.NewReader(file)
	pos := int64(0)

	for {
		item, err := readChunkRune(reader, pos)
		if err != nil {
			if err == io.EOF {
				return 0, false, nil
			}
			return 0, false, err
		}
		pos += int64(item.size)

		window = append(window, item.value)
		starts = append(starts, item.startByte)
		if len(window) > len(want) {
			window = window[1:]
			starts = starts[1:]
		}
		if len(window) == len(want) && equalRunes(window, want) {
			return starts[0], true, nil
		}
	}
}

func equalRunes(left []rune, right []rune) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func isFileUTF8Boundary(path string, pos int64, size int64) (bool, error) {
	if pos < 0 || pos > size {
		return false, nil
	}
	if pos == 0 || pos == size {
		return true, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	if _, err := file.Seek(pos, io.SeekStart); err != nil {
		return false, err
	}
	var buf [1]byte
	if _, err := io.ReadFull(file, buf[:]); err != nil {
		return false, err
	}
	return utf8.RuneStart(buf[0]), nil
}

func previewTextFromFile(path string, start int64, limit int) (string, error) {
	if limit <= 0 {
		return "", nil
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}

	reader := bufio.NewReader(file)
	pos := start
	var builder strings.Builder
	for i := 0; i < limit; i++ {
		item, err := readChunkRune(reader, pos)
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
		pos += int64(item.size)
		builder.Grow(item.size)
		builder.WriteRune(item.value)
	}
	return strings.ReplaceAll(builder.String(), "\n", " "), nil
}
