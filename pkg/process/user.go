package process

import (
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

type User struct {
	Username string
	UID      int
	GID      int
	Home     string
	Shell    string
}

func (u User) Enabled() bool {
	return strings.TrimSpace(u.Username) != ""
}

func (u User) ShellPath(fallback string) string {
	if strings.TrimSpace(u.Shell) != "" {
		return u.Shell
	}
	return fallback
}

func (u User) Apply(cmd *exec.Cmd, env []string) {
	if env == nil {
		env = os.Environ()
	}

	if u.Enabled() {
		env = UpsertEnv(env, "USER", u.Username)
		env = UpsertEnv(env, "LOGNAME", u.Username)
		if u.Home != "" {
			env = UpsertEnv(env, "HOME", u.Home)
			env = UpsertEnv(env, "PWD", u.Home)
			if info, err := os.Stat(u.Home); err == nil && info.IsDir() {
				cmd.Dir = u.Home
			}
		}
		if u.Shell != "" {
			env = UpsertEnv(env, "SHELL", u.Shell)
		}
		if u.UID >= 0 {
			runtimeDir := "/run/user/" + strconv.Itoa(u.UID)
			if info, err := os.Stat(runtimeDir); err == nil && info.IsDir() {
				env = UpsertEnv(env, "XDG_RUNTIME_DIR", runtimeDir)
			}
		}
	} else if cmd.Dir == "" {
		if current, err := user.Current(); err == nil && current.HomeDir != "" {
			cmd.Dir = current.HomeDir
		}
	}

	cmd.Env = env
	u.applyCredential(cmd)
}

func (u User) Env(base []string, values map[string]string) []string {
	if base == nil {
		base = os.Environ()
	}
	for key, value := range values {
		base = UpsertEnv(base, key, value)
	}
	return base
}

func (u User) Ownable() bool {
	return u.Enabled() && u.UID >= 0 && u.GID >= 0
}

func (u User) Chown(path string) error {
	if !u.Ownable() || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, u.UID, u.GID)
}

func (u User) applyCredential(cmd *exec.Cmd) {
	if !u.Ownable() || os.Geteuid() != 0 {
		return
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: uint32(u.UID),
		Gid: uint32(u.GID),
	}
}

func UpsertEnv(env []string, key, value string) []string {
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

func UserFromStrings(username, uid, gid, home, shell string) User {
	return User{
		Username: strings.TrimSpace(username),
		UID:      atoi(uid, -1),
		GID:      atoi(gid, -1),
		Home:     strings.TrimSpace(home),
		Shell:    strings.TrimSpace(shell),
	}
}

func atoi(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}
