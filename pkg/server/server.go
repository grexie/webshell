package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/grexie/webshell/v2/packages/website"
	"github.com/grexie/webshell/v2/pkg/auth"
	"github.com/grexie/webshell/v2/pkg/config"
	"github.com/grexie/webshell/v2/pkg/gui"
	"github.com/grexie/webshell/v2/pkg/sessions"
	"github.com/grexie/webshell/v2/pkg/transport"
	ws "github.com/grexie/webshell/v2/pkg/websocket"
)

const (
	sessionKindTerminal = "terminal"
	sessionKindGUI      = "gui"
)

type Server struct {
	config     config.Config
	logger     *slog.Logger
	auth       *auth.Manager
	sessions   *sessions.Manager
	gui        *gui.Manager
	httpServer *http.Server
	website    website.Website
}

func New(config config.Config, logger *slog.Logger) *Server {
	authManager := auth.New(auth.Config{
		AuthorizedKeysPath:   config.AuthorizedKeysPath,
		AuthorizedKeysInline: config.AuthorizedKeysInline,
		ChallengeTTL:         config.AuthChallengeTTL,
		SessionTTL:           config.AuthSessionTTL,
	})

	staticWebsite := mustWebsite(config.StaticDir)

	s := &Server{
		config: config,
		logger: logger,
		auth:   authManager,
		sessions: sessions.NewManagerWithConfig(sessions.Config{
			RunAs: config.RuntimeUser,
		}, logger),
		gui: gui.NewManager(gui.Config{
			Enabled:   config.GUIEnabled,
			Command:   config.GUICommand,
			XServer:   config.GUIXServer,
			FPS:       config.GUIFPS,
			Quality:   config.GUIQuality,
			RunAs:     config.RuntimeUser,
			Width:     config.GUIDefaultWidth,
			Height:    config.GUIDefaultHeight,
			Depth:     config.GUIDepth,
			MinWidth:  config.GUIMinWidth,
			MinHeight: config.GUIMinHeight,
			MaxWidth:  config.GUIMaxWidth,
			MaxHeight: config.GUIMaxHeight,
			DebugKeys: config.GUIDebugKeys,
			Audio: gui.AudioConfig{
				Enabled:    config.AudioEnabled,
				SampleRate: config.AudioSampleRate,
				Channels:   config.AudioChannels,
				Codec:      config.AudioCodec,
				Bitrate:    config.AudioBitrate,
				LatencyMS:  config.AudioLatencyMS,
			},
			Transport: config.Transport(),
		}, logger),
		website: staticWebsite,
	}

	s.httpServer = &http.Server{
		Addr:              config.Addr(),
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s
}

func (s *Server) ListenAndServe() error {
	keys, err := s.auth.AuthorizedKeys()
	if err != nil {
		s.logger.Warn("authorized_keys could not be loaded", "error", err)
	}

	s.logger.Info(
		"starting webshell",
		"addr", s.config.Addr(),
		"authorizedKeys", len(keys),
		"shell", s.sessions.ShellPath(),
		"user", s.config.RuntimeUser.Username,
		"guiEnabled", s.gui.Enabled(),
	)

	err = s.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/features", s.features)
	mux.HandleFunc("POST /api/auth/challenge", s.createChallenge)
	mux.HandleFunc("POST /api/auth/verify", s.verifyChallenge)
	mux.HandleFunc("POST /api/logout", s.requireAuth(s.logout))
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/sessions", s.requireAuth(s.listSessions))
	mux.HandleFunc("POST /api/sessions", s.requireAuth(s.createSession))
	mux.HandleFunc("PATCH /api/sessions/{id}", s.requireAuth(s.renameSession))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.requireAuth(s.deleteSession))
	mux.HandleFunc("GET /api/sessions/{id}/clipboard", s.requireAuth(s.getGUIClipboard))
	mux.HandleFunc("PUT /api/sessions/{id}/clipboard", s.requireAuth(s.setGUIClipboard))
	mux.HandleFunc("PUT /api/sessions/{id}/clipboard/files", s.requireAuth(s.setGUIClipboardFiles))
	mux.HandleFunc("GET /api/sessions/{id}/ws", s.requireAuth(s.sessionWebSocket))
	mux.HandleFunc("GET /api/sessions/{id}/gui", s.requireAuth(s.guiWebSocket))
	mux.HandleFunc("GET /api/sessions/{id}/agent", s.requireAuth(s.agentWebSocket))
	mux.Handle("/", s.website.HTTPHandler())

	return s.withCORS(mux)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) features(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"e2ee": transport.FeatureSet(s.config.Transport()),
		"audio": map[string]any{
			"enabled":    s.config.AudioEnabled,
			"codec":      s.config.AudioCodec,
			"sampleRate": s.config.AudioSampleRate,
			"channels":   s.config.AudioChannels,
			"latencyMs":  s.config.AudioLatencyMS,
		},
	})
}

