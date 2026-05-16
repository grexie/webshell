package websocket

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	gorillawebsocket "github.com/gorilla/websocket"
	"github.com/grexie/webshell/v2/pkg/sessions"
	"github.com/grexie/webshell/v2/pkg/transport"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 70 * time.Second
	pingPeriod = 25 * time.Second
)

type Handler struct {
	Sessions    *sessions.Manager
	Logger      *slog.Logger
	CheckOrigin func(r *http.Request) bool
	Transport   transport.Config
}

type clientMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

type serverMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

func (h Handler) ServeSession(w http.ResponseWriter, r *http.Request, id string) {
	session, ok := h.Sessions.Get(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	output, history, detach, alive := session.Subscribe()
	if !alive {
		http.Error(w, "session exited", http.StatusGone)
		return
	}

	upgrader := gorillawebsocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 32 * 1024,
		CheckOrigin:     h.checkOrigin,
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		detach()
		return
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() {
		doneOnce.Do(func() {
			close(done)
			detach()
			_ = conn.Close()
		})
	}

	transportConn, err := transport.Accept(conn, h.Transport)
	if err != nil {
		closeDone()
		return
	}

	go h.writePump(transportConn, output, history, session, done, closeDone)
	h.readPump(transportConn, session, closeDone)
}

func (h Handler) readPump(conn *transport.Conn, session *sessions.Session, closeDone func()) {
	defer closeDone()

	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}

		switch messageType {
		case gorillawebsocket.BinaryMessage:
			if err := session.Write(payload); err != nil {
				h.Logger.Debug("terminal binary input failed", "session", session.ID, "error", err)
				return
			}
		case gorillawebsocket.TextMessage:
			if err := h.handleClientMessage(session, payload); err != nil {
				h.Logger.Debug("terminal client message failed", "session", session.ID, "error", err)
				return
			}
		}
	}
}

func (h Handler) writePump(conn *transport.Conn, output <-chan []byte, history []byte, session *sessions.Session, done <-chan struct{}, closeDone func()) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		closeDone()
	}()

	if len(history) > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := conn.WriteJSON(serverMessage{
			Type: "history",
			Data: base64.StdEncoding.EncodeToString(history),
		}); err != nil {
			return
		}
	}

	for {
		select {
		case data, ok := <-output:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				if session.Exited() {
					writeSessionExit(conn)
				}
				return
			}
			if err := conn.WriteMessage(gorillawebsocket.BinaryMessage, data); err != nil {
				return
			}
		case <-session.Done():
			writeSessionExit(conn)
			return
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteControl(gorillawebsocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

type controlJSONWriter interface {
	SetWriteDeadline(time.Time) error
	WriteJSON(any) error
	WriteControl(int, []byte, time.Time) error
}

func writeSessionExit(conn controlJSONWriter) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = conn.WriteJSON(serverMessage{Type: "exit"})
	_ = conn.WriteControl(
		gorillawebsocket.CloseMessage,
		gorillawebsocket.FormatCloseMessage(gorillawebsocket.CloseNormalClosure, "session exited"),
		time.Now().Add(writeWait),
	)
}

func (h Handler) handleClientMessage(session *sessions.Session, payload []byte) error {
	var message clientMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return err
	}

	switch message.Type {
	case "input":
		return session.Write([]byte(message.Data))
	case "resize":
		return session.Resize(message.Cols, message.Rows)
	case "ping":
		return nil
	default:
		return nil
	}
}

func (h Handler) checkOrigin(r *http.Request) bool {
	if h.CheckOrigin == nil {
		return true
	}
	return h.CheckOrigin(r)
}
