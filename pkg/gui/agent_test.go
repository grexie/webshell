package gui

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/grexie/webshell/v2/pkg/agent"
)

func TestGUIEnvWithDisplayIncludesBrowserAgentSocket(t *testing.T) {
	runtimeDir, err := os.MkdirTemp("/tmp", "ws-gui-agent-*")
	if err != nil {
		t.Fatalf("create runtime dir: %v", err)
	}
	defer os.RemoveAll(runtimeDir)
	t.Setenv("WEBSHELL_RUNTIME_DIR", runtimeDir)

	proxy, err := agent.New("gui-agent-test", slog.New(slog.NewTextHandler(io.Discard, nil)), agent.SocketOwner{})
	if err != nil {
		t.Fatalf("create agent proxy: %v", err)
	}
	defer proxy.Close()

	session := &Session{
		Display: ":123",
		Agent:   proxy,
	}
	env := session.guiEnvWithDisplay()

	if got := envValue(env, "DISPLAY"); got != ":123" {
		t.Fatalf("DISPLAY = %q, want :123", got)
	}
	if got := envValue(env, "SSH_AUTH_SOCK"); got != proxy.SocketPath() {
		t.Fatalf("SSH_AUTH_SOCK = %q, want %q", got, proxy.SocketPath())
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}
