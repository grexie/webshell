package transport

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	gorillawebsocket "github.com/gorilla/websocket"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	Version = 1

	writeWait = 10 * time.Second

	messageKindText   = 1
	messageKindBinary = 2

	flagCompressed = 1

	envelopeHeaderLen = 28
)

var envelopeMagic = [4]byte{'W', 'S', 'E', '1'}

type Config struct {
	Enabled              bool
	Required             bool
	CompressionEnabled   bool
	CompressionMinBytes  int
	MaxDecompressedBytes int
}

type Features struct {
	Enabled             bool   `json:"enabled"`
	Required            bool   `json:"required"`
	Cipher              string `json:"cipher"`
	KeyExchange         string `json:"keyExchange"`
	CompressionEnabled  bool   `json:"compressionEnabled"`
	CompressionCodec    string `json:"compressionCodec"`
	CompressionMinBytes int    `json:"compressionMinBytes"`
}

type Conn struct {
	raw    *gorillawebsocket.Conn
	secure *secureSession
}

type serverHello struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	PublicKey   string `json:"publicKey"`
	Cipher      string `json:"cipher"`
	KeyExchange string `json:"keyExchange"`
	Compression string `json:"compression"`
}

type clientHello struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	PublicKey string `json:"publicKey"`
}

type secureSession struct {
	raw       *gorillawebsocket.Conn
	sendAEAD  cipher.AEAD
	recvAEAD  cipher.AEAD
	config    Config
	writeMu   sync.Mutex
	sendSeq   uint64
	recvSeq   uint64
	recvReady bool
}

func NormalizeConfig(config Config) Config {
	if config.Required {
		config.Enabled = true
	}
	if config.CompressionMinBytes <= 0 {
		config.CompressionMinBytes = 256
	}
	if config.MaxDecompressedBytes <= 0 {
		config.MaxDecompressedBytes = 16 << 20
	}
	return config
}

func FeatureSet(config Config) Features {
	config = NormalizeConfig(config)
	return Features{
		Enabled:             config.Enabled,
		Required:            config.Required,
		Cipher:              "aes-256-gcm",
		KeyExchange:         "x25519",
		CompressionEnabled:  config.CompressionEnabled,
		CompressionCodec:    "gzip",
		CompressionMinBytes: config.CompressionMinBytes,
	}
}

func Accept(raw *gorillawebsocket.Conn, config Config) (*Conn, error) {
	config = NormalizeConfig(config)
	if !config.Enabled {
		return &Conn{raw: raw}, nil
	}

	privateKey := make([]byte, 32)
	if _, err := rand.Read(privateKey); err != nil {
		return nil, err
	}
	publicKey, err := curve25519.X25519(privateKey, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}

	_ = raw.SetWriteDeadline(time.Now().Add(writeWait))
	if err := raw.WriteJSON(serverHello{
		Type:        "e2ee.serverHello",
		Version:     Version,
		PublicKey:   base64.StdEncoding.EncodeToString(publicKey),
		Cipher:      "aes-256-gcm",
		KeyExchange: "x25519",
		Compression: compressionName(config),
	}); err != nil {
		return nil, err
	}

	_ = raw.SetReadDeadline(time.Now().Add(10 * time.Second))
	messageType, payload, err := raw.ReadMessage()
	if err != nil {
		return nil, err
	}
	if messageType != gorillawebsocket.TextMessage {
		return nil, errors.New("e2ee client hello must be a text message")
	}

	var hello clientHello
	if err := json.Unmarshal(payload, &hello); err != nil {
		return nil, err
	}
	if hello.Type != "e2ee.clientHello" || hello.Version != Version {
		return nil, errors.New("invalid e2ee client hello")
	}
	clientPublicKey, err := base64.StdEncoding.DecodeString(hello.PublicKey)
	if err != nil || len(clientPublicKey) != 32 {
		return nil, errors.New("invalid e2ee client public key")
	}

	sharedSecret, err := curve25519.X25519(privateKey, clientPublicKey)
	if err != nil {
		return nil, err
	}
	clientToServer, serverToClient, err := deriveKeys(sharedSecret, clientPublicKey, publicKey)
	if err != nil {
		return nil, err
	}
	recvAEAD, err := newAEAD(clientToServer)
	if err != nil {
		return nil, err
	}
	sendAEAD, err := newAEAD(serverToClient)
	if err != nil {
		return nil, err
	}

	_ = raw.SetReadDeadline(time.Time{})
	return &Conn{
		raw: raw,
		secure: &secureSession{
			raw:      raw,
			sendAEAD: sendAEAD,
			recvAEAD: recvAEAD,
			config:   config,
		},
	}, nil
}

func (c *Conn) ReadMessage() (int, []byte, error) {
	if c.secure == nil {
		return c.raw.ReadMessage()
	}
	return c.secure.ReadMessage()
}

func (c *Conn) WriteMessage(messageType int, data []byte) error {
	if c.secure == nil {
		return c.raw.WriteMessage(messageType, data)
	}
	return c.secure.WriteMessage(messageType, data)
}

