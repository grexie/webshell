package websocket

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gorillawebsocket "github.com/gorilla/websocket"
)

func TestWriteSessionExitSendsExitMessageBeforeClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := gorillawebsocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		writeSessionExit(conn)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := gorillawebsocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read exit message failed: %v", err)
	}
	if messageType != gorillawebsocket.TextMessage {
		t.Fatalf("message type = %d, want %d", messageType, gorillawebsocket.TextMessage)
	}

	var message serverMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatalf("decode exit message failed: %v", err)
	}
	if message.Type != "exit" {
		t.Fatalf("message type = %q, want exit", message.Type)
	}

	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected websocket close after exit message")
	}
	if !gorillawebsocket.IsCloseError(err, gorillawebsocket.CloseNormalClosure) {
		t.Fatalf("close error = %v, want normal closure", err)
	}
}
