//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func newSpeaker(cfg Config) speakFunc {
	return func(ctx context.Context, text string) error {
		return speakWindows(ctx, text, cfg.Voice, cfg.TTSTimeout)
	}
}

func speakWindows(parent context.Context, text string, voice string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := newSpeakWindowsCommand(ctx, text, voice)

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("TTS command timed out after %s", timeout)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func newSpeakWindowsCommand(ctx context.Context, text string, voice string) *exec.Cmd {
	psScript := "$ErrorActionPreference = 'Stop'; " +
		"[Console]::InputEncoding = [Text.Encoding]::UTF8; " +
		"Add-Type -AssemblyName System.Speech; " +
		"$speak = New-Object System.Speech.Synthesis.SpeechSynthesizer; " +
		"$voice64 = [Environment]::GetEnvironmentVariable('AUDIOBOOK_TTS_VOICE_B64'); " +
		"if (![string]::IsNullOrEmpty($voice64)) { " +
		"$voice = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($voice64)); " +
		"$speak.SelectVoice($voice); " +
		"}; " +
		"$rawText = [Console]::In.ReadToEnd(); " +
		"if ([string]::IsNullOrEmpty($rawText)) { exit 0 }; " +
		"$speak.Speak($rawText)"

	// Текст передаємо через stdin, щоб великі UTF-8 фрагменти не впиралися в ліміт Windows environment variable.
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdin = strings.NewReader(text)
	cmd.Env = append(cleanTTSEnvironment(os.Environ()), "AUDIOBOOK_TTS_VOICE_B64="+base64.StdEncoding.EncodeToString([]byte(voice)))
	return cmd
}

func cleanTTSEnvironment(env []string) []string {
	cleaned := make([]string, 0, len(env))
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			cleaned = append(cleaned, item)
			continue
		}
		if strings.EqualFold(key, "AUDIOBOOK_TTS_TEXT_B64") || strings.EqualFold(key, "AUDIOBOOK_TTS_VOICE_B64") {
			continue
		}
		cleaned = append(cleaned, item)
	}
	return cleaned
}

func listVoices() ([]string, error) {
	psScript := "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Speech; " +
		"$speak = New-Object System.Speech.Synthesis.SpeechSynthesizer; " +
		"$speak.GetInstalledVoices() | ForEach-Object { $_.VoiceInfo.Name }"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("voice discovery timed out")
		}
		return nil, err
	}

	var voices []string
	for _, line := range strings.Split(string(output), "\n") {
		voice := strings.TrimSpace(line)
		if voice != "" {
			voices = append(voices, voice)
		}
	}
	return voices, nil
}
