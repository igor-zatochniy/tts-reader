//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func newSpeaker(cfg Config) speakFunc {
	return func(text string) error {
		return speakWindows(text, cfg.Voice, cfg.TTSTimeout)
	}
}

func speakWindows(text string, voice string, timeout time.Duration) error {
	// Текст і назву голосу передаємо через base64 env vars, щоб не інтерполювати ввід у PowerShell script.
	psScript := "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Speech; " +
		"$speak = New-Object System.Speech.Synthesis.SpeechSynthesizer; " +
		"$voice64 = [Environment]::GetEnvironmentVariable('AUDIOBOOK_TTS_VOICE_B64'); " +
		"if (![string]::IsNullOrEmpty($voice64)) { " +
		"$voice = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($voice64)); " +
		"$speak.SelectVoice($voice); " +
		"}; " +
		"$text64 = [Environment]::GetEnvironmentVariable('AUDIOBOOK_TTS_TEXT_B64'); " +
		"$rawText = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($text64)); " +
		"if ([string]::IsNullOrEmpty($rawText)) { exit 0 }; " +
		"$speak.Speak($rawText)"

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Таймаут захищає CLI від зависання Windows SAPI або аудіостека.
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Env = append(
		os.Environ(),
		"AUDIOBOOK_TTS_TEXT_B64="+base64.StdEncoding.EncodeToString([]byte(text)),
		"AUDIOBOOK_TTS_VOICE_B64="+base64.StdEncoding.EncodeToString([]byte(voice)),
	)

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("TTS command timed out after %s", timeout)
		}
		return err
	}
	return nil
}
