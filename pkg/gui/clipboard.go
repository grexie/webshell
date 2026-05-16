package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const MaxClipboardBytes = 4 * 1024 * 1024
const MaxClipboardUploadBytes = 128 * 1024 * 1024

type ClipboardFile struct {
	Name   string
	Reader io.Reader
}

func (s *Session) ReadClipboard(ctx context.Context) (string, error) {
	if err := s.ensureRunning(); err != nil {
		return "", err
	}

	ctx, cancel := clipboardContext(ctx)
	defer cancel()

	if _, err := exec.LookPath("xclip"); err == nil {
		return s.readClipboardCommand(ctx, "xclip", "-selection", "clipboard", "-out")
	}
	if _, err := exec.LookPath("xsel"); err == nil {
		return s.readClipboardCommand(ctx, "xsel", "--clipboard", "--output")
	}
	return "", errors.New("remote clipboard support requires xclip or xsel")
}

func (s *Session) WriteClipboard(ctx context.Context, text string) error {
	if err := s.ensureRunning(); err != nil {
		return err
	}
	if len([]byte(text)) > MaxClipboardBytes {
		return fmt.Errorf("clipboard text exceeds %d bytes", MaxClipboardBytes)
	}

	ctx, cancel := clipboardContext(ctx)
	defer cancel()

	if _, err := exec.LookPath("xclip"); err == nil {
		return s.writeClipboardCommand(ctx, text, "xclip", "-selection", "clipboard", "-in")
	}
	if _, err := exec.LookPath("xsel"); err == nil {
		return s.writeClipboardCommand(ctx, text, "xsel", "--clipboard", "--input")
	}
	return errors.New("remote clipboard support requires xclip or xsel")
}

func (s *Session) WriteClipboardFiles(ctx context.Context, files []ClipboardFile) error {
	if err := s.ensureRunning(); err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no clipboard files supplied")
	}
	if _, err := exec.LookPath("xclip"); err != nil {
		return errors.New("remote file clipboard support requires xclip")
	}

	dir, err := s.createClipboardStagingDir()
	if err != nil {
		return err
	}

	usedNames := make(map[string]struct{}, len(files))
	roots := make(map[string]string, len(files))
	rootOrder := make([]string, 0, len(files))
	uris := make([]string, 0, len(files))
	var total int64
	for index, file := range files {
		if file.Reader == nil {
			return fmt.Errorf("clipboard file %d has no content", index+1)
		}
		parts := sanitizeClipboardRelativePath(file.Name, index+1)
		parts = uniqueClipboardRelativePath(parts, usedNames)
		if len(parts) > 1 {
			if err := s.createOwnedClipboardDirs(dir, parts[:len(parts)-1]); err != nil {
				return err
			}
		}
		path := filepath.Join(append([]string{dir}, parts...)...)
		w, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("stage clipboard file %q: %w", strings.Join(parts, "/"), err)
		}

		written, copyErr := io.Copy(w, io.LimitReader(file.Reader, MaxClipboardUploadBytes-total+1))
		closeErr := w.Close()
		total += written
		if copyErr != nil {
			return fmt.Errorf("stage clipboard file %q: %w", strings.Join(parts, "/"), copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("stage clipboard file %q: %w", strings.Join(parts, "/"), closeErr)
		}
		if total > MaxClipboardUploadBytes {
			_ = os.Remove(path)
			return fmt.Errorf("clipboard files exceed %d bytes", MaxClipboardUploadBytes)
		}
		if err := s.config.RunAs.Chown(path); err != nil {
			return fmt.Errorf("own clipboard file %q: %w", strings.Join(parts, "/"), err)
		}

		rootPath := path
		if len(parts) > 1 {
			rootPath = filepath.Join(dir, parts[0])
		}
		if _, ok := roots[rootPath]; !ok {
			roots[rootPath] = rootPath
			rootOrder = append(rootOrder, rootPath)
		}
	}
	for _, path := range rootOrder {
		uris = append(uris, (&url.URL{Scheme: "file", Path: path}).String())
	}

	// GNOME/Nautilus expects file clipboard payloads in this target shape:
	// first line is "copy" or "cut", followed by file:// URIs.
	// Nautilus rejects a trailing newline here because it treats that as an
	// empty URI line: "Nautilus Clipboard must not have empty lines."
	payload := "copy\n" + strings.Join(uris, "\n")
	ctx, cancel := clipboardContext(ctx)
	defer cancel()
	return s.writeClipboardCommand(ctx, payload, "xclip", "-selection", "clipboard", "-t", "x-special/gnome-copied-files", "-in")
}

func clipboardContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, 5*time.Second)
}

func (s *Session) ensureRunning() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateExited {
		return ErrExited
	}
	return nil
}

