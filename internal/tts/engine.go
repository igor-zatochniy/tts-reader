package tts

import (
	"context"
	"sync"
	"time"
)

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
type VoiceProvider func(ctx context.Context) ([]string, error)
type EngineFactory func(cfg Config) Engine

type Voice struct {
	Name string `json:"name"`
}

type Engine interface {
	Speak(ctx context.Context, text string) error
	Voices(ctx context.Context) ([]Voice, error)
	Stop(ctx context.Context) error
}

type functionEngine struct {
	mu      sync.Mutex
	speaker SpeakFunc
	voices  VoiceProvider
	stop    context.CancelFunc
	done    chan struct{}
}

func NewFunctionEngineFactory(makeSpeaker SpeakerFactory, voices VoiceProvider) EngineFactory {
	return func(cfg Config) Engine {
		return &functionEngine{
			speaker: makeSpeaker(cfg),
			voices:  voices,
		}
	}
}

func (e *functionEngine) Speak(ctx context.Context, text string) error {
	speakCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	e.mu.Lock()
	e.stop = cancel
	e.done = done
	e.mu.Unlock()

	defer func() {
		cancel()
		e.mu.Lock()
		if e.done == done {
			e.stop = nil
			e.done = nil
		}
		e.mu.Unlock()
		close(done)
	}()

	return e.speaker(speakCtx, text)
}

func (e *functionEngine) Voices(ctx context.Context) ([]Voice, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	names, err := e.voices(ctx)
	if err != nil {
		return nil, err
	}

	voices := make([]Voice, 0, len(names))
	for _, name := range names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			voices = append(voices, Voice{Name: name})
		}
	}
	return voices, nil
}

func (e *functionEngine) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	e.mu.Lock()
	stop := e.stop
	done := e.done
	e.mu.Unlock()

	if stop == nil {
		return nil
	}

	stop()
	if done == nil {
		return nil
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
