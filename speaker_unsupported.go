//go:build !windows

package main

import "fmt"

func newSpeaker(cfg Config) speakFunc {
	return func(text string) error {
		return fmt.Errorf("Windows SAPI TTS is supported only on Windows Desktop")
	}
}

func listVoices() ([]string, error) {
	return nil, fmt.Errorf("Windows SAPI voice discovery is supported only on Windows Desktop")
}
