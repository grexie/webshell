package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

var (
	jpegSOI = []byte{0xff, 0xd8}
	jpegEOI = []byte{0xff, 0xd9}
)

type frameResult struct {
	data      []byte
	width     int
	height    int
	err       error
	transient bool
}

type frameSource interface {
	Frames() <-chan frameResult
	Close()
}

type ffmpegFrameSource struct {
	cmd    *exec.Cmd
	frames chan frameResult
	done   chan struct{}
	once   sync.Once
	width  int
	height int
}

func newFFmpegFrameSource(s *Session, width, height, fps int) (frameSource, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, commandError("find ffmpeg", err)
	}

	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "error",
		"-f", "x11grab",
		"-draw_mouse", "1",
		"-framerate", strconv.Itoa(clamp(fps, 1, 30)),
		"-video_size", fmt.Sprintf("%dx%d", width, height),
		"-i", s.Display + ".0",
		"-an",
		"-vcodec", "mjpeg",
		"-q:v", strconv.Itoa(ffmpegJPEGQScale(s.config.Quality)),
		"-f", "image2pipe",
		"pipe:1",
	}
	cmd := exec.Command("ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, commandError("open ffmpeg stdout", err)
	}
	cmd.Stderr = io.Discard
	s.prepareCommand(cmd, envWithDisplay(s.Display))
	if err := cmd.Start(); err != nil {
		return nil, commandError("start ffmpeg framebuffer capture", err)
	}

	source := &ffmpegFrameSource{
		cmd:    cmd,
		frames: make(chan frameResult, 1),
		done:   make(chan struct{}),
		width:  width,
		height: height,
	}
	go source.read(stdout)
	return source, nil
}

func (s *ffmpegFrameSource) Frames() <-chan frameResult {
	return s.frames
}

func (s *ffmpegFrameSource) Close() {
	s.once.Do(func() {
		close(s.done)
		terminate(s.cmd)
	})
}

func (s *ffmpegFrameSource) read(stdout io.Reader) {
	defer close(s.frames)

	var pending []byte
	buf := make([]byte, 64*1024)
	var readErr error
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			pending = s.deliverCompleteJPEGs(pending)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}

	waitErr := s.cmd.Wait()
	if s.isClosed() {
		return
	}
	if readErr != nil {
		deliverFrameResult(s.frames, frameResult{
			err: fmt.Errorf("read ffmpeg framebuffer stream: %w", readErr),
		})
		return
	}
	if waitErr != nil {
		deliverFrameResult(s.frames, frameResult{
			err: commandError("ffmpeg framebuffer capture", waitErr),
		})
	}
}

func (s *ffmpegFrameSource) deliverCompleteJPEGs(pending []byte) []byte {
	for {
		start := bytes.Index(pending, jpegSOI)
		if start < 0 {
			if len(pending) > 1 {
				return pending[len(pending)-1:]
			}
			return pending
		}
		if start > 0 {
			pending = pending[start:]
		}

		end := bytes.Index(pending[2:], jpegEOI)
		if end < 0 {
			return pending
		}
		end += 4

		frame := make([]byte, end)
		copy(frame, pending[:end])
		deliverFrameResult(s.frames, frameResult{
			data:   frame,
			width:  s.width,
			height: s.height,
		})
		pending = pending[end:]
	}
}

func (s *ffmpegFrameSource) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

type importFrameSource struct {
	frames chan frameResult
	cancel context.CancelFunc
}

func newImportFrameSource(s *Session, fps int) frameSource {
	ctx, cancel := context.WithCancel(context.Background())
	source := &importFrameSource{
		frames: make(chan frameResult, 1),
		cancel: cancel,
	}

	go func() {
		defer close(source.frames)

		frameEvery := time.Second / time.Duration(clamp(fps, 1, 30))
		ticker := time.NewTicker(frameEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				captureCtx, stopCapture := context.WithTimeout(ctx, 5*time.Second)
				frame, width, height, err := s.captureFrame(captureCtx)
				stopCapture()
				if err != nil {
					deliverFrameResult(source.frames, frameResult{err: err, transient: true})
					continue
				}
				deliverFrameResult(source.frames, frameResult{
					data:   frame,
					width:  width,
					height: height,
				})
			}
		}
	}()

	return source
}

func (s *importFrameSource) Frames() <-chan frameResult {
	return s.frames
}

func (s *importFrameSource) Close() {
	s.cancel()
}

func deliverFrameResult(frames chan frameResult, result frameResult) {
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

func ffmpegJPEGQScale(quality int) int {
	quality = clamp(quality, 1, 100)
	return clamp(31-((quality-1)*29/99), 2, 31)
}
