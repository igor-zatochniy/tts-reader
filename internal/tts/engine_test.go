package tts

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFunctionEngineVoicesCancelsProvider(t *testing.T) {
	started := make(chan struct{})
	factory := NewFunctionEngineFactory(
		func(Config) SpeakFunc {
			return func(context.Context, string) error { return nil }
		},
		func(ctx context.Context) ([]string, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := factory(Config{}).Voices(ctx)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("voice provider не стартував")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("очікував context.Canceled, отримав %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("voice provider проігнорував скасування context")
	}
}
