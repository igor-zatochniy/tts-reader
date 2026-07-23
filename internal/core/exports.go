package core

import "os"

func NewFunctionEngineFactory(makeSpeaker SpeakerFactory, voices VoiceProvider) EngineFactory {
	return newFunctionEngineFactory(makeSpeaker, voices)
}

func DefaultProgressPath(bookPath string) string {
	return defaultProgressPath(bookPath)
}

func InspectBookFile(path string) (BookFileIdentity, error) {
	return inspectBookFile(path)
}

func ProgressBook(bookPath, saveFile string, identity BookFileIdentity) Book {
	return progressBook(bookPath, saveFile, identity)
}

func ProgressForBook(book Book, pos int64) Progress {
	return progressForBook(book, pos)
}

func ValidateProgressForBook(book Book, progress Progress, currentSize int64) (int64, error) {
	return validateProgressForBook(book, progress, currentSize)
}

func ValidateStartPlaybackRequest(req StartPlaybackRequest) (int, error) {
	return validateStartPlaybackRequest(req)
}

func ValidateChunkSize(size int) error {
	return validateChunkSize(size)
}

func ValidateSetPositionRequest(req SetPositionRequest) (int64, error) {
	return validateSetPositionRequest(req)
}

func FindPhraseOffset(path string, phrase string) (int64, bool, error) {
	return findPhraseOffset(path, phrase)
}

func IsFileUTF8Boundary(path string, pos int64, size int64) (bool, error) {
	return isFileUTF8Boundary(path, pos, size)
}

func PreviewTextFromFile(path string, start int64, limit int) (string, error) {
	return previewTextFromFile(path, start, limit)
}

func SaveBookProgress(book Book, pos int64) error {
	return saveBookProgress(book, pos)
}

func WriteFileReplace(path string, data []byte, perm os.FileMode) error {
	return writeFileReplace(path, data, perm)
}

func WriteFileReplaceWith(path string, data []byte, perm os.FileMode, replace func(string, string) error) error {
	return writeFileReplaceWith(path, data, perm, replace)
}