func (c *Conn) WriteJSON(value any) error {
	if c.secure == nil {
		return c.raw.WriteJSON(value)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.secure.WriteMessage(gorillawebsocket.TextMessage, payload)
}

func (c *Conn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	return c.raw.WriteControl(messageType, data, deadline)
}

func (c *Conn) SetReadLimit(limit int64) {
	c.raw.SetReadLimit(limit)
}

func (c *Conn) SetReadDeadline(deadline time.Time) error {
	return c.raw.SetReadDeadline(deadline)
}

func (c *Conn) SetPongHandler(handler func(string) error) error {
	c.raw.SetPongHandler(handler)
	return nil
}

func (c *Conn) SetWriteDeadline(deadline time.Time) error {
	return c.raw.SetWriteDeadline(deadline)
}

func (c *Conn) Close() error {
	return c.raw.Close()
}

func (s *secureSession) ReadMessage() (int, []byte, error) {
	messageType, envelope, err := s.raw.ReadMessage()
	if err != nil {
		return messageType, nil, err
	}
	if messageType != gorillawebsocket.BinaryMessage {
		return messageType, nil, errors.New("encrypted websocket payload must be binary")
	}
	if len(envelope) < envelopeHeaderLen || !bytes.Equal(envelope[:4], envelopeMagic[:]) {
		return messageType, nil, errors.New("invalid encrypted websocket envelope")
	}

	flags := envelope[4]
	seq := binary.BigEndian.Uint64(envelope[8:16])
	if s.recvReady {
		if seq != s.recvSeq+1 {
			return messageType, nil, fmt.Errorf("encrypted websocket sequence out of order: got %d want %d", seq, s.recvSeq+1)
		}
	} else if seq != 0 {
		return messageType, nil, fmt.Errorf("encrypted websocket sequence out of order: got %d want 0", seq)
	}
	nonce := envelope[16:28]
	plaintext, err := s.recvAEAD.Open(nil, nonce, envelope[envelopeHeaderLen:], envelope[:envelopeHeaderLen])
	if err != nil {
		return messageType, nil, err
	}
	if len(plaintext) < 1 {
		return messageType, nil, errors.New("empty encrypted websocket plaintext")
	}

	s.recvSeq = seq
	s.recvReady = true

	kind := plaintext[0]
	payload := plaintext[1:]
	if flags&flagCompressed != 0 {
		payload, err = decompress(payload, s.config.MaxDecompressedBytes)
		if err != nil {
			return messageType, nil, err
		}
	}

	switch kind {
	case messageKindText:
		return gorillawebsocket.TextMessage, payload, nil
	case messageKindBinary:
		return gorillawebsocket.BinaryMessage, payload, nil
	default:
		return messageType, nil, errors.New("unknown encrypted websocket message kind")
	}
}

func (s *secureSession) WriteMessage(messageType int, data []byte) error {
	kind := byte(messageKindBinary)
	if messageType == gorillawebsocket.TextMessage {
		kind = messageKindText
	}

	flags := byte(0)
	payload := data
	if s.config.CompressionEnabled && messageType == gorillawebsocket.TextMessage && len(data) >= s.config.CompressionMinBytes {
		compressed, err := compress(data)
		if err != nil {
			return err
		}
		payload = compressed
		flags |= flagCompressed
	}

	plaintext := make([]byte, 1+len(payload))
	plaintext[0] = kind
	copy(plaintext[1:], payload)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	seq := s.sendSeq
	s.sendSeq++

	envelope := make([]byte, envelopeHeaderLen)
	copy(envelope[:4], envelopeMagic[:])
	envelope[4] = flags
	binary.BigEndian.PutUint64(envelope[8:16], seq)
	if _, err := rand.Read(envelope[16:28]); err != nil {
		return err
	}
	ciphertext := s.sendAEAD.Seal(nil, envelope[16:28], plaintext, envelope)

	_ = s.raw.SetWriteDeadline(time.Now().Add(writeWait))
	return s.raw.WriteMessage(gorillawebsocket.BinaryMessage, append(envelope, ciphertext...))
}

func deriveKeys(sharedSecret, clientPublicKey, serverPublicKey []byte) ([]byte, []byte, error) {
	info := make([]byte, 0, len("webshell e2ee v1")+len(clientPublicKey)+len(serverPublicKey))
	info = append(info, []byte("webshell e2ee v1")...)
	info = append(info, clientPublicKey...)
	info = append(info, serverPublicKey...)

	reader := hkdf.New(sha256.New, sharedSecret, nil, info)
	keyMaterial := make([]byte, 64)
	if _, err := io.ReadFull(reader, keyMaterial); err != nil {
		return nil, nil, err
	}
	return keyMaterial[:32], keyMaterial[32:], nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func compressionName(config Config) string {
	if config.CompressionEnabled {
		return "gzip"
	}
	return "none"
}

func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompress(data []byte, limit int) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, reader, int64(limit)+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if buf.Len() > limit {
		return nil, errors.New("encrypted websocket compressed payload exceeds limit")
	}
	return buf.Bytes(), nil
}