func (s *Session) createClipboardStagingDir() (string, error) {
	home := strings.TrimSpace(s.config.RunAs.Home)
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = userHome
		}
	}
	if home == "" {
		home = os.TempDir()
	}

	root := filepath.Join(home, ".webshell")
	clipboardRoot := filepath.Join(root, "clipboard")
	dir := filepath.Join(
		clipboardRoot,
		time.Now().UTC().Format("20060102-150405")+"-"+safeClipboardPathID(s.ID),
	)
	for _, path := range []string{root, clipboardRoot, dir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", fmt.Errorf("create clipboard staging directory: %w", err)
		}
		if err := s.config.RunAs.Chown(path); err != nil {
			return "", fmt.Errorf("own clipboard staging directory: %w", err)
		}
	}
	return dir, nil
}

func (s *Session) createOwnedClipboardDirs(root string, parts []string) error {
	path := root
	for _, part := range parts {
		path = filepath.Join(path, part)
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create clipboard staging directory: %w", err)
		}
		if err := s.config.RunAs.Chown(path); err != nil {
			return fmt.Errorf("own clipboard staging directory: %w", err)
		}
	}
	return nil
}

func sanitizeClipboardRelativePath(name string, index int) []string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	rawParts := strings.Split(name, "/")
	parts := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		part = sanitizeClipboardPathPart(part)
		if part == "" || part == "." || part == ".." {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return []string{fmt.Sprintf("clipboard-file-%d", index)}
	}
	return parts
}

func sanitizeClipboardPathPart(part string) string {
	part = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, part)
	return strings.TrimSpace(part)
}

func uniqueClipboardRelativePath(parts []string, used map[string]struct{}) []string {
	key := strings.Join(parts, "/")
	if _, ok := used[key]; !ok {
		used[key] = struct{}{}
		return parts
	}

	next := append([]string(nil), parts...)
	leaf := next[len(next)-1]
	ext := filepath.Ext(leaf)
	stem := strings.TrimSuffix(leaf, ext)
	for suffix := 2; ; suffix++ {
		next[len(next)-1] = fmt.Sprintf("%s-%d%s", stem, suffix, ext)
		candidate := strings.Join(next, "/")
		if _, ok := used[candidate]; !ok {
			used[candidate] = struct{}{}
			return next
		}
	}
}

func safeClipboardPathID(id string) string {
	id = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
	id = strings.Trim(id, "_")
	if id == "" {
		return "session"
	}
	return id
}

func (s *Session) readClipboardCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", commandError("open clipboard stdout", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	s.prepareCommand(cmd, s.guiEnvWithDisplay())

	if err := cmd.Start(); err != nil {
		return "", commandErrorWithStderr("read remote clipboard", err, stderr.String())
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, MaxClipboardBytes+1))
	waitErr := cmd.Wait()
	if readErr != nil {
		return "", commandError("read remote clipboard", readErr)
	}
	if len(data) > MaxClipboardBytes {
		return "", fmt.Errorf("remote clipboard exceeds %d bytes", MaxClipboardBytes)
	}
	if waitErr != nil {
		return "", commandErrorWithStderr("read remote clipboard", waitErr, stderr.String())
	}

	return string(data), nil
}

func (s *Session) writeClipboardCommand(ctx context.Context, text, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return commandError("open clipboard stdin", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	s.prepareCommand(cmd, s.guiEnvWithDisplay())

	if err := cmd.Start(); err != nil {
		return commandErrorWithStderr("write remote clipboard", err, stderr.String())
	}

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := io.Copy(stdin, strings.NewReader(text))
		if closeErr := stdin.Close(); writeErr == nil {
			writeErr = closeErr
		}
		writeDone <- writeErr
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			terminate(cmd)
			<-waitDone
			return commandError("write remote clipboard", err)
		}
	case <-ctx.Done():
		terminate(cmd)
		<-waitDone
		return commandError("write remote clipboard", ctx.Err())
	}

	select {
	case err := <-waitDone:
		if err != nil {
			return commandErrorWithStderr("write remote clipboard", err, stderr.String())
		}
		return nil
	default:
		s.replaceClipboardOwner(cmd, waitDone)
		return nil
	}
}

func (s *Session) replaceClipboardOwner(cmd *exec.Cmd, waitDone <-chan error) {
	s.clipboardMu.Lock()
	previous := s.clipboardOwner
	s.clipboardOwner = cmd
	s.clipboardMu.Unlock()

	terminate(previous)
	go func() {
		<-waitDone
		s.clipboardMu.Lock()
		if s.clipboardOwner == cmd {
			s.clipboardOwner = nil
		}
		s.clipboardMu.Unlock()
	}()
}

func (s *Session) stopClipboardOwner() {
	s.clipboardMu.Lock()
	cmd := s.clipboardOwner
	s.clipboardOwner = nil
	s.clipboardMu.Unlock()

	terminate(cmd)
}
