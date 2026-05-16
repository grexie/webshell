package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const audioSinkName = "webshell_sink"

type audioResult struct {
	data []byte
	err  error
}

type audioSource struct {
	cancel context.CancelFunc
	cmd    *exec.Cmd
	frames chan audioResult
}

func (s *Session) ensureAudioSink() error {
	if !s.config.Audio.Enabled {
		return nil
	}

	if _, err := exec.LookPath("pulseaudio"); err != nil {
		return commandError("find pulseaudio", err)
	}
	if _, err := exec.LookPath("pactl"); err != nil {
		return commandError("find pactl", err)
	}

	s.audioMu.Lock()
	defer s.audioMu.Unlock()

	dir, socket, configPath := s.audioPathsLocked()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return commandError("create pulseaudio runtime", err)
	}
	if err := s.config.RunAs.Chown(dir); err != nil {
		return commandError("own pulseaudio runtime", err)
	}
	if err := os.WriteFile(configPath, []byte(pulseConfig(socket)), 0o600); err != nil {
		return commandError("write pulseaudio config", err)
	}
	if err := s.config.RunAs.Chown(configPath); err != nil {
		return commandError("own pulseaudio config", err)
	}

	if s.pactlInfoLocked(socket) == nil {
		ok, err := s.audioSinkExistsLocked(socket)
		if err == nil && ok {
			s.audioReady = true
			return nil
		}
	}

	// A previous failed start can leave a stale socket at this path. The socket
	// is private to this GUI session, so removing it is safe after pactl failed.
	_ = os.Remove(socket)

	cmd := exec.Command(
		"pulseaudio",
		"-n",
		"--daemonize=yes",
		"--exit-idle-time=-1",
		"--use-pid-file=no",
		"--file="+configPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	s.prepareCommand(cmd, s.audioEnvForSocket(os.Environ(), socket))
	if err := cmd.Run(); err != nil {
		return commandErrorWithStderr("start pulseaudio", err, stderr.String())
	}

	if err := s.waitForAudioSinkLocked(socket); err != nil {
		return err
	}
	s.audioReady = true
	return nil
}

func (s *Session) newAudioSource() (*audioSource, error) {
	config := s.config.Audio
	if !config.Enabled {
		return nil, nil
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, commandError("find ffmpeg", err)
	}

	if err := s.ensureAudioSink(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	args := audioFFmpegArgs(config)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, commandError("open audio stdout", err)
	}
	cmd.Stderr = io.Discard
	s.prepareCommand(cmd, s.guiEnv())
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, commandError("start audio capture", err)
	}

	source := &audioSource{
		cancel: cancel,
		cmd:    cmd,
		frames: make(chan audioResult, 32),
	}
	go source.read(stdout)
	return source, nil
}

func audioFFmpegArgs(config AudioConfig) []string {
	channels := clamp(config.Channels, 1, 2)
	sampleRate := max(config.SampleRate, 8000)
	latencyMS := clamp(config.LatencyMS, 20, 500)
	fragmentMS := clamp(latencyMS/2, 20, 60)

	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
		"-fflags", "nobuffer",
		"-f", "pulse",
		"-fragment_size", strconv.Itoa(audioFragmentBytes(sampleRate, channels, fragmentMS)),
		"-frame_size", strconv.Itoa(audioFragmentBytes(sampleRate, channels, 10)),
		"-sample_rate", strconv.Itoa(sampleRate),
		"-channels", strconv.Itoa(channels),
		"-i", audioSinkName + ".monitor",
		"-ac", strconv.Itoa(channels),
		"-ar", strconv.Itoa(sampleRate),
		"-f", "s16le",
		"pipe:1",
	}
	return args
}

func audioFragmentBytes(sampleRate, channels, durationMS int) int {
	// PulseAudio's fragment_size is bytes, not samples. FFmpeg receives s16
	// stereo/mono here, so keep fragments small to avoid capture-side buffering
	// before PCM reaches the browser AudioWorklet.
	bytes := sampleRate * channels * 2 * durationMS / 1000
	return clamp(bytes, 512, 8192)
}

