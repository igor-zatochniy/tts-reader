//go:build !windows

package main

import (
	"context"
	"fmt"

	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

func newSpeaker(cfg tts.Config) tts.SpeakFunc {
	return func(ctx context.Context, text string) error {
		return fmt.Errorf("Windows SAPI TTS is supported only on Windows Desktop")
	}
}

func listVoices(ctx context.Context) ([]string, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("Windows SAPI voice discovery is supported only on Windows Desktop")
}
