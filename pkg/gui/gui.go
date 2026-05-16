package gui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	gorillawebsocket "github.com/gorilla/websocket"
	"github.com/grexie/webshell/v2/pkg/agent"
	guiinput "github.com/grexie/webshell/v2/pkg/gui/input"
	"github.com/grexie/webshell/v2/pkg/process"
	"github.com/grexie/webshell/v2/pkg/transport"
)

const (
	SessionKindGUI = "gui"

	stateStarting = "starting"
	stateRunning  = "running"
	stateExited   = "exited"

	writeWait  = 10 * time.Second
	pongWait   = 70 * time.Second
	pingPeriod = 25 * time.Second

	binaryFullFrameHeaderLen  = 12
	binaryRectFrameHeaderLen  = 28
	binaryAudioFrameHeaderLen = 12
)

var (
	binaryFullFrameMagic  = [4]byte{'W', 'S', 'G', 'F'}
	binaryRectFrameMagic  = [4]byte{'W', 'S', 'G', 'R'}
	binaryAudioFrameMagic = [4]byte{'W', 'S', 'A', 'U'}
)

var (
	ErrDisabled = errors.New("gui sessions are disabled")
	ErrNotFound = errors.New("gui session not found")
	ErrExited   = errors.New("gui session exited")
)

type Config struct {
	Enabled   bool
	Command   string
	XServer   string
	FPS       int
	Quality   int
	RunAs     process.User
	Width     int
	Height    int
	Depth     int
	MinWidth  int
	MinHeight int
	MaxWidth  int
	MaxHeight int
	DebugKeys bool
	Audio     AudioConfig
	Transport transport.Config
}

type AudioConfig struct {
	Enabled    bool
	SampleRate int
	Channels   int
	Codec      string
	Bitrate    int
	LatencyMS  int
}

type Metadata struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	CreatedAt    time.Time `json:"createdAt"`
	LastActiveAt time.Time `json:"lastActiveAt"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	Display      string    `json:"display,omitempty"`
	State        string    `json:"state"`
}

type Session struct {
	ID           string
	Name         string
	CreatedAt    time.Time
	LastActiveAt time.Time
	Width        int
	Height       int
	Display      string
	DisplayNum   int
	Cmd          *exec.Cmd
	XServerCmd   *exec.Cmd
	WMCmd        *exec.Cmd
	Agent        *agent.Proxy
	Keyboard     *guiinput.Keyboard

	config Config
	logger *slog.Logger
	onExit func(string)

	mu          sync.Mutex
	processMu   sync.Mutex
	audioMu     sync.Mutex
	clipboardMu sync.Mutex
	done        chan struct{}
	state       string
	generation  int
	finishOnce  sync.Once

	audioRuntimeDir string
	audioSocketPath string
	audioConfigPath string
	audioReady      bool

	clipboardOwner *exec.Cmd
	xorgConfigPath string
}

type Manager struct {
	mu          sync.RWMutex
	sessions    map[string]*Session
	config      Config
	logger      *slog.Logger
	nextName    int
	nextDisplay int
}

type clientMessage struct {
	Type      string    `json:"type"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	X         int       `json:"x,omitempty"`
	Y         int       `json:"y,omitempty"`
	Button    int       `json:"button,omitempty"`
	Down      bool      `json:"down,omitempty"`
	DeltaX    float64   `json:"deltaX,omitempty"`
	DeltaY    float64   `json:"deltaY,omitempty"`
	Key       string    `json:"key,omitempty"`
	Code      string    `json:"code,omitempty"`
	EventType string    `json:"eventType,omitempty"`
	Location  int       `json:"location,omitempty"`
	Repeat    bool      `json:"repeat,omitempty"`
	ShiftKey  bool      `json:"shiftKey,omitempty"`
	CtrlKey   bool      `json:"ctrlKey,omitempty"`
	AltKey    bool      `json:"altKey,omitempty"`
	MetaKey   bool      `json:"metaKey,omitempty"`
	Modifiers modifiers `json:"modifiers,omitempty"` // Legacy shape accepted from older frontends.
}

type modifiers = guiinput.Modifiers

type serverMessage struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Data     string `json:"data,omitempty"`
	State    string `json:"state,omitempty"`
	Message  string `json:"message,omitempty"`
}

func NewManager(config Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		sessions:    make(map[string]*Session),
		config:      normalizeConfig(config),
		logger:      logger,
		nextName:    1,
		nextDisplay: 100,
	}
}

