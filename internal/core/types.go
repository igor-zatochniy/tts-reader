package core

import (
	"context"
	"errors"
	"time"
)

const (
	// Позиція прогресу зберігається в байтах, бо рядки Go індексуються байтовими зміщеннями.
	PositionUnit     = "bytes (UTF-8)"
	ProgressVersion  = 2
	DefaultChunkSize = 250
	MaxChunkSize     = 10000

	PlaybackStopped  = "stopped"
	PlaybackStopping = "stopping"
	PlaybackPlaying  = "playing"
	PlaybackPaused   = "paused"
	PlaybackFinished = "finished"
	PlaybackFailed   = "failed"
)

var (
	ErrPlaybackActive       = errors.New("playback active")
	ErrPlaybackStopping     = errors.New("playback stopping")
	ErrPlaybackNotPlaying   = errors.New("playback not playing")
	ErrPlaybackNotPaused    = errors.New("playback not paused")
	ErrBookModified         = errors.New("book modified")
	ErrBookNotFound         = errors.New("book not found")
	ErrBookNotReadable      = errors.New("book not readable")
	ErrBookNotRegular       = errors.New("book must be a regular file")
	ErrPathRequired         = errors.New("path required")
	ErrBookIDRequired       = errors.New("book_id required")
	ErrCurrentByteRequired  = errors.New("current_byte required")
	ErrPositionOutsideBook  = errors.New("position outside book")
	ErrPositionInsideRune   = errors.New("position inside UTF-8 rune")
	ErrInvalidChunkSize     = errors.New("invalid chunk_size")
	ErrProgressFormat       = errors.New("unsupported progress format")
	ErrProgressBookMismatch = errors.New("progress belongs to a different book")
)

type Progress struct {
	Version         int    `json:"version"`
	LastPosition    int64  `json:"last_position"`
	PositionUnit    string `json:"position_unit"`
	BookSize        int64  `json:"book_size"`
	BookFingerprint string `json:"book_fingerprint"`
}

type Config struct {
	BookFile    string
	SaveFile    string
	StartPhrase string
	Voice       string
	ChunkSize   int
	TTSTimeout  time.Duration
}

type SpeakFunc func(ctx context.Context, text string) error
type SpeakerFactory func(cfg Config) SpeakFunc
type VoiceProvider func() ([]string, error)
type EngineFactory func(cfg Config) TTSEngine

type speakFunc = SpeakFunc
type speakerFactory = SpeakerFactory
type voiceProvider = VoiceProvider
type engineFactory = EngineFactory

const (
	defaultChunkSize = DefaultChunkSize
	maxChunkSize     = MaxChunkSize

	playbackStopped  = PlaybackStopped
	playbackStopping = PlaybackStopping
	playbackPlaying  = PlaybackPlaying
	playbackPaused   = PlaybackPaused
	playbackFinished = PlaybackFinished
	playbackFailed   = PlaybackFailed
)

type AddBookRequest struct {
	Path  string `json:"path"`
	Title string `json:"title"`
}

type StartPlaybackRequest struct {
	BookID    string `json:"book_id"`
	Voice     string `json:"voice,omitempty"`
	ChunkSize *int   `json:"chunk_size,omitempty"`
}

type SetPositionRequest struct {
	BookID      string `json:"book_id"`
	CurrentByte *int64 `json:"current_byte"`
}
