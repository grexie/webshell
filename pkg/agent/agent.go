package agent

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	gorillawebsocket "github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
)

var ErrNoBrowserAgent = errors.New("browser ssh agent is not connected")

type Proxy struct {
	sessionID  string
	socketDir  string
	socketPath string
	listener   net.Listener
	logger     *slog.Logger

	mu     sync.RWMutex
	remote *remoteClient
	closed bool
}

type SocketOwner struct {
	UID     int
	GID     int
	Enabled bool
}

type remoteClient struct {
	conn    *gorillawebsocket.Conn
	writeMu sync.Mutex

	mu      sync.RWMutex
	keys    []*sshagent.Key
	pending map[string]chan signResponse
}

type wireMessage struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId,omitempty"`
	Keys      []wireKey       `json:"keys,omitempty"`
	PublicKey string          `json:"publicKey,omitempty"`
	Data      string          `json:"data,omitempty"`
	Flags     uint32          `json:"flags,omitempty"`
	Format    string          `json:"format,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Error     string          `json:"error,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type wireKey struct {
	PublicKey string `json:"publicKey"`
	Comment   string `json:"comment,omitempty"`
}

type signResponse struct {
	format    string
	signature []byte
	err       error
}

func New(sessionID string, logger *slog.Logger, owner SocketOwner) (*Proxy, error) {
	socketDir, err := makeSocketDir(owner)
	if err != nil {
		return nil, err
	}

	socketPath := filepath.Join(socketDir, "a.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	if err := chownIfRequested(socketPath, owner); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(socketDir)
		return nil, err
	}

	proxy := &Proxy{
		sessionID:  sessionID,
		socketDir:  socketDir,
		socketPath: socketPath,
		listener:   listener,
		logger:     logger,
	}

	go proxy.acceptLoop()
	return proxy, nil
}

func makeSocketDir(owner SocketOwner) (string, error) {
	root := os.Getenv("WEBSHELL_RUNTIME_DIR")
	if root == "" {
		root = os.Getenv("XDG_RUNTIME_DIR")
	}
	if root == "" {
		root = "/tmp"
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}

	socketDir, err := os.MkdirTemp(root, "ws-a-*")
	if err != nil {
		return "", err
	}
	if err := chownIfRequested(socketDir, owner); err != nil {
		_ = os.RemoveAll(socketDir)
		return "", err
	}
	return socketDir, nil
}

func chownIfRequested(path string, owner SocketOwner) error {
	if !owner.Enabled || owner.UID < 0 || owner.GID < 0 || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, owner.UID, owner.GID)
}

func (p *Proxy) SocketPath() string {
	return p.socketPath
}

func (p *Proxy) Attach(w http.ResponseWriter, r *http.Request, checkOrigin func(*http.Request) bool) {
	upgrader := gorillawebsocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
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

	client := &remoteClient{
		conn:    conn,
		pending: make(map[string]chan signResponse),
	}

	p.mu.Lock()
	if p.remote != nil {
		p.remote.close(errors.New("browser ssh agent replaced"))
	}
	p.remote = client
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.remote == client {
			p.remote = nil
		}
		p.mu.Unlock()
		client.close(ErrNoBrowserAgent)
	}()

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var message wireMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			continue
		}

		switch message.Type {
		case "identities":
			client.setKeys(message.Keys)
		case "signature":
			client.resolveSignature(message)
		case "failure":
			client.resolveFailure(message)
		}
	}
}

func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	remote := p.remote
	p.remote = nil
	p.mu.Unlock()

	if remote != nil {
		remote.close(ErrNoBrowserAgent)
	}

	err := p.listener.Close()
	_ = os.RemoveAll(p.socketDir)
	return err
}

func (p *Proxy) List() ([]*sshagent.Key, error) {
	remote := p.currentRemote()
	if remote == nil {
		return []*sshagent.Key{}, nil
	}
	return remote.keysSnapshot(), nil
}

func (p *Proxy) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return p.SignWithFlags(key, data, 0)
}

func (p *Proxy) SignWithFlags(key ssh.PublicKey, data []byte, flags sshagent.SignatureFlags) (*ssh.Signature, error) {
	remote := p.currentRemote()
	if remote == nil {
		return nil, ErrNoBrowserAgent
	}

	response, err := remote.sign(key, data, uint32(flags))
	if err != nil {
		return nil, err
	}

	return &ssh.Signature{
		Format: response.format,
		Blob:   response.signature,
	}, nil
}

func (p *Proxy) Add(sshagent.AddedKey) error {
	return errors.New("browser ssh agent is read-only")
}

func (p *Proxy) Remove(ssh.PublicKey) error {
	return errors.New("browser ssh agent is read-only")
}