func (s *Session) stopAudio() {
	if !s.config.Audio.Enabled {
		return
	}

	s.audioMu.Lock()
	dir := s.audioRuntimeDir
	socket := s.audioSocketPath
	s.audioRuntimeDir = ""
	s.audioSocketPath = ""
	s.audioConfigPath = ""
	s.audioReady = false
	s.audioMu.Unlock()

	if socket != "" {
		cmd := exec.Command("pactl", "exit")
		s.prepareCommand(cmd, s.audioEnvForSocket(os.Environ(), socket))
		_ = cmd.Run()
	}
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

func (s *Session) envWithAudio(env []string) []string {
	if !s.config.Audio.Enabled {
		return env
	}
	return s.audioEnvForSocket(env, s.audioSocket())
}

func (s *Session) audioSocket() string {
	s.audioMu.Lock()
	_, socket, _ := s.audioPathsLocked()
	s.audioMu.Unlock()
	return socket
}

func (s *Session) audioPathsLocked() (string, string, string) {
	if s.audioRuntimeDir != "" {
		return s.audioRuntimeDir, s.audioSocketPath, s.audioConfigPath
	}

	base := os.TempDir()
	if s.config.RunAs.UID >= 0 {
		runDir := filepath.Join("/run/user", strconv.Itoa(s.config.RunAs.UID))
		if info, err := os.Stat(runDir); err == nil && info.IsDir() {
			base = runDir
		}
	}

	s.audioRuntimeDir = filepath.Join(base, "webshell-pulse-"+safePathID(s.ID))
	s.audioSocketPath = filepath.Join(s.audioRuntimeDir, "native")
	s.audioConfigPath = filepath.Join(s.audioRuntimeDir, "default.pa")
	return s.audioRuntimeDir, s.audioSocketPath, s.audioConfigPath
}

func (s *Session) audioEnvForSocket(env []string, socket string) []string {
	if socket != "" {
		env = upsertEnv(env, "PULSE_SERVER", "unix:"+socket)
	}
	env = upsertEnv(env, "PULSE_SINK", audioSinkName)
	env = upsertEnv(env, "PULSE_PROP_media.role", "production")
	return env
}

func (s *Session) waitForAudioSinkLocked(socket string) error {
	var lastErr error
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.pactlInfoLocked(socket); err != nil {
			lastErr = err
		} else if ok, err := s.audioSinkExistsLocked(socket); err != nil {
			lastErr = err
		} else if ok {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		return commandError("wait for pulseaudio sink", lastErr)
	}
	return errors.New("timed out waiting for pulseaudio sink")
}

func (s *Session) pactlInfoLocked(socket string) error {
	cmd := exec.Command("pactl", "info")
	s.prepareCommand(cmd, s.audioEnvForSocket(os.Environ(), socket))
	return cmd.Run()
}

func (s *Session) audioSinkExistsLocked(socket string) (bool, error) {
	cmd := exec.Command("pactl", "list", "short", "sinks")
	s.prepareCommand(cmd, s.audioEnvForSocket(os.Environ(), socket))
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return bytes.Contains(output, []byte(audioSinkName)), nil
}

func pulseConfig(socket string) string {
	return strings.Join([]string{
		".nofail",
		"load-module module-native-protocol-unix socket=" + socket + " auth-anonymous=1",
		"load-module module-null-sink sink_name=" + audioSinkName + " sink_properties=device.description=WebShell",
		"set-default-sink " + audioSinkName,
		"",
	}, "\n")
}

func safePathID(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "session"
	}
	return builder.String()
}

func commandErrorWithStderr(action string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return fmt.Errorf("%s: %w: %s", action, err, stderr)
	}
	return commandError(action, err)
}

func (s *audioSource) Frames() <-chan audioResult {
	return s.frames
}

func (s *audioSource) Close() {
	s.cancel()
	terminate(s.cmd)
}

func (s *audioSource) read(stdout io.Reader) {
	defer close(s.frames)

	buf := make([]byte, 4*1024)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			deliverAudioResult(s.frames, audioResult{data: chunk})
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				deliverAudioResult(s.frames, audioResult{err: fmt.Errorf("read audio stream: %w", err)})
			}
			break
		}
	}
	if err := s.cmd.Wait(); err != nil {
		deliverAudioResult(s.frames, audioResult{err: commandError("audio capture", err)})
	}
}

func deliverAudioResult(frames chan audioResult, result audioResult) {
	select {
	case frames <- result:
		return
	default:
	}

	select {
	case <-frames:
	default:
	}

	select {
	case frames <- result:
	default:
	}
}
