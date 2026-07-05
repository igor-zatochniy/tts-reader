//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

func TestSpeakWindowsCommandPassesTextThroughStdin(t *testing.T) {
	t.Setenv("AUDIOBOOK_TTS_TEXT_B64", "stale-text")
	t.Setenv("AUDIOBOOK_TTS_VOICE_B64", "stale-voice")

	text := strings.Repeat("😀", maxChunkSize) + " Український текст"
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