func (m *Manager) Enabled() bool {
	return m.config.Enabled
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

func (m *Manager) Create(name string, width, height int) (*Session, error) {
	if !m.config.Enabled {
		return nil, ErrDisabled
	}

	width, height = clampSize(m.config, width, height)

	id, err := randomID()
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if strings.TrimSpace(name) == "" {
		name = "GUI " + strconv.Itoa(m.nextName)
		m.nextName++
	}
	displayNum := m.nextDisplay
	m.nextDisplay++
	m.mu.Unlock()

	now := time.Now()
	display := ":" + strconv.Itoa(displayNum)
	agentProxy, err := agent.New(id, m.logger, agent.SocketOwner{
		UID:     m.config.RunAs.UID,
		GID:     m.config.RunAs.GID,
		Enabled: m.config.RunAs.Ownable(),
	})
	if err != nil {
		return nil, err
	}

	session := &Session{
		ID:           id,
		Name:         strings.TrimSpace(name),
		CreatedAt:    now,
		LastActiveAt: now,
		Width:        width,
		Height:       height,
		Display:      display,
		DisplayNum:   displayNum,
		Agent:        agentProxy,
		Keyboard: guiinput.NewKeyboard(guiinput.XDoToolInjector{
			Display: display,
			RunAs:   m.config.RunAs,
		}, m.logger, m.config.DebugKeys),
		config:     m.config,
		logger:     m.logger,
		done:       make(chan struct{}),
		state:      stateStarting,
		generation: 1,
	}
	session.onExit = func(id string) {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
	}

	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()

	if err := session.startProcesses(width, height); err != nil {
		session.finish("start failed")
		return nil, err
	}

	m.logger.Info(
		"gui session created",
		"id", session.ID,
		"name", session.Name,
		"display", session.Display,
		"size", fmt.Sprintf("%dx%d", session.Width, session.Height),
	)
	return session, nil
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

	session.Close()

	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()

	return nil
}

func (m *Manager) ServeWebSocket(w http.ResponseWriter, r *http.Request, id string, checkOrigin func(*http.Request) bool) {
	session, ok := m.Get(id)
	if !ok {
		http.Error(w, "gui session not found", http.StatusNotFound)
		return
	}

	upgrader := gorillawebsocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 2 << 20,
		CheckOrigin: func(r *http.Request) bool {
			if checkOrigin == nil {
				return true
			}
			return checkOrigin(r)
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() {
		doneOnce.Do(func() {
			close(done)
			_ = conn.Close()
		})
	}

	transportConn, err := transport.Accept(conn, session.config.Transport)
	if err != nil {
		closeDone()
		return
	}

	go session.guiWritePump(transportConn, done, closeDone)
	session.guiReadPump(transportConn, closeDone)
}

func (s *Session) Metadata() Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()

	return Metadata{
		ID:           s.ID,
		Name:         s.Name,
		Kind:         SessionKindGUI,
		CreatedAt:    s.CreatedAt,
		LastActiveAt: s.LastActiveAt,
		Width:        s.Width,
		Height:       s.Height,
		Display:      s.Display,
		State:        s.state,
	}
}

func (s *Session) Done() <-chan struct{} {
	return s.done
}

func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) Close() {
	if s.Keyboard != nil {
		_ = s.Keyboard.ReleaseAll()
	}
	s.finish("closed")
}

func (s *Session) Resize(width, height int) error {
	width, height = clampSize(s.config, width, height)
	if width <= 0 || height <= 0 {
		return nil
	}

	s.mu.Lock()
	if s.state == stateExited {
		s.mu.Unlock()
		return ErrExited
	}
	if s.Width == width && s.Height == height {
		s.mu.Unlock()
		return nil
	}
	currentWidth := s.Width
	currentHeight := s.Height
	s.LastActiveAt = time.Now()
	s.mu.Unlock()

	if err := s.resizeWithXrandr(width, height); err != nil {
		s.logger.Warn(
			"live gui resize failed; keeping existing desktop size",
			"id", s.ID,
			"display", s.Display,
			"width", width,
			"height", height,
			"currentWidth", currentWidth,
			"currentHeight", currentHeight,
			"error", err,
		)
		// Restarting the X server is technically a resize fallback, but it
		// destroys the desktop process tree and all user-launched apps. A
		// browser resize must never be allowed to kill the session, so if live
		// RandR resize is not supported we keep the existing framebuffer and let
		// the browser scale it.
		return nil
	}

	s.mu.Lock()
	s.Width = width
	s.Height = height
	s.LastActiveAt = time.Now()
	s.mu.Unlock()

	return nil
}

func (s *Session) Mouse(x, y, button int, down bool) error {
	args := []string{"mousemove", strconv.Itoa(max(0, x)), strconv.Itoa(max(0, y))}
	if button <= 0 {
		return s.runXDoTool(args...)
	}
	button = clamp(button, 1, 7)
	action := "mouseup"
	if down {
		action = "mousedown"
	}
	args = append(args, action, strconv.Itoa(button))
	return s.runXDoTool(args...)
}

func (s *Session) Wheel(x, y int, deltaX, deltaY float64) error {
	var args []string
	args = appendWheelClicks(args, deltaY, "4", "5")
	args = appendWheelClicks(args, deltaX, "6", "7")
	if len(args) == 0 {
		return nil
	}
	return s.runXDoTool(args...)
}

