package transport

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gorillawebsocket "github.com/gorilla/websocket"
	"golang.org/x/crypto/curve25519"
)

func TestEncryptedTransportRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := gorillawebsocket.Upgrade(w, r, nil, 4096, 4096)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer raw.Close()

		conn, err := Accept(raw, Config{Enabled: true, Required: true})
		if err != nil {
			t.Errorf("accept failed: %v", err)
			return
		}

		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read failed: %v", err)
			return
		}
		if messageType != gorillawebsocket.TextMessage || string(payload) != "ping" {
			t.Errorf("message = (%d,%q), want text ping", messageType, payload)
			return
		}
		if err := conn.WriteMessage(gorillawebsocket.BinaryMessage, []byte("pong")); err != nil {
			t.Errorf("write failed: %v", err)
		}
	}))
	defer server.Close()

	raw, _, err := gorillawebsocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer raw.Close()

	client, err := testClientHandshake(raw)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if err := client.WriteMessage(gorillawebsocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	messageType, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if messageType != gorillawebsocket.BinaryMessage || string(payload) != "pong" {
		t.Fatalf("message = (%d,%q), want binary pong", messageType, payload)
	}
}

func testClientHandshake(raw *gorillawebsocket.Conn) (*Conn, error) {
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := raw.ReadMessage()
	if err != nil {
		return nil, err
	}
	var hello serverHello
	if err := json.Unmarshal(payload, &hello); err != nil {
		return nil, err
	}
	serverPublicKey, err := base64.StdEncoding.DecodeString(hello.PublicKey)
	if err != nil {
		return nil, err
	}
	privateKey := make([]byte, 32)
	for i := range privateKey {
		privateKey[i] = byte(i + 1)
	}
	publicKey, err := curve25519.X25519(privateKey, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	sharedSecret, err := curve25519.X25519(privateKey, serverPublicKey)
	if err != nil {
		return nil, err
	}
	clientToServer, serverToClient, err := deriveKeys(sharedSecret, publicKey, serverPublicKey)
	if err != nil {
		return nil, err
	}
	sendAEAD, err := newAEAD(clientToServer)
	if err != nil {
		return nil, err
	}
	recvAEAD, err := newAEAD(serverToClient)
	if err != nil {
		return nil, err
	}

	if err := raw.WriteJSON(clientHello{
		Type:      "e2ee.clientHello",
		Version:   Version,
		PublicKey: base64.StdEncoding.EncodeToString(publicKey),
	}); err != nil {
		return nil, err
	}
	_ = raw.SetReadDeadline(time.Time{})
	return &Conn{
		raw: raw,
		secure: &secureSession{
			raw:      raw,
			sendAEAD: sendAEAD,
			recvAEAD: recvAEAD,
			config:   NormalizeConfig(Config{Enabled: true, Required: true}),
		},
	}, nil
}
