package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMakeSocketDirUsesShortUnixSocketPath(t *testing.T) {
	socketDir, err := makeSocketDir(SocketOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)

	socketPath := filepath.Join(socketDir, "a.sock")
	if len(socketPath) > unixSocketPathBudget() {
		t.Fatalf("socket path is too long: %d bytes: %s", len(socketPath), socketPath)
	}
	if info, err := os.Stat(socketDir); err != nil {
		t.Fatal(err)
	} else if !info.IsDir() {
		t.Fatalf("socket dir is not a directory: %s", socketDir)
	}
}

func unixSocketPathBudget() int {
	if runtime.GOOS == "darwin" {
		return 104
	}
	return 108
}
