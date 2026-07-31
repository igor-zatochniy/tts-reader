//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/igor-zatochniy/tts-reader/internal/tts"
)

func newSpeaker(cfg tts.Config) tts.SpeakFunc {
	return func(ctx context.Context, text string) error {
		return speakWindows(ctx, text, cfg.Voice, cfg.TTSTimeout)
	}
}

func speakWindows(parent context.Context, text string, voice string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := newSpeakWindowsCommand(ctx, text, voice)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return powerShellCommandError(fmt.Sprintf("TTS command timed out after %s", timeout), ctx.Err(), stderr.String())
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return powerShellCommandError("TTS command failed", err, stderr.String())
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

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, powerShellCommandError("voice discovery timed out", ctx.Err(), stderr.String())
		}
		return nil, powerShellCommandError("voice discovery failed", err, stderr.String())
	}

	var voices []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		voice := strings.TrimSpace(line)
		if voice != "" {
			voices = append(voices, voice)
		}
	}
	return voices, nil
}

func powerShellCommandError(operation string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	const maxDetailBytes = 4096
	if len(detail) > maxDetailBytes {
		detail = detail[:maxDetailBytes] + "..."
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w: %s", operation, err, detail)
}
