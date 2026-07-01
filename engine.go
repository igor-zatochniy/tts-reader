package main

import (
	"context"
)

type Voice struct {
	Name string `json:"name"`
}

type TTSEngine interface {
	Speak(ctx context.Context, text string) error
	Voices(ctx context.Context) ([]Voice, error)
	Stop(ctx context.Context) error
}

type engineFactory func(cfg Config) TTSEngine

type functionEngine struct {
	speaker speakFunc
	voices  voiceProvider
}

func newFunctionEngineFactory(makeSpeaker speakerFactory, voices voiceProvider) engineFactory {
	return func(cfg Config) TTSEngine {
		return &functionEngine{
			speaker: makeSpeaker(cfg),
			voices:  voices,
		}
	}
}

func (e *functionEngine) Speak(ctx context.Context, text string) error {
	return e.speaker(ctx, text)
}

func (e *functionEngine) Voices(ctx context.Context) ([]Voice, error) {
	names, err := e.voices()
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
	return nil
}
