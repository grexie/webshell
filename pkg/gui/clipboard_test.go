package gui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grexie/webshell/v2/pkg/process"
)

func TestClipboardUsesXClip(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.txt")
	xclip := filepath.Join(dir, "xclip")
	script := `#!/bin/sh
case "$3" in
  -out)
    printf remote-clipboard
    ;;
  -in)
    cat > "$WEBSHELL_CLIPBOARD_CAPTURE"
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(xclip, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake xclip: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WEBSHELL_CLIPBOARD_CAPTURE", capture)

	session := &Session{Display: ":99"}
	text, err := session.ReadClipboard(context.Background())
	if err != nil {
		t.Fatalf("read clipboard: %v", err)
	}
	if text != "remote-clipboard" {
		t.Fatalf("clipboard text = %q, want remote-clipboard", text)
	}

	if err := session.WriteClipboard(context.Background(), "local-clipboard"); err != nil {
		t.Fatalf("write clipboard: %v", err)
	}
	data, err := waitForFile(capture, time.Second)
	if err != nil {
		t.Fatalf("read captured clipboard: %v", err)
	}
	if string(data) != "local-clipboard" {
		t.Fatalf("captured clipboard = %q, want local-clipboard", string(data))
	}
}

func TestWriteClipboardRejectsOversizedText(t *testing.T) {
	session := &Session{Display: ":99"}
	err := session.WriteClipboard(context.Background(), strings.Repeat("x", MaxClipboardBytes+1))
	if err == nil {
		t.Fatal("expected oversized clipboard error")
	}
}

func TestWriteClipboardReturnsWhileXClipOwnsSelection(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.txt")
	xclip := filepath.Join(dir, "xclip")
	script := `#!/bin/sh
cat > "$WEBSHELL_CLIPBOARD_CAPTURE"
sleep 30
`
	if err := os.WriteFile(xclip, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake xclip: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WEBSHELL_CLIPBOARD_CAPTURE", capture)

	session := &Session{Display: ":99"}
	start := time.Now()
	if err := session.WriteClipboard(context.Background(), "local-clipboard"); err != nil {
		t.Fatalf("write clipboard: %v", err)
	}
	defer session.stopClipboardOwner()

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("WriteClipboard took %s; want it to return without waiting for clipboard owner exit", elapsed)
	}
	if session.clipboardOwner == nil || session.clipboardOwner.Process == nil {
		t.Fatal("expected active clipboard owner process")
	}
	data, err := waitForFile(capture, time.Second)
	if err != nil {
		t.Fatalf("read captured clipboard: %v", err)
	}
	if string(data) != "local-clipboard" {
		t.Fatalf("captured clipboard = %q, want local-clipboard", string(data))
	}
}

func TestWriteClipboardFilesStagesFilesAndSetsGNOMETarget(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create home: %v", err)
	}

	argsPath := filepath.Join(dir, "args.txt")
	capture := filepath.Join(dir, "capture.txt")
	xclip := filepath.Join(dir, "xclip")
	script := `#!/bin/sh
printf '%s\n' "$*" > "$WEBSHELL_CLIPBOARD_ARGS"
cat > "$WEBSHELL_CLIPBOARD_CAPTURE"
sleep 30
`
	if err := os.WriteFile(xclip, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake xclip: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WEBSHELL_CLIPBOARD_ARGS", argsPath)
	t.Setenv("WEBSHELL_CLIPBOARD_CAPTURE", capture)

	session := &Session{
		ID:      "gui-test",
		Display: ":99",
		config: Config{
			RunAs: process.User{Username: "webshell", Home: home},
		},
	}
	err := session.WriteClipboardFiles(context.Background(), []ClipboardFile{
		{Name: "../example.txt", Reader: strings.NewReader("file body")},
	})
	if err != nil {
		t.Fatalf("write clipboard files: %v", err)
	}
	defer session.stopClipboardOwner()

	data, err := waitForFile(capture, time.Second)
	if err != nil {
		t.Fatalf("read captured clipboard: %v", err)
	}
	payload := string(data)
	if !strings.HasPrefix(payload, "copy\nfile://") || !strings.Contains(payload, "/example.txt") {
		t.Fatalf("clipboard file payload = %q, want GNOME copied-files URI payload", payload)
	}
	if strings.HasSuffix(payload, "\n") || strings.Contains(payload, "\n\n") {
		t.Fatalf("clipboard file payload = %q, must not contain empty lines", payload)
	}

	stagedPath := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(payload, "copy\n")), "file://")
	stagedData, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(stagedData) != "file body" {
		t.Fatalf("staged file = %q, want file body", string(stagedData))
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read xclip args: %v", err)
	}
	if !strings.Contains(string(args), "x-special/gnome-copied-files") {
		t.Fatalf("xclip args = %q, want GNOME copied-files target", string(args))
	}
}

func waitForFile(path string, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}
