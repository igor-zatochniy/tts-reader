//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/chunk"
)

func TestSpeakWindowsCommandPassesTextThroughStdin(t *testing.T) {
	t.Setenv("AUDIOBOOK_TTS_TEXT_B64", "stale-text")
	t.Setenv("AUDIOBOOK_TTS_VOICE_B64", "stale-voice")

	text := strings.Repeat("😀", chunk.MaxSize) + " Український текст"
	voice := "Microsoft Irina Desktop"

	cmd := newSpeakWindowsCommand(context.Background(), text, voice)
	if cmd.Stdin == nil {
		t.Fatal("очікував stdin для PowerShell command")
	}

	stdin, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("не вдалося прочитати command stdin: %v", err)
	}
	if string(stdin) != text {
		t.Fatalf("stdin не містить оригінальний текст")
	}

	wantVoice := "AUDIOBOOK_TTS_VOICE_B64=" + base64.StdEncoding.EncodeToString([]byte(voice))
	voiceEnvCount := 0
	for _, item := range cmd.Env {
		if strings.HasPrefix(strings.ToUpper(item), "AUDIOBOOK_TTS_TEXT_B64=") {
			t.Fatalf("текст не має передаватися через environment variable")
		}
		if strings.HasPrefix(strings.ToUpper(item), "AUDIOBOOK_TTS_VOICE_B64=") {
			voiceEnvCount++
			if item != wantVoice {
				t.Fatalf("неочікуване значення voice env: %q", item)
			}
		}
	}
	if voiceEnvCount != 1 {
		t.Fatalf("очікував один AUDIOBOOK_TTS_VOICE_B64 env, отримав %d", voiceEnvCount)
	}

	script := cmd.Args[len(cmd.Args)-1]
	if strings.Contains(script, "AUDIOBOOK_TTS_TEXT_B64") {
		t.Fatalf("PowerShell script не має читати текст із environment variable")
	}
	if !strings.Contains(script, "[Console]::In.ReadToEnd()") {
		t.Fatalf("PowerShell script має читати текст зі stdin")
	}
}

func TestPowerShellCommandErrorIncludesStderr(t *testing.T) {
	err := errors.New("exit status 1")
	got := powerShellCommandError("TTS command failed", err, "System.Speech failure")
	if !errors.Is(got, err) || !strings.Contains(got.Error(), "System.Speech failure") {
		t.Fatalf("PowerShell stderr втрачено: %v", got)
	}
}

func TestListVoicesHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	startedAt := time.Now()
	_, err := listVoices(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("очікував context.Canceled, отримав %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("voice discovery надто довго обробляв скасований context: %s", elapsed)
	}
}

func TestWindowsSAPIVoiceDiscoverySmoke(t *testing.T) {
	if os.Getenv("RUN_WINDOWS_SAPI_SMOKE") != "1" {
		t.Skip("встановіть RUN_WINDOWS_SAPI_SMOKE=1 для перевірки SAPI у Windows Desktop-сесії")
	}

	if _, err := listVoices(context.Background()); err != nil {
		t.Fatalf("Windows SAPI smoke test failed: %v", err)
	}
}
