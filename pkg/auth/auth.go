package auth

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	ErrChallengeNotFound = errors.New("challenge not found")
	ErrKeyNotAuthorized  = errors.New("ssh key is not authorized")
	ErrInvalidSignature  = errors.New("ssh signature is invalid")
)

type User struct {
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"keyType"`
	Comment     string `json:"comment,omitempty"`
}

type Config struct {
	AuthorizedKeysPath   string
	AuthorizedKeysInline string
	ChallengeTTL         time.Duration
	SessionTTL           time.Duration
}

type Manager struct {
	config Config

	mu         sync.Mutex
	challenges map[string]challenge
	sessions   map[string]session
}

type challenge struct {
	Nonce   []byte
	Expires time.Time
}

type session struct {
	User    User
	Expires time.Time
}

type AuthorizedKey struct {
	PublicKey   ssh.PublicKey
	Fingerprint string
	Comment     string
}

func New(config Config) *Manager {
	if config.ChallengeTTL <= 0 {
		config.ChallengeTTL = 5 * time.Minute
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = 12 * time.Hour
	}

	return &Manager{
		config:     config,
		challenges: make(map[string]challenge),
		sessions:   make(map[string]session),
	}
}

func (m *Manager) CreateChallenge() (id string, nonce []byte, expires time.Time, err error) {
	id, err = randomToken(18)
	if err != nil {
		return "", nil, time.Time{}, err
	}

	nonce = make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, time.Time{}, err
	}

	expires = time.Now().Add(m.config.ChallengeTTL)

	m.mu.Lock()
	m.pruneLocked(time.Now())
	m.challenges[id] = challenge{Nonce: nonce, Expires: expires}
	m.mu.Unlock()

	return id, nonce, expires, nil
}

func (m *Manager) Verify(challengeID, publicKeyLine string, signatureBlob []byte) (token string, authUser User, err error) {
	now := time.Now()

	m.mu.Lock()
	challenge, ok := m.challenges[challengeID]
	if ok {
		delete(m.challenges, challengeID)
	}
	m.pruneLocked(now)
	m.mu.Unlock()

	if !ok || now.After(challenge.Expires) {
		return "", User{}, ErrChallengeNotFound
	}

	publicKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(publicKeyLine)))
	if err != nil {
		return "", User{}, err
	}

	authorized, err := m.authorizedKey(publicKey)
	if err != nil {
		return "", User{}, err
	}
	if authorized == nil {
		return "", User{}, ErrKeyNotAuthorized
	}

	var signature ssh.Signature
	if err := ssh.Unmarshal(signatureBlob, &signature); err != nil {
		return "", User{}, ErrInvalidSignature
	}
	if signature.Format == "" || len(signature.Blob) == 0 {
		return "", User{}, ErrInvalidSignature
	}
	if err := publicKey.Verify(challenge.Nonce, &signature); err != nil {
		return "", User{}, ErrInvalidSignature
	}

	token, err = randomToken(32)
	if err != nil {
		return "", User{}, err
	}

	authUser = User{
		Fingerprint: authorized.Fingerprint,
		KeyType:     authorized.PublicKey.Type(),
		Comment:     authorized.Comment,
	}

	m.mu.Lock()
	m.sessions[token] = session{
		User:    authUser,
		Expires: now.Add(m.config.SessionTTL),
	}
	m.mu.Unlock()

	return token, authUser, nil
}

func (m *Manager) CurrentUser(r *http.Request) (User, bool) {
	token := bearerToken(r)
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if token == "" {
		return User{}, false
	}

	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[token]
	if !ok || now.After(session.Expires) {
		delete(m.sessions, token)
		return User{}, false
	}

	session.Expires = now.Add(m.config.SessionTTL)
	m.sessions[token] = session

	return session.User, true
}

func (m *Manager) Authenticated(r *http.Request) bool {
	_, ok := m.CurrentUser(r)
	return ok
}

func (m *Manager) Logout(r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		return
	}

	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}

func (m *Manager) AuthorizedKeys() ([]AuthorizedKey, error) {
	var keys []AuthorizedKey

	if strings.TrimSpace(m.config.AuthorizedKeysInline) != "" {
		parsed, err := parseAuthorizedKeys(strings.NewReader(m.config.AuthorizedKeysInline))
		if err != nil {
			return nil, err
		}
		keys = append(keys, parsed...)
	}

	path := m.authorizedKeysPath()
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return keys, nil
			}
			return nil, err
		}
		defer file.Close()

		parsed, err := parseAuthorizedKeys(file)
		if err != nil {
			return nil, err
		}
		keys = append(keys, parsed...)
	}

	return keys, nil
}

func (m *Manager) authorizedKey(publicKey ssh.PublicKey) (*AuthorizedKey, error) {
	fingerprint := ssh.FingerprintSHA256(publicKey)
	keys, err := m.AuthorizedKeys()
	if err != nil {
		return nil, err
	}

	for _, key := range keys {
		if key.Fingerprint == fingerprint {
			return &key, nil
		}
	}

	return nil, nil
}

func (m *Manager) authorizedKeysPath() string {
	if strings.TrimSpace(m.config.AuthorizedKeysPath) != "" {
		return m.config.AuthorizedKeysPath
	}

	current, err := user.Current()
	if err != nil || current.HomeDir == "" {
		return ""
	}

	return filepath.Join(current.HomeDir, ".ssh", "authorized_keys")
}

func (m *Manager) pruneLocked(now time.Time) {
	for id, challenge := range m.challenges {
		if now.After(challenge.Expires) {
			delete(m.challenges, id)
		}
	}
	for token, session := range m.sessions {
		if now.After(session.Expires) {
			delete(m.sessions, token)
		}
	}
}

func parseAuthorizedKeys(input interface {
	Read([]byte) (int, error)
}) ([]AuthorizedKey, error) {
	var keys []AuthorizedKey
	scanner := bufio.NewScanner(input)
	lineNumber := 0

	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		publicKey, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("authorized_keys line %d: %w", lineNumber, err)
		}

		keys = append(keys, AuthorizedKey{
			PublicKey:   publicKey,
			Fingerprint: ssh.FingerprintSHA256(publicKey),
			Comment:     comment,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return keys, nil
}

func bearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if value == "" {
		return ""
	}

	prefix := "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return ""
	}

	return strings.TrimSpace(strings.TrimPrefix(value, prefix))
}

func randomToken(byteCount int) (string, error) {
	buf := make([]byte, byteCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