func (p *Proxy) RemoveAll() error {
	return errors.New("browser ssh agent is read-only")
}

func (p *Proxy) Lock([]byte) error {
	return errors.New("browser ssh agent locking is managed by the frontend")
}

func (p *Proxy) Unlock([]byte) error {
	return errors.New("browser ssh agent unlocking is managed by the frontend")
}

func (p *Proxy) Signers() ([]ssh.Signer, error) {
	return nil, errors.New("browser ssh agent does not expose private signers")
}

func (p *Proxy) Extension(string, []byte) ([]byte, error) {
	return nil, errors.New("browser ssh agent extension unsupported")
}

func (p *Proxy) acceptLoop() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			p.mu.RLock()
			closed := p.closed
			p.mu.RUnlock()
			if !closed {
				p.logger.Debug("ssh agent socket accept failed", "session", p.sessionID, "error", err)
			}
			return
		}

		go func() {
			if err := sshagent.ServeAgent(p, conn); err != nil {
				p.logger.Debug("ssh agent connection ended", "session", p.sessionID, "error", err)
			}
		}()
	}
}

func (p *Proxy) currentRemote() *remoteClient {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.remote
}

func (c *remoteClient) setKeys(keys []wireKey) {
	parsed := make([]*sshagent.Key, 0, len(keys))
	for _, key := range keys {
		publicKey, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(key.PublicKey))
		if err != nil {
			continue
		}
		if key.Comment != "" {
			comment = key.Comment
		}
		parsed = append(parsed, &sshagent.Key{
			Format:  publicKey.Type(),
			Blob:    publicKey.Marshal(),
			Comment: comment,
		})
	}

	c.mu.Lock()
	c.keys = parsed
	c.mu.Unlock()
}

func (c *remoteClient) keysSnapshot() []*sshagent.Key {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := make([]*sshagent.Key, 0, len(c.keys))
	for _, key := range c.keys {
		keys = append(keys, &sshagent.Key{
			Format:  key.Format,
			Blob:    append([]byte(nil), key.Blob...),
			Comment: key.Comment,
		})
	}

	return keys
}

func (c *remoteClient) sign(key ssh.PublicKey, data []byte, flags uint32) (signResponse, error) {
	requestID, err := randomID()
	if err != nil {
		return signResponse{}, err
	}

	responseCh := make(chan signResponse, 1)

	c.mu.Lock()
	c.pending[requestID] = responseCh
	c.mu.Unlock()

	message := wireMessage{
		Type:      "sign",
		RequestID: requestID,
		PublicKey: base64.StdEncoding.EncodeToString(key.Marshal()),
		Data:      base64.StdEncoding.EncodeToString(data),
		Flags:     flags,
	}

	c.writeMu.Lock()
	err = c.conn.WriteJSON(message)
	c.writeMu.Unlock()
	if err != nil {
		c.forget(requestID)
		return signResponse{}, err
	}

	select {
	case response := <-responseCh:
		if response.err != nil {
			return signResponse{}, response.err
		}
		return response, nil
	case <-time.After(45 * time.Second):
		c.forget(requestID)
		return signResponse{}, errors.New("browser ssh agent signing timed out")
	}
}

func (c *remoteClient) resolveSignature(message wireMessage) {
	signature, err := base64.StdEncoding.DecodeString(message.Signature)
	if err != nil {
		c.resolve(message.RequestID, signResponse{err: err})
		return
	}
	if message.Format == "" {
		message.Format = "ssh-ed25519"
	}

	c.resolve(message.RequestID, signResponse{
		format:    message.Format,
		signature: signature,
	})
}

func (c *remoteClient) resolveFailure(message wireMessage) {
	if message.Error == "" {
		message.Error = "browser ssh agent rejected signing request"
	}
	c.resolve(message.RequestID, signResponse{err: errors.New(message.Error)})
}

func (c *remoteClient) resolve(requestID string, response signResponse) {
	c.mu.Lock()
	responseCh, ok := c.pending[requestID]
	if ok {
		delete(c.pending, requestID)
	}
	c.mu.Unlock()

	if ok {
		responseCh <- response
		close(responseCh)
	}
}

func (c *remoteClient) forget(requestID string) {
	c.mu.Lock()
	delete(c.pending, requestID)
	c.mu.Unlock()
}

func (c *remoteClient) close(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan signResponse)
	c.mu.Unlock()

	for _, responseCh := range pending {
		responseCh <- signResponse{err: err}
		close(responseCh)
	}

	_ = c.conn.Close()
}

func randomID() (string, error) {
	var buf [18]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

func FormatAgentError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("ssh agent: %v", err)
}
