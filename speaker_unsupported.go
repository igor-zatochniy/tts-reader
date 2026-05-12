//go:build !windows

package main

import (
	"context"
	"errors"
	"time"
)

func speakText(context.Context, string, string, time.Duration) error {
	return errors.New("Windows SAPI is required for audio playback")
}
