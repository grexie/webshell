package gui

import (
	"slices"
	"strconv"
	"testing"
)

func TestAudioFFmpegArgsUseLowLatencySettings(t *testing.T) {
	args := audioFFmpegArgs(AudioConfig{
		SampleRate: 48000,
		Channels:   2,
		Bitrate:    96000,
		LatencyMS:  120,
	})

	assertArgValue(t, args, "-fragment_size", strconv.Itoa(audioFragmentBytes(48000, 2, 60)))
	assertArgValue(t, args, "-frame_size", strconv.Itoa(audioFragmentBytes(48000, 2, 10)))
	assertLastArgValue(t, args, "-f", "s16le")
	assertArgValue(t, args, "-ac", "2")
	assertArgValue(t, args, "-ar", "48000")
}

func assertLastArgValue(t *testing.T, args []string, key, want string) {
	t.Helper()
	index := -1
	for i, arg := range args {
		if arg == key {
			index = i
		}
	}
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("%s not found in ffmpeg args", key)
	}
	if got := args[index+1]; got != want {
		t.Fatalf("last %s = %q, want %q", key, got, want)
	}
}

func assertArgValue(t *testing.T, args []string, key, want string) {
	t.Helper()
	index := slices.Index(args, key)
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("%s not found in ffmpeg args", key)
	}
	if got := args[index+1]; got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}
