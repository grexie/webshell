package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/grexie/webshell/v2/pkg/env"
	"github.com/grexie/webshell/v2/pkg/process"
	"github.com/grexie/webshell/v2/pkg/transport"
)

type Config struct {
	Host                 string
	Port                 int
	StaticDir            string
	AuthorizedKeysPath   string
	AuthorizedKeysInline string
	AuthChallengeTTL     time.Duration
	AuthSessionTTL       time.Duration
	CORSOrigins          []string
	RuntimeUser          process.User
	GUIEnabled           bool
	GUIDefault           string
	GUICommand           string
	GUIXServer           string
	GUIDefaultWidth      int
	GUIDefaultHeight     int
	GUIDepth             int
	GUIFPS               int
	GUIQuality           int
	GUIMinWidth          int
	GUIMinHeight         int
	GUIMaxWidth          int
	GUIMaxHeight         int
	GUIDebugKeys         bool
	AudioEnabled         bool
	AudioSampleRate      int
	AudioChannels        int
	AudioCodec           string
	AudioBitrate         int
	AudioLatencyMS       int
	E2EEEnabled          bool
	E2EERequired         bool
	CompressionEnabled   bool
	CompressionCodec     string
	CompressionMinBytes  int
}

func Load() Config {
	guiDefault := env.String("WEBSHELL_GUI_DEFAULT", "xfce")
	runtimeUser := process.User{
		Username: env.String("WEBSHELL_USER", ""),
		UID:      env.Int("WEBSHELL_UID", -1),
		GID:      env.Int("WEBSHELL_GID", -1),
		Home:     env.String("WEBSHELL_HOME", ""),
		Shell:    env.String("WEBSHELL_USER_SHELL", ""),
	}
	authorizedKeysPath := env.String("WEBSHELL_AUTHORIZED_KEYS", "")
	if authorizedKeysPath == "" && runtimeUser.Home != "" {
		authorizedKeysPath = filepath.Join(runtimeUser.Home, ".ssh", "authorized_keys")
	}
	return Config{
		Host:                 env.String("WEBSHELL_HOST", "0.0.0.0"),
		Port:                 env.Int("WEBSHELL_PORT", 8080),
		StaticDir:            env.String("WEBSHELL_STATIC_DIR", ""),
		AuthorizedKeysPath:   authorizedKeysPath,
		AuthorizedKeysInline: env.String("WEBSHELL_AUTHORIZED_KEYS_INLINE", ""),
		AuthChallengeTTL:     env.Duration("WEBSHELL_AUTH_CHALLENGE_TTL", 5*time.Minute),
		AuthSessionTTL:       env.Duration("WEBSHELL_AUTH_SESSION_TTL", 12*time.Hour),
		CORSOrigins:          splitList(env.String("WEBSHELL_CORS_ORIGIN", "http://localhost:3000")),
		RuntimeUser:          runtimeUser,
		GUIEnabled:           env.Bool("WEBSHELL_GUI_ENABLED", false),
		GUIDefault:           guiDefault,
		GUICommand:           env.String("WEBSHELL_GUI_COMMAND", defaultGUICommand(guiDefault)),
		GUIXServer:           env.String("WEBSHELL_GUI_XSERVER", "auto"),
		GUIDefaultWidth:      env.Int("WEBSHELL_GUI_WIDTH", 1280),
		GUIDefaultHeight:     env.Int("WEBSHELL_GUI_HEIGHT", 720),
		GUIDepth:             env.Int("WEBSHELL_GUI_DEPTH", 24),
		GUIFPS:               env.Int("WEBSHELL_GUI_FPS", 20),
		GUIQuality:           env.Int("WEBSHELL_GUI_QUALITY", 50),
		GUIMinWidth:          env.Int("WEBSHELL_GUI_MIN_WIDTH", 640),
		GUIMinHeight:         env.Int("WEBSHELL_GUI_MIN_HEIGHT", 480),
		GUIMaxWidth:          env.Int("WEBSHELL_GUI_MAX_WIDTH", 2560),
		GUIMaxHeight:         env.Int("WEBSHELL_GUI_MAX_HEIGHT", 1600),
		GUIDebugKeys:         env.Bool("WEBSHELL_DEBUG_KEYS", false),
		AudioEnabled:         env.Bool("WEBSHELL_AUDIO_ENABLED", true),
		AudioSampleRate:      env.Int("WEBSHELL_AUDIO_SAMPLE_RATE", 48000),
		AudioChannels:        env.Int("WEBSHELL_AUDIO_CHANNELS", 2),
		AudioCodec:           env.String("WEBSHELL_AUDIO_CODEC", "pcm-s16le"),
		AudioBitrate:         env.Int("WEBSHELL_AUDIO_BITRATE", 96000),
		AudioLatencyMS:       env.Int("WEBSHELL_AUDIO_LATENCY_MS", 120),
		E2EEEnabled:          env.Bool("WEBSHELL_E2EE_ENABLED", true),
		E2EERequired:         env.Bool("WEBSHELL_E2EE_REQUIRED", true),
		CompressionEnabled:   env.Bool("WEBSHELL_COMPRESSION_ENABLED", false),
		CompressionCodec:     env.String("WEBSHELL_COMPRESSION_CODEC", "gzip"),
		CompressionMinBytes:  env.Int("WEBSHELL_COMPRESSION_MIN_BYTES", 256),
	}
}

func (c Config) Transport() transport.Config {
	return transport.NormalizeConfig(transport.Config{
		Enabled:             c.E2EEEnabled,
		Required:            c.E2EERequired,
		CompressionEnabled:  c.CompressionEnabled,
		CompressionMinBytes: c.CompressionMinBytes,
	})
}

func defaultGUICommand(guiDefault string) string {
	switch strings.ToLower(strings.TrimSpace(guiDefault)) {
	case "gnome":
		return "dbus-run-session -- gnome-session --session=gnome-flashback-metacity"
	case "gnome-shell":
		return "dbus-run-session -- gnome-session"
	case "gnome-flashback":
		return "dbus-run-session -- gnome-session --session=gnome-flashback-metacity"
	case "xfce", "xfce4":
		return "xfce4-session"
	case "openbox":
		return "openbox-session"
	default:
		return "xterm"
	}
}

func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
