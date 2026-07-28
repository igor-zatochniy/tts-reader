package main

import (
	"io"
	"os"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/book"
	"github.com/igor-zatochniy/tts-reader/internal/chunk"
	apidto "github.com/igor-zatochniy/tts-reader/internal/httpapi"
	"github.com/igor-zatochniy/tts-reader/internal/playback"
	"github.com/igor-zatochniy/tts-reader/internal/progress"
	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

const (
	PositionUnit     = progress.Unit
	ProgressVersion  = progress.Version
	defaultChunkSize = chunk.DefaultSize
	maxChunkSize     = chunk.MaxSize

	playbackStopped  = playback.Stopped
	playbackStopping = playback.Stopping
	playbackPlaying  = playback.Playing
	playbackPaused   = playback.Paused
	playbackFinished = playback.Finished
	playbackFailed   = playback.Failed
)

var (
	ErrPlaybackActive       = playback.ErrActive
	ErrPlaybackStopping     = playback.ErrStopping
	ErrPlaybackNotPlaying   = playback.ErrNotPlaying
	ErrPlaybackNotPaused    = playback.ErrNotPaused
	ErrBookModified         = book.ErrModified
	ErrBookNotFound         = book.ErrNotFound
	ErrBookNotReadable      = book.ErrNotReadable
	ErrBookNotRegular       = book.ErrNotRegular
	ErrPathRequired         = book.ErrPathRequired
	ErrBookIDRequired       = playback.ErrBookIDRequired
	ErrCurrentByteRequired  = playback.ErrCurrentByteRequired
	ErrPositionOutsideBook  = progress.ErrPositionOutside
	ErrPositionInsideRune   = progress.ErrPositionInside
	ErrInvalidChunkSize     = chunk.ErrInvalidSize
	ErrProgressFormat       = progress.ErrFormat
	ErrProgressBookMismatch = progress.ErrBookMismatch
)

type Progress = progress.Progress
type Config = tts.Config
type speakFunc = tts.SpeakFunc
type speakerFactory = tts.SpeakerFactory
type voiceProvider = tts.VoiceProvider
type engineFactory = tts.EngineFactory
type Voice = tts.Voice
type TTSEngine = tts.Engine
type Book = book.Book
type BookFileIdentity = book.FileIdentity
type BookStore = book.BookStore
type PlaybackManager = playback.PlaybackManager
type PlaybackSnapshot = playback.PlaybackSnapshot
type PlaybackEvent = playback.PlaybackEvent
type EventBroker = playback.EventBroker
type Chunk = chunk.Chunk
type JSONProgressStore = progress.JSONProgressStore
type ProgressStore = progress.ProgressStore
type AddBookRequest = book.AddRequest
type StartPlaybackRequest = playback.StartRequest
type SetPositionRequest = playback.SetPositionRequest
type ErrorResponse = apidto.ErrorResponse
type PublicPlaybackSnapshot = apidto.PlaybackState
type PublicPlaybackEvent = apidto.PlaybackEvent
type PublicBook = apidto.Book

func NewBookStore() *BookStore {
	return book.NewStore()
}

func NewPlaybackManager(engines engineFactory, ttsTimeout time.Duration, events *EventBroker) *PlaybackManager {
	return playback.NewManager(engines, ttsTimeout, events)
}

func NewPlaybackManagerWithProgress(engines engineFactory, ttsTimeout time.Duration, events *EventBroker, progressStore ProgressStore) *PlaybackManager {
	return playback.NewManagerWithProgress(engines, ttsTimeout, events, progressStore)
}

func NewEventBroker() *EventBroker {
	return playback.NewEventBroker()
}

func NewStreamingChunkReader(reader io.Reader, startByte int64, limit int) (*chunk.StreamingReader, error) {
	return chunk.NewStreamingReader(reader, startByte, limit)
}

func newFunctionEngineFactory(makeSpeaker speakerFactory, voices voiceProvider) engineFactory {
	return tts.NewFunctionEngineFactory(makeSpeaker, voices)
}

func defaultProgressPath(bookPath string) string {
	return book.DefaultProgressPath(bookPath)
}

func inspectBookFile(path string) (BookFileIdentity, error) {
	return book.InspectFile(path)
}

func progressBook(bookPath, saveFile string, identity BookFileIdentity) Book {
	return progress.BookForProgress(bookPath, saveFile, identity)
}

func progressForBook(book Book, pos int64) Progress {
	return progress.ProgressForBook(book, pos)
}

func findPhraseOffset(path string, phrase string) (int64, bool, error) {
	return chunk.FindPhraseOffset(path, phrase)
}

func isFileUTF8Boundary(path string, pos int64, size int64) (bool, error) {
	return chunk.IsFileUTF8Boundary(path, pos, size)
}

func writeFileReplace(path string, data []byte, perm os.FileMode) error {
	return progress.WriteFileReplace(path, data, perm)
}

func writeFileReplaceWith(path string, data []byte, perm os.FileMode, replace func(string, string) error) error {
	return progress.WriteFileReplaceWith(path, data, perm, replace)
}
