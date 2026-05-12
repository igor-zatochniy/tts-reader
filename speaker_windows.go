//go:build windows

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

func speakText(parent context.Context, text string, voice string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	script := `
Add-Type -AssemblyName System.Speech
$synth = New-Object System.Speech.Synthesis.SpeechSynthesizer
if ($env:AUDIOBOOK_TTS_VOICE) {
    $synth.SelectVoice($env:AUDIOBOOK_TTS_VOICE)
}
$rawText = [Console]::In.ReadToEnd()
$synth.Speak($rawText)
`

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Stdin = strings.NewReader(text)
	if voice != "" {
		cmd.Env = append(cmd.Environ(), "AUDIOBOOK_TTS_VOICE="+voice)
	}
	return cmd.Run()
}