func appendWheelClicks(args []string, delta float64, negativeButton, positiveButton string) []string {
	steps := wheelSteps(delta)
	if steps == 0 {
		return args
	}
	button := positiveButton
	if delta < 0 {
		button = negativeButton
	}
	return append(args, "click", "--repeat", strconv.Itoa(steps), "--delay", "0", button)
}

func wheelSteps(delta float64) int {
	absolute := math.Abs(delta)
	if absolute < 0.5 {
		return 0
	}
	return clamp(int(math.Ceil(absolute/80)), 1, 8)
}

func (s *Session) Key(message clientMessage) error {
	if s.Keyboard == nil {
		return nil
	}

	eventType := message.EventType
	if eventType == "" {
		if message.Down {
			eventType = guiinput.EventKeyDown
		} else {
			eventType = guiinput.EventKeyUp
		}
	}

	// New clients send top-level KeyboardEvent modifier booleans. The nested
	// field is kept only so older clients do not break during local reloads.
	shiftKey := message.ShiftKey || message.Modifiers.Shift
	ctrlKey := message.CtrlKey || message.Modifiers.Ctrl
	altKey := message.AltKey || message.Modifiers.Alt
	metaKey := message.MetaKey || message.Modifiers.Meta

	return s.Keyboard.Handle(guiinput.BrowserKeyEvent{
		EventType: eventType,
		Key:       message.Key,
		Code:      message.Code,
		Location:  message.Location,
		Repeat:    message.Repeat,
		ShiftKey:  shiftKey,
		CtrlKey:   ctrlKey,
		AltKey:    altKey,
		MetaKey:   metaKey,
	})
}

func (s *Session) captureFrame(ctx context.Context) ([]byte, int, int, error) {
	s.mu.Lock()
	width := s.Width
	height := s.Height
	state := s.state
	s.mu.Unlock()

	if state != stateRunning {
		return nil, width, height, ErrExited
	}

	cmd := exec.CommandContext(
		ctx,
		"import",
		"-display", s.Display,
		"-window", "root",
		"-quality", strconv.Itoa(s.config.Quality),
		"jpeg:-",
	)
	s.prepareCommand(cmd, envWithDisplay(s.Display))
	output, err := cmd.Output()
	if err != nil {
		return nil, width, height, commandError("capture framebuffer", err)
	}
	return output, width, height, nil
}

func (s *Session) guiReadPump(conn *transport.Conn, closeDone func()) {
	defer func() {
		if s.Keyboard != nil {
			_ = s.Keyboard.ReleaseAll()
		}
		closeDone()
	}()

	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var message clientMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			s.logger.Debug("invalid gui websocket message", "id", s.ID, "error", err)
			continue
		}

		if err := s.handleClientMessage(message); err != nil {
			s.logger.Debug("gui input failed", "id", s.ID, "type", message.Type, "error", err)
		}
	}
}