func (s *Server) createChallenge(w http.ResponseWriter, _ *http.Request) {
	id, nonce, expires, err := s.auth.CreateChallenge()
	if err != nil {
		writeHTTPError(w, http.StatusInternalServerError, "challenge could not be created")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"challengeId": id,
		"nonce":       base64.StdEncoding.EncodeToString(nonce),
		"expiresAt":   expires,
	})
}

func (s *Server) verifyChallenge(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ChallengeID string `json:"challengeId"`
		PublicKey   string `json:"publicKey"`
		Signature   string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeHTTPError(w, http.StatusBadRequest, "invalid json")
		return
	}

	signature, err := base64.StdEncoding.DecodeString(request.Signature)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, "signature must be base64")
		return
	}

	token, user, err := s.auth.Verify(request.ChallengeID, request.PublicKey, signature)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrChallengeNotFound) {
			status = http.StatusBadRequest
		}
		writeHTTPError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token":         token,
		"authenticated": true,
		"user":          user,
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth.CurrentUser(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": ok,
		"user":          user,
	})
}

func (s *Server) listSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": s.sessionList(),
	})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Cols   int    `json:"cols"`
		Rows   int    `json:"rows"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&request)
	}

	kind := strings.TrimSpace(request.Kind)
	if kind == "" {
		kind = sessionKindTerminal
	}

	var session any
	switch kind {
	case sessionKindTerminal:
		terminalSession, err := s.sessions.Create(request.Name, request.Cols, request.Rows)
		if err != nil {
			s.logger.Error("create terminal session failed", "error", err)
			writeHTTPError(w, http.StatusInternalServerError, "session could not be created")
			return
		}
		session = terminalMetadata(terminalSession.Metadata())
	case sessionKindGUI:
		guiSession, err := s.gui.Create(request.Name, request.Width, request.Height)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, gui.ErrDisabled) {
				status = http.StatusForbidden
			}
			s.logger.Error("create gui session failed", "error", err)
			writeHTTPError(w, status, err.Error())
			return
		}
		session = guiMetadata(guiSession.Metadata())
	default:
		writeHTTPError(w, http.StatusBadRequest, "unknown session kind")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"session": session,
	})
}

func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeHTTPError(w, http.StatusBadRequest, "invalid json")
		return
	}

	id := r.PathValue("id")
	if session, err := s.sessions.Rename(id, request.Name); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"session": terminalMetadata(session.Metadata()),
		})
		return
	} else if !errors.Is(err, sessions.ErrNotFound) {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}

	if session, err := s.gui.Rename(id, request.Name); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"session": guiMetadata(session.Metadata()),
		})
		return
	} else if !errors.Is(err, gui.ErrNotFound) {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeHTTPError(w, http.StatusNotFound, "session not found")
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sessions.Delete(id); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	} else if !errors.Is(err, sessions.ErrNotFound) {
		writeHTTPError(w, http.StatusInternalServerError, "session could not be closed")
		return
	}

	if err := s.gui.Delete(id); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	} else if !errors.Is(err, gui.ErrNotFound) {
		writeHTTPError(w, http.StatusInternalServerError, "session could not be closed")
		return
	}

	writeHTTPError(w, http.StatusNotFound, "session not found")
}

func (s *Server) getGUIClipboard(w http.ResponseWriter, r *http.Request) {
	session, ok := s.gui.Get(r.PathValue("id"))
	if !ok {
		writeHTTPError(w, http.StatusNotFound, "gui session not found")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	text, err := session.ReadClipboard(ctx)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, gui.ErrExited) {
			status = http.StatusGone
		}
		writeHTTPError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func (s *Server) setGUIClipboard(w http.ResponseWriter, r *http.Request) {
	session, ok := s.gui.Get(r.PathValue("id"))
	if !ok {
		writeHTTPError(w, http.StatusNotFound, "gui session not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, gui.MaxClipboardBytes+1024)
	var request struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeHTTPError(w, http.StatusBadRequest, "invalid json")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := session.WriteClipboard(ctx, request.Text); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, gui.ErrExited) {
			status = http.StatusGone
		}
		writeHTTPError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setGUIClipboardFiles(w http.ResponseWriter, r *http.Request) {
	session, ok := s.gui.Get(r.PathValue("id"))
	if !ok {
		writeHTTPError(w, http.StatusNotFound, "gui session not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, gui.MaxClipboardUploadBytes+10*1024*1024)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeHTTPError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	headers := r.MultipartForm.File["files"]
	if len(headers) == 0 {
		writeHTTPError(w, http.StatusBadRequest, "no clipboard files supplied")
		return
	}

	paths := r.MultipartForm.Value["paths"]
	files := make([]gui.ClipboardFile, 0, len(headers))
	opened := make([]interface{ Close() error }, 0, len(headers))
	for index, header := range headers {
		file, err := header.Open()
		if err != nil {
			for _, closer := range opened {
				_ = closer.Close()
			}
			writeHTTPError(w, http.StatusBadRequest, "clipboard file could not be read")
			return
		}
		opened = append(opened, file)
		name := header.Filename
		if index < len(paths) && strings.TrimSpace(paths[index]) != "" {
			name = paths[index]
		}
		files = append(files, gui.ClipboardFile{
			Name:   name,
			Reader: file,
		})
	}
	defer func() {
		for _, closer := range opened {
			_ = closer.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := session.WriteClipboardFiles(ctx, files); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, gui.ErrExited) {
			status = http.StatusGone
		}
		writeHTTPError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) sessionWebSocket(w http.ResponseWriter, r *http.Request) {
	handler := ws.Handler{
		Sessions:    s.sessions,
		Logger:      s.logger,
		CheckOrigin: s.originAllowed,
		Transport:   s.config.Transport(),
	}
	handler.ServeSession(w, r, r.PathValue("id"))
}

func (s *Server) guiWebSocket(w http.ResponseWriter, r *http.Request) {
	s.gui.ServeWebSocket(w, r, r.PathValue("id"), s.originAllowed)
}

func (s *Server) agentWebSocket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if session, ok := s.sessions.Get(id); ok {
		if session.Agent == nil {
			writeHTTPError(w, http.StatusGone, "agent proxy is not available")
			return
		}
		session.Agent.Attach(w, r, s.originAllowed)
		return
	}

	if session, ok := s.gui.Get(id); ok {
		if session.Agent == nil {
			writeHTTPError(w, http.StatusGone, "agent proxy is not available")
			return
		}
		session.Agent.Attach(w, r, s.originAllowed)
		return
	}

	writeHTTPError(w, http.StatusNotFound, "session not found")
}

type sessionMetadata struct {
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

func (s *Server) sessionList() []sessionMetadata {
	list := make([]sessionMetadata, 0)
	for _, metadata := range s.sessions.List() {
		list = append(list, terminalMetadata(metadata))
	}
	for _, metadata := range s.gui.List() {
		list = append(list, guiMetadata(metadata))
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})

	return list
}

func terminalMetadata(metadata sessions.Metadata) sessionMetadata {
	return sessionMetadata{
		ID:           metadata.ID,
		Name:         metadata.Name,
		Kind:         sessionKindTerminal,
		CreatedAt:    metadata.CreatedAt,
		LastActiveAt: metadata.LastActiveAt,
		Width:        metadata.Width,
		Height:       metadata.Height,
		State:        metadata.State,
	}
}

func guiMetadata(metadata gui.Metadata) sessionMetadata {
	return sessionMetadata{
		ID:           metadata.ID,
		Name:         metadata.Name,
		Kind:         sessionKindGUI,
		CreatedAt:    metadata.CreatedAt,
		LastActiveAt: metadata.LastActiveAt,
		Width:        metadata.Width,
		Height:       metadata.Height,
		Display:      metadata.Display,
		State:        metadata.State,
	}
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Authenticated(r) {
			writeHTTPError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}

	host := forwardedHost(r)
	if strings.EqualFold(parsed.Host, host) {
		return true
	}

	for _, allowed := range s.config.CORSOrigins {
		if strings.EqualFold(origin, allowed) {
			return true
		}
	}

	return false
}

func forwardedHost(r *http.Request) string {
	if host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); host != "" {
		return host
	}
	return r.Host
}

func mustWebsite(staticDir string) website.Website {
	if website.Exists(staticDir) {
		return website.NewDirectoryWebsite(staticDir)
	}

	site, err := website.NewWebsite()
	if err != nil {
		panic(err)
	}

	return site
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
