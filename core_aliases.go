package main

import (
	"io"
	"os"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/core"
)

const (
	PositionUnit     = core.PositionUnit
	ProgressVersion  = core.ProgressVersion
	defaultChunkSize = core.DefaultChunkSize
	maxChunkSize     = core.MaxChunkSize

	playbackStopped  = core.PlaybackStopped
	playbackStopping = core.PlaybackStopping
	playbackPlaying  = core.PlaybackPlaying
	playbackPaused   = core.PlaybackPaused
	playbackFinished = core.PlaybackFinished
	playbackFailed   = core.PlaybackFailed
)

var (
	ErrPlaybackActive       = core.ErrPlaybackActive
	ErrPlaybackStopping     = core.ErrPlaybackStopping
	ErrPlaybackNotPlaying   = core.ErrPlaybackNotPlaying
	ErrPlaybackNotPaused    = core.ErrPlaybackNotPaused
	ErrBookModified         = core.ErrBookModified
	ErrBookNotFound         = core.ErrBookNotFound
	ErrBookNotReadable      = core.ErrBookNotReadable
	ErrBookNotRegular       = core.ErrBookNotRegular
	ErrPathRequired         = core.ErrPathRequired
	ErrBookIDRequired       = core.ErrBookIDRequired
	ErrCurrentByteRequired  = core.ErrCurrentByteRequired
	ErrPositionOutsideBook  = core.ErrPositionOutsideBook
	ErrPositionInsideRune   = core.ErrPositionInsideRune
	ErrInvalidChunkSize     = core.ErrInvalidChunkSize
	ErrProgressFormat       = core.ErrProgressFormat
	ErrProgressBookMismatch = core.ErrProgressBookMismatch
)

type Progress = core.Progress
type Config = core.Config
type speakFunc = core.SpeakFunc
type speakerFactory = core.SpeakerFactory
type voiceProvider = core.VoiceProvider
type engineFactory = core.EngineFactory
type Voice = core.Voice
type TTSEngine = core.TTSEngine
type Book = core.Book
type BookFileIdentity = core.BookFileIdentity
type BookStore = core.BookStore
type PlaybackManager = core.PlaybackManager
type PlaybackSnapshot = core.PlaybackSnapshot
type PlaybackEvent = core.PlaybackEvent
type EventBroker = core.EventBroker
type Chunk = core.Chunk
type JSONProgressStore = core.JSONProgressStore
type ProgressStore = core.ProgressStore
type AddBookRequest = core.AddBookRequest
type StartPlaybackRequest = core.StartPlaybackRequest
type SetPositionRequest = core.SetPositionRequest

func NewBookStore() *BookStore {
	return core.NewBookStore()
}

func NewPlaybackManager(engines engineFactory, ttsTimeout time.Duration, events *EventBroker) *PlaybackManager {
	return core.NewPlaybackManager(engines, ttsTimeout, events)
}

func NewPlaybackManagerWithProgress(engines engineFactory, ttsTimeout time.Duration, events *EventBroker, progress ProgressStore) *PlaybackManager {
	return core.NewPlaybackManagerWithProgress(engines, ttsTimeout, events, progress)
}

func NewEventBroker() *EventBroker {
	return core.NewEventBroker()
}

func NewStreamingChunkReader(reader io.Reader, startByte int64, limit int) (*core.StreamingChunkReader, error) {
	return core.NewStreamingChunkReader(reader, startByte, limit)
}

func newFunctionEngineFactory(makeSpeaker speakerFactory, voices voiceProvider) engineFactory {
	return core.NewFunctionEngineFactory(makeSpeaker, voices)
}

func defaultProgressPath(bookPath string) string {
	return core.DefaultProgressPath(bookPath)
}

func inspectBookFile(path string) (BookFileIdentity, error) {
	return core.InspectBookFile(path)
}

func progressBook(bookPath, saveFile string, identity BookFileIdentity) Book {
	return core.ProgressBook(bookPath, saveFile, identity)
}

func progressForBook(book Book, pos int64) Progress {
	return core.ProgressForBook(book, pos)
}

func validateProgressForBook(book Book, progress Progress, currentSize int64) (int64, error) {
	return core.ValidateProgressForBook(book, progress, currentSize)
}

func validateStartPlaybackRequest(req StartPlaybackRequest) (int, error) {
	return core.ValidateStartPlaybackRequest(req)
}

func validateChunkSize(size int) error {
	return core.ValidateChunkSize(size)
}

func validateSetPositionRequest(req SetPositionRequest) (int64, error) {
	return core.ValidateSetPositionRequest(req)
}

func findPhraseOffset(path string, phrase string) (int64, bool, error) {
	return core.FindPhraseOffset(path, phrase)
}

func isFileUTF8Boundary(path string, pos int64, size int64) (bool, error) {
	return core.IsFileUTF8Boundary(path, pos, size)
}

func previewTextFromFile(path string, start int64, limit int) (string, error) {
	return core.PreviewTextFromFile(path, start, limit)
}

func progressPercent(pos int64, total int64) float64 {
	return core.ProgressPercent(pos, total)
}

func saveBookProgress(book Book, pos int64) error {
	return core.SaveBookProgress(book, pos)
}

func writeFileReplace(path string, data []byte, perm os.FileMode) error {
	return core.WriteFileReplace(path, data, perm)
}

func writeFileReplaceWith(path string, data []byte, perm os.FileMode, replace func(string, string) error) error {
	return core.WriteFileReplaceWith(path, data, perm, replace)
}