func (s *Session) guiWritePump(conn *transport.Conn, done <-chan struct{}, closeDone func()) {
	fps := clamp(s.config.FPS, 1, 30)
	frameCheckTicker := time.NewTicker(250 * time.Millisecond)
	pingTicker := time.NewTicker(pingPeriod)
	var source frameSource
	var sourceWidth int
	var sourceHeight int
	var sourceGeneration int
	encoder := newDirtyEncoder(s.config.Quality)
	var audio *audioSource
	defer func() {
		frameCheckTicker.Stop()
		pingTicker.Stop()
		if source != nil {
			source.Close()
		}
		if audio != nil {
			audio.Close()
		}
		closeDone()
	}()

	if !writeJSON(conn, serverMessage{Type: "status", State: s.State()}) {
		return
	}

	startSource := func(forceImport bool) {
		if source != nil {
			source.Close()
			source = nil
		}

		width, height, state, generation := s.frameSnapshot()
		if state != stateRunning {
			return
		}

		next, err := s.newFrameSource(width, height, fps, forceImport)
		if err != nil {
			_ = writeJSON(conn, serverMessage{
				Type:    "status",
				State:   s.State(),
				Message: err.Error(),
			})
			return
		}
		source = next
		sourceWidth = width
		sourceHeight = height
		sourceGeneration = generation
		encoder.Reset()
	}

	startSource(false)
	if s.config.Audio.Enabled {
		var err error
		audio, err = s.newAudioSource()
		if err != nil {
			_ = writeJSON(conn, serverMessage{
				Type:    "status",
				State:   s.State(),
				Message: err.Error(),
			})
		}
	}

	writeAudio := func(result audioResult, ok bool) bool {
		if !ok {
			audio = nil
			return true
		}
		if result.err != nil {
			_ = writeJSON(conn, serverMessage{
				Type:    "status",
				State:   s.State(),
				Message: result.err.Error(),
			})
			return true
		}
		return writeAudioBinaryFrame(conn, s.config.Audio, result.data)
	}

	for {
		var frames <-chan frameResult
		if source != nil {
			frames = source.Frames()
		}
		var audioFrames <-chan audioResult
		if audio != nil {
			audioFrames = audio.Frames()
		}

		// Audio is low bandwidth but very sensitive to queueing delay. Give it a
		// non-blocking priority pass before desktop frames so a burst of large
		// framebuffer updates does not starve PulseAudio/PCM chunks.
		if audioFrames != nil {
			select {
			case audioResult, ok := <-audioFrames:
				if !writeAudio(audioResult, ok) {
					return
				}
				continue
			default:
			}
		}

		select {
		case result, ok := <-frames:
			if !ok {
				source = nil
				startSource(false)
				continue
			}
			if result.err != nil {
				if errors.Is(result.err, ErrExited) {
					continue
				}
				_ = writeJSON(conn, serverMessage{
					Type:    "status",
					State:   s.State(),
					Message: result.err.Error(),
				})
				if result.transient {
					startSource(false)
				} else {
					startSource(true)
				}
				continue
			}
			updates, err := encoder.Encode(result)
			if err != nil {
				_ = writeJSON(conn, serverMessage{
					Type:    "status",
					State:   s.State(),
					Message: "encode dirty gui frame: " + err.Error(),
				})
				if !writeFullBinaryFrame(conn, result.width, result.height, result.data) {
					return
				}
				continue
			}
			for _, update := range updates {
				if update.full {
					if !writeFullBinaryFrame(conn, update.screenWidth, update.screenHeight, update.data) {
						return
					}
					continue
				}
				if !writeRectBinaryFrame(conn, update) {
					return
				}
			}
		case audioResult, ok := <-audioFrames:
			if !writeAudio(audioResult, ok) {
				return
			}
		case <-frameCheckTicker.C:
			width, height, state, generation := s.frameSnapshot()
			if state != stateRunning {
				continue
			}
			if source == nil || width != sourceWidth || height != sourceHeight || generation != sourceGeneration {
				startSource(false)
			}
		case <-s.Done():
			_ = writeJSON(conn, serverMessage{Type: "status", State: stateExited})
			_ = conn.WriteControl(
				gorillawebsocket.CloseMessage,
				gorillawebsocket.FormatCloseMessage(gorillawebsocket.CloseNormalClosure, "gui session exited"),
				time.Now().Add(writeWait),
			)
			return
		case <-pingTicker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteControl(gorillawebsocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

func (s *Session) frameSnapshot() (int, int, string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Width, s.Height, s.state, s.generation
}

func (s *Session) newFrameSource(width, height, fps int, forceImport bool) (frameSource, error) {
	if !forceImport {
		source, err := newFFmpegFrameSource(s, width, height, fps)
		if err == nil {
			return source, nil
		}
		s.logger.Warn("ffmpeg gui capture unavailable; falling back to imagemagick import", "id", s.ID, "error", err)
	}
	return newImportFrameSource(s, fps), nil
}

func writeFullBinaryFrame(conn *transport.Conn, width, height int, jpeg []byte) bool {
	payload := make([]byte, binaryFullFrameHeaderLen+len(jpeg))
	copy(payload[:4], binaryFullFrameMagic[:])
	binary.BigEndian.PutUint32(payload[4:8], uint32(width))
	binary.BigEndian.PutUint32(payload[8:12], uint32(height))
	copy(payload[binaryFullFrameHeaderLen:], jpeg)

	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	return conn.WriteMessage(gorillawebsocket.BinaryMessage, payload) == nil
}

func writeRectBinaryFrame(conn *transport.Conn, update frameUpdate) bool {
	payload := make([]byte, binaryRectFrameHeaderLen+len(update.data))
	copy(payload[:4], binaryRectFrameMagic[:])
	binary.BigEndian.PutUint32(payload[4:8], uint32(update.screenWidth))
	binary.BigEndian.PutUint32(payload[8:12], uint32(update.screenHeight))
	binary.BigEndian.PutUint32(payload[12:16], uint32(update.x))
	binary.BigEndian.PutUint32(payload[16:20], uint32(update.y))
	binary.BigEndian.PutUint32(payload[20:24], uint32(update.width))
	binary.BigEndian.PutUint32(payload[24:28], uint32(update.height))
	copy(payload[binaryRectFrameHeaderLen:], update.data)

	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	return conn.WriteMessage(gorillawebsocket.BinaryMessage, payload) == nil
}

func writeAudioBinaryFrame(conn *transport.Conn, config AudioConfig, data []byte) bool {
	payload := make([]byte, binaryAudioFrameHeaderLen+len(data))
	copy(payload[:4], binaryAudioFrameMagic[:])
	binary.BigEndian.PutUint32(payload[4:8], uint32(max(config.SampleRate, 8000)))
	binary.BigEndian.PutUint16(payload[8:10], uint16(clamp(config.Channels, 1, 2)))
	copy(payload[binaryAudioFrameHeaderLen:], data)

	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	return conn.WriteMessage(gorillawebsocket.BinaryMessage, payload) == nil
}

func (s *Session) handleClientMessage(message clientMessage) error {
	switch message.Type {
	case "resize":
		return s.Resize(message.Width, message.Height)
	case "mouse":
		return s.Mouse(message.X, message.Y, message.Button, message.Down)
	case "wheel":
		return s.Wheel(message.X, message.Y, message.DeltaX, message.DeltaY)
	case "key":
		return s.Key(message)
	case "keyReset":
		if s.Keyboard != nil {
			return s.Keyboard.ReleaseAll()
		}
		return nil
	default:
		return nil
	}
}

func (s *Session) startProcesses(width, height int) error {
	s.processMu.Lock()
	defer s.processMu.Unlock()
	return s.startProcessesLocked(width, height)
}

func (s *Session) startProcessesLocked(width, height int) error {
	s.setState(stateStarting)

	gen := s.generation
	if err := s.startDisplayServerLocked(width, height, gen); err != nil {
		return err
	}

	var wmCmd *exec.Cmd
	if shouldStartWindowManager(s.config.Command) {
		if _, err := exec.LookPath("openbox"); err == nil {
			wmCmd = exec.Command("openbox")
			s.prepareCommand(wmCmd, s.guiEnvWithDisplay())
			if err := wmCmd.Start(); err == nil {
				s.WMCmd = wmCmd
				go s.waitCommand(gen, "window manager", wmCmd, false)
			} else {
				s.logger.Warn("openbox could not be started", "id", s.ID, "error", commandError("start openbox", err))
			}
		}
	}

	command := strings.TrimSpace(s.config.Command)
	if command == "" {
		command = "xterm"
	}
	if err := s.ensureAudioSink(); err != nil {
		s.logger.Warn("gui audio sink unavailable", "id", s.ID, "error", err)
	}
	appCmd := exec.Command("/bin/sh", "-lc", command)
	s.prepareCommand(appCmd, s.guiEnvWithDisplay())
	if err := appCmd.Start(); err != nil {
		s.killProcessesLocked()
		return commandError("start gui command", err)
	}
	s.Cmd = appCmd
	go s.waitCommand(gen, "gui command", appCmd, true)

	s.mu.Lock()
	s.Width = width
	s.Height = height
	s.LastActiveAt = time.Now()
	s.state = stateRunning
	s.mu.Unlock()

	return nil
}

func (s *Session) startDisplayServerLocked(width, height, generation int) error {
	preferred := strings.ToLower(strings.TrimSpace(s.config.XServer))
	if preferred == "" {
		preferred = "auto"
	}

	var lastErr error
	candidates := make([]string, 0, 2)
	switch preferred {
	case "xorg", "xorg-dummy", "dummy":
		candidates = append(candidates, "xorg")
	case "xvfb":
		candidates = append(candidates, "xvfb")
	default:
		candidates = append(candidates, "xorg", "xvfb")
	}

	for _, candidate := range candidates {
		cmd, name, err := s.displayServerCommand(candidate, width, height)
		if err != nil {
			lastErr = err
			if candidate != "xvfb" {
				s.logger.Warn("gui display server unavailable", "id", s.ID, "server", candidate, "error", err)
			}
			continue
		}
		if err := cmd.Start(); err != nil {
			lastErr = commandError("start "+name, err)
			if candidate != "xvfb" {
				s.logger.Warn("gui display server could not be started", "id", s.ID, "server", candidate, "error", lastErr)
			}
			continue
		}

		s.XServerCmd = cmd
		if err := s.waitForDisplay(); err != nil {
			lastErr = err
			s.logger.Warn("gui display server did not become ready", "id", s.ID, "server", candidate, "error", err)
			terminate(cmd)
			_ = cmd.Wait()
			s.XServerCmd = nil
			s.waitForDisplayDown(2 * time.Second)
			continue
		}

		go s.waitCommand(generation, name, cmd, true)
		s.logger.Info("gui display server started", "id", s.ID, "server", name, "display", s.Display)
		return nil
	}

	if lastErr != nil {
		return lastErr
	}
	return errors.New("no gui display server candidates configured")
}

func (s *Session) displayServerCommand(server string, width, height int) (*exec.Cmd, string, error) {
	if server == "xorg" {
		return s.xorgDummyCommand(width, height)
	}
	return s.xvfbCommand(width, height)
}

func (s *Session) xvfbCommand(width, height int) (*exec.Cmd, string, error) {
	if _, err := exec.LookPath("Xvfb"); err != nil {
		return nil, "xvfb", commandError("find Xvfb", err)
	}
	screen := fmt.Sprintf("%dx%dx%d", width, height, s.config.Depth)
	cmd := exec.Command("Xvfb", s.Display, "-screen", "0", screen, "-nolisten", "tcp", "-ac")
	s.prepareCommand(cmd, os.Environ())
	return cmd, "xvfb", nil
}

func (s *Session) xorgDummyCommand(width, height int) (*exec.Cmd, string, error) {
	if _, err := exec.LookPath("Xorg"); err != nil {
		return nil, "xorg dummy", commandError("find Xorg", err)
	}
	configPath, err := s.writeXorgDummyConfig(width, height)
	if err != nil {
		return nil, "xorg dummy", err
	}
	logPath := filepath.Join(os.TempDir(), fmt.Sprintf("webshell-xorg-%d.log", s.DisplayNum))
	cmd := exec.Command(
		"Xorg",
		s.Display,
		"-config", configPath,
		"-noreset",
		"-nolisten", "tcp",
		"-ac",
		"-logfile", logPath,
		"-verbose", "0",
	)
	s.prepareCommand(cmd, os.Environ())
	return cmd, "xorg dummy", nil
}

func (s *Session) writeXorgDummyConfig(width, height int) (string, error) {
	mode := newXrandrMode(width, height)
	maxWidth := max(width, s.config.MaxWidth)
	maxHeight := max(height, s.config.MaxHeight)
	path := filepath.Join(os.TempDir(), fmt.Sprintf("webshell-xorg-%d.conf", s.DisplayNum))
	config := fmt.Sprintf(`Section "ServerFlags"
    Option "AutoAddDevices" "false"
    Option "AllowMouseOpenFail" "true"
    Option "DontVTSwitch" "true"
    Option "PciForceNone" "true"
EndSection

Section "Device"
    Identifier "WebShellDummyDevice"
    Driver "dummy"
    Option "SWCursor" "true"
    VideoRam 262144
EndSection

Section "Monitor"
    Identifier "WebShellDummyMonitor"
    HorizSync 5.0 - 1000.0
    VertRefresh 5.0 - 200.0
    %s
    Option "PreferredMode" "%s"
EndSection

Section "Screen"
    Identifier "WebShellDummyScreen"
    Device "WebShellDummyDevice"
    Monitor "WebShellDummyMonitor"
    DefaultDepth %d
    SubSection "Display"
        Depth %d
        Virtual %d %d
        Modes "%s"
    EndSubSection
EndSection

Section "ServerLayout"
    Identifier "WebShellDummyLayout"
    Screen "WebShellDummyScreen"
EndSection
`, mode.configLine(), mode.Name, s.config.Depth, s.config.Depth, maxWidth, maxHeight, mode.Name)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		return "", commandError("write Xorg dummy config", err)
	}
	s.xorgConfigPath = path
	return path, nil
}

func (s *Session) waitForDisplay() error {
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		cmd := exec.Command("xrandr", "--display", s.Display, "--query")
		s.prepareCommand(cmd, envWithDisplay(s.Display))
		if err := cmd.Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-s.Done():
			return ErrExited
		case <-time.After(75 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return commandError("wait for X display", lastErr)
	}
	return errors.New("timed out waiting for X display")
}

func (s *Session) waitCommand(generation int, name string, cmd *exec.Cmd, finishOnExit bool) {
	err := cmd.Wait()

	s.mu.Lock()
	current := s.generation == generation && s.state != stateExited
	s.mu.Unlock()
	if !current {
		return
	}

	if err != nil {
		s.logger.Info("gui process exited", "id", s.ID, "process", name, "error", err)
	} else {
		s.logger.Info("gui process exited", "id", s.ID, "process", name)
	}

	if finishOnExit {
		s.finish(name + " exited")
	}
}

func (s *Session) resizeWithXrandr(width, height int) error {
	output, err := s.primaryXrandrOutput()
	if err != nil {
		if fbErr := s.runXrandr("--fb", fmt.Sprintf("%dx%d", width, height)); fbErr == nil {
			return nil
		}
		return err
	}

	mode := newXrandrMode(width, height)
	if err := s.runXrandr(append([]string{"--newmode"}, mode.args()...)...); err != nil && !isBenignXrandrDuplicate(err) {
		return err
	}
	if err := s.runXrandr("--addmode", output, mode.Name); err != nil && !isBenignXrandrDuplicate(err) {
		return err
	}
	if err := s.runXrandr("--fb", fmt.Sprintf("%dx%d", width, height), "--output", output, "--mode", mode.Name); err != nil {
		if fbErr := s.runXrandr("--fb", fmt.Sprintf("%dx%d", width, height)); fbErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func (s *Session) primaryXrandrOutput() (string, error) {
	cmd := exec.Command("xrandr", "--display", s.Display, "--query")
	s.prepareCommand(cmd, envWithDisplay(s.Display))
	output, err := cmd.Output()
	if err != nil {
		return "", commandError("query X outputs", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "connected" {
			return fields[0], nil
		}
	}
	return "", errors.New("no connected X output found")
}

func (s *Session) runXrandr(args ...string) error {
	fullArgs := append([]string{"--display", s.Display}, args...)
	cmd := exec.Command("xrandr", fullArgs...)
	s.prepareCommand(cmd, envWithDisplay(s.Display))
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("xrandr %s: %w: %s", strings.Join(args, " "), err, message)
		}
		return commandError("xrandr "+strings.Join(args, " "), err)
	}
	return nil
}

type xrandrMode struct {
	Name       string
	Clock      float64
	HDisplay   int
	HSyncStart int
	HSyncEnd   int
	HTotal     int
	VDisplay   int
	VSyncStart int
	VSyncEnd   int
	VTotal     int
}

func newXrandrMode(width, height int) xrandrMode {
	width = max(1, width)
	height = max(1, height)
	hFront := alignTo(max(16, width/32), 8)
	hSync := alignTo(max(32, width/16), 8)
	hBack := alignTo(max(48, width/16), 8)
	hTotal := width + hFront + hSync + hBack
	vFront := 3
	vSync := 5
	vBack := max(23, height/30)
	vTotal := height + vFront + vSync + vBack
	clock := float64(hTotal*vTotal*60) / 1_000_000
	return xrandrMode{
		Name:       fmt.Sprintf("webshell-%dx%d", width, height),
		Clock:      clock,
		HDisplay:   width,
		HSyncStart: width + hFront,
		HSyncEnd:   width + hFront + hSync,
		HTotal:     hTotal,
		VDisplay:   height,
		VSyncStart: height + vFront,
		VSyncEnd:   height + vFront + vSync,
		VTotal:     vTotal,
	}
}

func (m xrandrMode) args() []string {
	return []string{
		m.Name,
		fmt.Sprintf("%.2f", m.Clock),
		strconv.Itoa(m.HDisplay),
		strconv.Itoa(m.HSyncStart),
		strconv.Itoa(m.HSyncEnd),
		strconv.Itoa(m.HTotal),
		strconv.Itoa(m.VDisplay),
		strconv.Itoa(m.VSyncStart),
		strconv.Itoa(m.VSyncEnd),
		strconv.Itoa(m.VTotal),
		"-hsync",
		"+vsync",
	}
}

func (m xrandrMode) configLine() string {
	return fmt.Sprintf(
		`Modeline "%s" %.2f %d %d %d %d %d %d %d %d -HSync +VSync`,
		m.Name,
		m.Clock,
		m.HDisplay,
		m.HSyncStart,
		m.HSyncEnd,
		m.HTotal,
		m.VDisplay,
		m.VSyncStart,
		m.VSyncEnd,
		m.VTotal,
	)
}

func isBenignXrandrDuplicate(err error) bool {
	if err == nil {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already exists") ||
		strings.Contains(message, "already added") ||
		strings.Contains(message, "duplicate")
}

func alignTo(value, multiple int) int {
	if multiple <= 1 {
		return value
	}
	return ((value + multiple - 1) / multiple) * multiple
}

func (s *Session) restart(width, height int) error {
	s.processMu.Lock()
	defer s.processMu.Unlock()

	s.mu.Lock()
	if s.state == stateExited {
		s.mu.Unlock()
		return ErrExited
	}
	s.generation++
	s.state = stateStarting
	s.mu.Unlock()

	s.killProcessesLocked()
	s.waitForDisplayDown(2 * time.Second)
	return s.startProcessesLocked(width, height)
}

func (s *Session) waitForDisplayDown(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("xrandr", "--display", s.Display, "--query")
		s.prepareCommand(cmd, envWithDisplay(s.Display))
		if err := cmd.Run(); err != nil {
			return
		}
		time.Sleep(75 * time.Millisecond)
	}
}

func (s *Session) finish(reason string) {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		s.state = stateExited
		s.LastActiveAt = time.Now()
		close(s.done)
		s.mu.Unlock()

		s.processMu.Lock()
		s.killProcessesLocked()
		s.processMu.Unlock()
		s.stopAudio()
		s.stopClipboardOwner()
		if s.Agent != nil {
			_ = s.Agent.Close()
		}

		if s.onExit != nil {
			s.onExit(s.ID)
		}
		s.logger.Info("gui session exited", "id", s.ID, "reason", reason)
	})
}

func (s *Session) killProcessesLocked() {
	for _, cmd := range []*exec.Cmd{s.Cmd, s.WMCmd} {
		terminate(cmd)
	}
	time.Sleep(600 * time.Millisecond)
	terminate(s.XServerCmd)
	s.Cmd = nil
	s.WMCmd = nil
	s.XServerCmd = nil
	if s.xorgConfigPath != "" {
		_ = os.Remove(s.xorgConfigPath)
		s.xorgConfigPath = ""
	}
}

func (s *Session) setState(state string) {
	s.mu.Lock()
	s.state = state
	s.LastActiveAt = time.Now()
	s.mu.Unlock()
}

func (s *Session) runXDoTool(args ...string) error {
	cmd := exec.Command("xdotool", args...)
	s.prepareCommand(cmd, envWithDisplay(s.Display))
	if err := cmd.Run(); err != nil {
		return commandError("xdotool "+strings.Join(args, " "), err)
	}

	s.mu.Lock()
	s.LastActiveAt = time.Now()
	s.mu.Unlock()
	return nil
}

func (s *Session) prepareCommand(cmd *exec.Cmd, env []string) {
	s.config.RunAs.Apply(cmd, env)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// GUI commands are often shell launchers that spawn dbus-run-session,
	// desktop daemons, window managers, and app children. Put every managed
	// command in its own process group so session shutdown can tear down the
	// whole desktop tree instead of only killing /bin/sh.
	cmd.SysProcAttr.Setpgid = true
}

func (s *Session) guiEnvWithDisplay() []string {
	env := s.envWithAudio(envWithDisplay(s.Display))
	if s.Agent != nil {
		env = upsertEnv(env, "SSH_AUTH_SOCK", s.Agent.SocketPath())
	}
	return env
}

func (s *Session) guiEnv() []string {
	return s.envWithAudio(os.Environ())
}

type jsonWebSocketWriter interface {
	SetWriteDeadline(time.Time) error
	WriteJSON(any) error
}

func writeJSON(conn jsonWebSocketWriter, message serverMessage) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	return conn.WriteJSON(message) == nil
}

func terminate(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}

	signalCommand(cmd, syscall.SIGTERM)
	go func() {
		time.Sleep(1200 * time.Millisecond)
		if cmd.ProcessState == nil {
			signalCommand(cmd, syscall.SIGKILL)
		}
	}()
}

