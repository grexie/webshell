package sessions

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/grexie/webshell/v2/pkg/agent"
	"github.com/grexie/webshell/v2/pkg/process"
	webshellpty "github.com/grexie/webshell/v2/pkg/pty"
	"github.com/grexie/webshell/v2/pkg/shell"
)

const historyLimit = 2 * 1024 * 1024

const SessionKindTerminal = "terminal"

var ErrNotFound = errors.New("session not found")

type Metadata struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	CreatedAt    time.Time `json:"createdAt"`
	LastActiveAt time.Time `json:"lastActiveAt"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	State        string    `json:"state"`
}

type Session struct {
	ID           string
	Name         string
	CreatedAt    time.Time
	LastActiveAt time.Time
	PTY          *os.File
	Cmd          *exec.Cmd
	Agent        *agent.Proxy
	Width        int
	Height       int

	mu        sync.Mutex
	clients   map[chan []byte]struct{}
	history   []byte
	done      chan struct{}
	exited    bool
	closeOnce sync.Once
	logger    *slog.Logger
}

type Manager struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	shellPath string
	runAs     process.User
	logger    *slog.Logger
	nextName  int
}

type Config struct {
	RunAs process.User
}

func NewManager(logger *slog.Logger) *Manager {
	return NewManagerWithConfig(Config{}, logger)
}

func NewManagerWithConfig(config Config, logger *slog.Logger) *Manager {
	shellPath := config.RunAs.ShellPath(shell.ResolveLoginShell())
	return &Manager{
		sessions:  make(map[string]*Session),
		shellPath: shellPath,
		runAs:     config.RunAs,
		logger:    logger,
		nextName:  1,
	}
}

func (m *Manager) ShellPath() string {
	return m.shellPath
}

func (m *Manager) Create(name string, cols, rows int) (*Session, error) {
	m.mu.Lock()
	if strings.TrimSpace(name) == "" {
		name = "Terminal " + strconvItoa(m.nextName)
		m.nextName++
	}
	m.mu.Unlock()

	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	id, err := randomID()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	agentProxy, err := agent.New(id, m.logger, agent.SocketOwner{
		UID:     m.runAs.UID,
		GID:     m.runAs.GID,
		Enabled: m.runAs.Ownable(),
	})
	if err != nil {
		return nil, err
	}

	cmd := shell.Command(m.shellPath, cols, rows, map[string]string{
		"SSH_AUTH_SOCK": agentProxy.SocketPath(),
	}, m.runAs)
	process, err := webshellpty.Start(cmd, cols, rows)
	if err != nil {
		_ = agentProxy.Close()
		return nil, err
	}

	session := &Session{
		ID:           id,
		Name:         strings.TrimSpace(name),
		CreatedAt:    now,
		LastActiveAt: now,
		PTY:          process.File,
		Cmd:          process.Cmd,
		Agent:        agentProxy,
		Width:        cols,
		Height:       rows,
		clients:      make(map[chan []byte]struct{}),
		done:         make(chan struct{}),
		logger:       m.logger,
	}

	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()

	go session.readLoop(func(id string) {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	})

	m.logger.Info("session created", "id", id, "name", session.Name, "shell", shell.BaseName(m.shellPath))
	return session, nil
}

func (m *Manager) List() []Metadata {
	m.mu.RLock()
	list := make([]Metadata, 0, len(m.sessions))
	for _, session := range m.sessions {
		list = append(list, session.Metadata())
	}
	m.mu.RUnlock()

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})

	return list
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()
	return session, ok
}

func (m *Manager) Rename(id, name string) (*Session, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name is required")
	}

	session, ok := m.Get(id)
	if !ok {
		return nil, ErrNotFound
	}

	session.mu.Lock()
	session.Name = name
	session.LastActiveAt = time.Now()
	session.mu.Unlock()

	return session, nil
}

func (m *Manager) Delete(id string) error {
	session, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}

	if err := session.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}

	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()

	return nil
}

func (s *Session) Metadata() Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()

	return Metadata{
		ID:           s.ID,
		Name:         s.Name,
		Kind:         SessionKindTerminal,
		CreatedAt:    s.CreatedAt,
		LastActiveAt: s.LastActiveAt,
		Width:        s.Width,
		Height:       s.Height,
		State:        "running",
	}
}

func (s *Session) Done() <-chan struct{} {
	return s.done
}

func (s *Session) Exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

func (s *Session) Subscribe() (<-chan []byte, []byte, func(), bool) {
	ch := make(chan []byte, 256)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.exited {
		return nil, nil, func() {}, false
	}

	history := append([]byte(nil), s.history...)
	s.clients[ch] = struct{}{}
	s.LastActiveAt = time.Now()

	detach := func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		if _, ok := s.clients[ch]; ok {
			delete(s.clients, ch)
			close(ch)
		}
	}

	return ch, history, detach, true
}

func (s *Session) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}

	s.mu.Lock()
	s.LastActiveAt = time.Now()
	s.mu.Unlock()

	_, err := s.PTY.Write(data)
	return err
}

func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}

	if err := webshellpty.Resize(s.PTY, cols, rows); err != nil {
		return err
	}

	s.mu.Lock()
	s.Width = cols
	s.Height = rows
	s.LastActiveAt = time.Now()
	s.mu.Unlock()

	return nil
}

func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.Cmd.Process != nil {
			_ = s.Cmd.Process.Signal(syscall.SIGHUP)
			go func() {
				select {
				case <-s.done:
				case <-time.After(2 * time.Second):
					_ = s.Cmd.Process.Kill()
				}
			}()
		}
		err = s.PTY.Close()
	})
	return err
}

func (s *Session) readLoop(onExit func(string)) {
	var waitErr error
	defer func() {
		_ = s.PTY.Close()
		if s.Agent != nil {
			_ = s.Agent.Close()
		}
		if s.Cmd.Process != nil {
			waitErr = s.Cmd.Wait()
		}

		s.finish()
		onExit(s.ID)

		if waitErr != nil {
			s.logger.Info("session exited", "id", s.ID, "error", waitErr)
			return
		}
		s.logger.Info("session exited", "id", s.ID)
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := s.PTY.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			s.recordAndBroadcast(data)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				s.logger.Debug("pty read failed", "id", s.ID, "error", err)
			}
			return
		}
	}
}

func (s *Session) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.exited {
		return
	}

	s.exited = true
	close(s.done)
	for ch := range s.clients {
		close(ch)
	}
	s.clients = nil
}

func (s *Session) recordAndBroadcast(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.history = append(s.history, data...)
	if overflow := len(s.history) - historyLimit; overflow > 0 {
		s.history = append([]byte(nil), s.history[overflow:]...)
	}

	for ch := range s.clients {
		select {
		case ch <- data:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- data:
			default:
			}
		}
	}
}

func randomID() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func strconvItoa(value int) string {
	if value <= 0 {
		return "0"
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
