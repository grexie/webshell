package shell

import (
	"bufio"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/grexie/webshell/v2/pkg/process"
)

func ResolveLoginShell() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}

	if shell := passwdShell(); shell != "" {
		return shell
	}

	return "/bin/bash"
}

func Command(shellPath string, cols, rows int, extraEnv map[string]string, runAs process.User) *exec.Cmd {
	cmd := exec.Command(shellPath, "-l")
	runAs.Apply(cmd, shellEnv(cols, rows, extraEnv))

	return cmd
}

func shellEnv(cols, rows int, extraEnv map[string]string) []string {
	env := os.Environ()
	env = upsertEnv(env, "TERM", "xterm-256color")
	env = upsertEnv(env, "COLORTERM", "truecolor")
	env = upsertEnv(env, "WEBSHELL", "1")
	env = upsertEnv(env, "COLUMNS", strconvItoa(cols))
	env = upsertEnv(env, "LINES", strconvItoa(rows))
	for key, value := range extraEnv {
		env = upsertEnv(env, key, value)
	}
	return env
}

func passwdShell() string {
	current, err := user.Current()
	if err != nil || current.Uid == "" {
		return ""
	}

	file, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 7 || fields[2] != current.Uid {
			continue
		}

		shellPath := strings.TrimSpace(fields[6])
		if shellPath != "" {
			return shellPath
		}
	}

	return ""
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	entry := prefix + value

	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = entry
			return env
		}
	}

	return append(env, entry)
}

func strconvItoa(value int) string {
	if value <= 0 {
		value = 80
	}

	var b [20]byte
	pos := len(b)
	for {
		pos--
		b[pos] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return string(b[pos:])
}

func BaseName(shellPath string) string {
	name := filepath.Base(shellPath)
	return strings.TrimPrefix(name, "-")
}