func signalCommand(cmd *exec.Cmd, signal syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, signal); err == nil {
		return
	}
	_ = cmd.Process.Signal(signal)
}

func commandError(action string, err error) error {
	if err == nil {
		return nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		stderr := strings.TrimSpace(string(exitErr.Stderr))
		if stderr != "" {
			return fmt.Errorf("%s: %w: %s", action, err, stderr)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

func envWithDisplay(display string) []string {
	env := os.Environ()
	for i, value := range env {
		if strings.HasPrefix(value, "DISPLAY=") {
			env[i] = "DISPLAY=" + display
			return env
		}
	}
	return append(env, "DISPLAY="+display)
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, current := range env {
		if strings.HasPrefix(current, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func shouldStartWindowManager(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return true
	}
	for _, token := range []string{"webshell-gui-session", "dbus-run-session", "gnome-session", "gnome-flashback", "xfce4-session"} {
		if strings.Contains(command, token) {
			return false
		}
	}
	first := strings.Fields(command)[0]
	base := first
	if slash := strings.LastIndex(first, "/"); slash >= 0 {
		base = first[slash+1:]
	}
	return base != "openbox" && base != "openbox-session" && base != "fluxbox"
}

func normalizeConfig(config Config) Config {
	if strings.TrimSpace(config.Command) == "" {
		config.Command = "xterm"
	}
	config.XServer = strings.ToLower(strings.TrimSpace(config.XServer))
	switch config.XServer {
	case "", "auto":
		config.XServer = "auto"
	case "xorg", "xorg-dummy", "dummy":
		config.XServer = "xorg"
	case "xvfb":
		config.XServer = "xvfb"
	default:
		config.XServer = "auto"
	}
	if config.Width <= 0 {
		config.Width = 1440
	}
	if config.Height <= 0 {
		config.Height = 900
	}
	if config.Depth <= 0 {
		config.Depth = 24
	}
	config.FPS = clamp(config.FPS, 1, 30)
	config.Quality = clamp(config.Quality, 1, 100)
	if config.MinWidth <= 0 {
		config.MinWidth = 640
	}
	if config.MinHeight <= 0 {
		config.MinHeight = 480
	}
	if config.MaxWidth < config.MinWidth {
		config.MaxWidth = max(config.MinWidth, 2560)
	}
	if config.MaxHeight < config.MinHeight {
		config.MaxHeight = max(config.MinHeight, 1600)
	}
	config.Width = clamp(config.Width, config.MinWidth, config.MaxWidth)
	config.Height = clamp(config.Height, config.MinHeight, config.MaxHeight)
	config.Depth = clamp(config.Depth, 8, 32)
	if strings.TrimSpace(config.Audio.Codec) == "" {
		config.Audio.Codec = "pcm-s16le"
	}
	if config.Audio.SampleRate <= 0 {
		config.Audio.SampleRate = 48000
	}
	if config.Audio.Channels <= 0 {
		config.Audio.Channels = 2
	}
	if config.Audio.Bitrate <= 0 {
		config.Audio.Bitrate = 96000
	}
	if config.Audio.LatencyMS <= 0 {
		config.Audio.LatencyMS = 120
	}
	return config
}

func clampSize(config Config, width, height int) (int, int) {
	if width <= 0 {
		width = config.Width
	}
	if height <= 0 {
		height = config.Height
	}
	return clamp(width, config.MinWidth, config.MaxWidth), clamp(height, config.MinHeight, config.MaxHeight)
}

func randomID() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
