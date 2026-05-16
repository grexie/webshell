# WebShell

WebShell is a self-hosted browser-native SSH/PTTY environment. It runs as one Go process, serves an embedded static Next.js frontend, keeps PTY and optional GUI sessions alive across browser reconnects, and authenticates with SSH keys instead of accounts, passwords, OAuth, or a central service.

## Architecture

```
Browser static frontend
  - xterm.js
  - encrypted SSH private key in LocalStorage
  - browser-side signing and agent forwarding
        |
        | WebSocket
        v
Go WebShell server
  - SSH challenge authentication
  - authorized_keys trust model
  - PTY session manager
  - optional Xorg dummy/Xvfb GUI session manager
  - SSH_AUTH_SOCK agent proxy
        |
        v
Login shell / virtual X display
```

The backend never receives or persists private keys. The browser signs login challenges and agent requests locally. Shell sessions get a temporary `SSH_AUTH_SOCK` pointing at a per-session proxy, so `ssh` inside the shell can request signatures from the unlocked browser key. GUI websocket endpoints use the same bearer session created by SSH-key login.

## Authentication

WebShell validates public keys against OpenSSH `authorized_keys`.

By default it reads:

```text
~/.ssh/authorized_keys
```

You can override that with:

```bash
export WEBSHELL_AUTHORIZED_KEYS=/path/to/authorized_keys
```

Or provide keys inline:

```bash
export WEBSHELL_AUTHORIZED_KEYS_INLINE='ssh-ed25519 AAAAC3... tim@laptop'
```

The browser import flow currently supports unencrypted OpenSSH private keys and traditional PKCS#1 `BEGIN RSA PRIVATE KEY` files. WebShell then encrypts the key into LocalStorage with a passphrase-derived AES-GCM key and only unlocks it into memory for the active browser session.

## Local Development

Install dependencies:

```bash
npm install
go mod download
```

Run the API server and hot-reloading Next.js frontend:

```bash
make dev
```

Open `http://localhost:3000`.

Build and run the production Docker image locally. The Docker build includes the Next.js static export and embeds it into the Go binary:

```bash
make webshell
```

Open `http://localhost:8080`.

The desktop container creates `WEBSHELL_USER` at startup and stores user state in a Docker volume:

```bash
docker run --rm -p 8080:8080 \
  -v webshell-home:/home/webshell \
  -v "$HOME/.ssh/authorized_keys:/authorized_keys:ro" \
  -e WEBSHELL_AUTHORIZED_KEYS=/authorized_keys \
  -e WEBSHELL_USER=webshell \
  -e WEBSHELL_GUI_ENABLED=true \
  webshell:latest
```

## Environment

| Variable | Default | Description |
| --- | --- | --- |
| `WEBSHELL_HOST` | `0.0.0.0` | HTTP bind host |
| `WEBSHELL_PORT` | `8080` | HTTP bind port |
| `WEBSHELL_AUTHORIZED_KEYS` | `~/.ssh/authorized_keys` | OpenSSH authorized keys file |
| `WEBSHELL_AUTHORIZED_KEYS_INLINE` | empty | Inline authorized_keys entries |
| `WEBSHELL_AUTH_CHALLENGE_TTL` | `5m` | Login challenge lifetime |
| `WEBSHELL_AUTH_SESSION_TTL` | `12h` | In-memory bearer session lifetime |
| `WEBSHELL_CORS_ORIGIN` | `http://localhost:3000` | Dev frontend origin |
| `WEBSHELL_STATIC_DIR` | empty | Optional static directory override instead of embedded assets |
| `WEBSHELL_RUNTIME_DIR` | `/tmp` | Directory for temporary Unix sockets used by browser-backed SSH agent forwarding |
| `WEBSHELL_USER` | `tim` in Docker | Linux user that terminal and GUI sessions run as |
| `WEBSHELL_UID` | `1000` in Docker | UID for startup user creation and process credentials |
| `WEBSHELL_GID` | `1000` in Docker | GID for startup user creation and process credentials |
| `WEBSHELL_USER_PASSWORD` | empty | Optional password for the startup user and sudo |
| `WEBSHELL_USER_SHELL` | `/bin/bash` in Docker | Login shell for terminal sessions |
| `WEBSHELL_USER_SUDO` | `true` in Docker | Add startup user to `sudo` |
| `WEBSHELL_USER_SUDO_NOPASSWD` | `true` in Docker | Install a sudoers drop-in so passwordless startup users can run `sudo` |
| `WEBSHELL_HOME` | `/home/tim` in Docker | Home directory and default session working directory |
| `WEBSHELL_GUI_ENABLED` | `false` | Enable browser-rendered GUI desktop sessions |
| `WEBSHELL_GUI_DEFAULT` | `gnome` in Docker | Default GUI profile used by `webshell-gui-session` |
| `WEBSHELL_GUI_COMMAND` | `webshell-gui-session` in Docker | GUI command launched inside each GUI session |
| `WEBSHELL_GUI_XSERVER` | `auto` | Display backend: `auto`, `xorg`, or `xvfb`; `auto` prefers Xorg dummy and falls back to Xvfb |
| `WEBSHELL_GUI_WIDTH` | `1280` | Default GUI desktop width |
| `WEBSHELL_GUI_HEIGHT` | `720` | Default GUI desktop height |
| `WEBSHELL_GUI_DEPTH` | `24` | Virtual X screen color depth |
| `WEBSHELL_GUI_FPS` | `20` | Full-frame capture rate, clamped to 1-30 FPS |
| `WEBSHELL_GUI_QUALITY` | `50` | JPEG capture quality, clamped to 1-100 |
| `WEBSHELL_GUI_MIN_WIDTH` | `640` | Minimum virtual X desktop width |
| `WEBSHELL_GUI_MIN_HEIGHT` | `480` | Minimum virtual X desktop height |
| `WEBSHELL_GUI_MAX_WIDTH` | `2560` | Maximum virtual X desktop width |
| `WEBSHELL_GUI_MAX_HEIGHT` | `1600` | Maximum virtual X desktop height |
| `WEBSHELL_GNOME_LOCK_ENABLED` | `false` in Docker | Re-enable GNOME screen locking when explicitly set to `true` |
| `WEBSHELL_GNOME_KEYRING_ENABLED` | `true` in Docker | Start an empty-password GNOME keyring inside GUI desktop sessions for Secret Service/libsecret clients |
| `WEBSHELL_GNOME_KEYRING_COMPONENTS` | `pkcs11,secrets` | GNOME keyring components to start; SSH is disabled by default so WebShell agent forwarding owns `SSH_AUTH_SOCK` |
| `WEBSHELL_GNOME_KEYRING_SSH_AGENT` | `false` in Docker | Allow GNOME keyring to export its own SSH agent socket instead of preserving WebShell browser-backed SSH agent forwarding |
| `WEBSHELL_AUDIO_ENABLED` | `true` in Docker | Stream GUI session audio to the browser |
| `WEBSHELL_AUDIO_SAMPLE_RATE` | `48000` | Audio capture sample rate |
| `WEBSHELL_AUDIO_CHANNELS` | `2` | Audio capture channels |
| `WEBSHELL_AUDIO_CODEC` | `pcm-s16le` | Browser audio stream format |
| `WEBSHELL_AUDIO_BITRATE` | `96000` | Reserved for compressed audio codecs |
| `WEBSHELL_AUDIO_LATENCY_MS` | `80` | PulseAudio capture fragment target |
| `WEBSHELL_E2EE_ENABLED` | `true` | Enable application-level websocket encryption |
| `WEBSHELL_E2EE_REQUIRED` | `true` | Require encrypted terminal and GUI websocket traffic |
| `WEBSHELL_COMPRESSION_ENABLED` | `false` | Compress eligible encrypted text payloads before encryption |
| `WEBSHELL_COMPRESSION_CODEC` | `gzip` | Compression codec for encrypted text payloads |
| `WEBSHELL_COMPRESSION_MIN_BYTES` | `256` | Minimum plaintext size before compression |
| `WEBSHELL_DEBUG_KEYS` | `false` | Log browser GUI keyboard events and backend X11 translations |
| `WEBSHELL_INSTALL_CHROME` | `true` in Docker | Documented desktop image install flag; tools are baked into the current desktop image |
| `WEBSHELL_INSTALL_VSCODE` | `true` in Docker | Documented desktop image install flag; tools are baked into the current desktop image |
| `WEBSHELL_INSTALL_TELEGRAM` | `true` in Docker | Documented desktop image install flag; tools are baked into the current desktop image |
| `WEBSHELL_INSTALL_CODEX` | `true` in Docker | Documented desktop image install flag; tools are baked into the current desktop image |
| `WEBSHELL_INSTALL_CHATGPT` | `true` in Docker | Enables the bundled Chrome app launcher approach for ChatGPT |

## GUI Sessions

GUI sessions are optional and use a practical framebuffer transport:

```text
Browser canvas
  <-> authenticated WebSocket
Go GUI session manager
  <-> Xorg dummy/Xvfb + GNOME/openbox/xfce + configured GUI command
```

The backend prefers Xorg with the dummy video driver, falls back to `Xvfb`, launches `WEBSHELL_GUI_COMMAND`, captures the root framebuffer as JPEG frames, and sends mouse, wheel, keyboard, and resize events back through `xdotool`/`xrandr`. Xorg dummy supports dynamic RandR modes, so browser resize can resize the remote desktop without killing GNOME or user apps. If live X resize still fails, WebShell keeps the existing desktop and browser-scales the framebuffer instead of restarting the session. Frame streaming prefers a persistent `ffmpeg -f x11grab` MJPEG capture process and sends JPEG bytes as binary websocket frames; if `ffmpeg` is unavailable, WebShell falls back to ImageMagick `import` captures. After the first full frame, WebShell compares frames in tiles and sends VNC-style dirty rectangle patches for changed regions, falling back to full frames for large changes, reconnects, and resizes. The browser demuxes GUI websocket frames in a worker, sends video frames to an OffscreenCanvas renderer when supported, and falls back to main-thread canvas rendering on Safari/iPad. GUI audio starts a private PulseAudio daemon per GUI session, routes apps to a `webshell_sink` null sink, captures small PulseAudio fragments as signed 16-bit PCM, and plays through an AudioWorklet with ScriptProcessor fallback. GUI sessions also receive the browser-backed `SSH_AUTH_SOCK`, expose toolbar buttons for browser-to-X and X-to-browser clipboard sync through `xclip`, and `webshell-gui-session` starts an empty-password GNOME keyring Secret Service for Chrome, VS Code, and libsecret clients without replacing WebShell's SSH agent socket. In the desktop Docker image the default GUI command is `webshell-gui-session`, which starts GNOME Flashback when the systemd container entrypoint is used. If systemd/logind services are unavailable, the launcher falls back to XFCE and then Openbox + xterm instead of leaving the browser attached to a broken desktop. Full GNOME Shell under headless X remains fragile, so the launcher only runs it when explicitly requested with `WEBSHELL_GUI_DEFAULT=gnome-shell`.

Terminal and GUI websocket payloads are encrypted above TLS when `WEBSHELL_E2EE_ENABLED=true`. The browser and server perform an ephemeral X25519 key exchange, derive per-direction AES-256-GCM keys with HKDF-SHA256, and then send opaque binary encrypted envelopes. TLS is still required; this layer prevents a TLS-terminating reverse proxy from reading terminal bytes, GUI input, framebuffer patches, or GUI audio chunks. SSH agent forwarding is still on its existing authenticated websocket path and should be moved onto the shared encrypted transport next.

All terminal and GUI processes run as `WEBSHELL_USER` when that user is configured. The server applies Unix process credentials directly and sets `HOME`, `USER`, `LOGNAME`, `SHELL`, and `PWD` to the configured home directory. Terminal sessions also set `TERM=xterm-256color` and `COLORTERM=truecolor`.

GUI keyboard input is event-based, not terminal byte-based. The browser sends structured `KeyboardEvent` metadata (`code`, `key`, `location`, `repeat`, and modifier booleans), and the backend translates from physical `code` first so Safari/macOS punctuation such as `-`, `_`, `/`, `?`, `=`, `+`, `~`, and brackets remains reliable. Set `WEBSHELL_DEBUG_KEYS=true` to log the browser event and translated X11 key action.

For local non-Docker development, install these tools on the WebShell host:

```bash
xserver-xorg-core xserver-xorg-video-dummy xvfb x11-xserver-utils xdotool openbox xterm ffmpeg imagemagick dbus-x11 pulseaudio pulseaudio-utils
```

Resize behavior attempts live X resize with dynamic RandR modes:

```bash
DISPLAY=:N xrandr --newmode webshell-WIDTHxHEIGHT ...
DISPLAY=:N xrandr --addmode DUMMY0 webshell-WIDTHxHEIGHT
DISPLAY=:N xrandr --fb WIDTHxHEIGHT --output DUMMY0 --mode webshell-WIDTHxHEIGHT
```

If live resize fails, WebShell keeps the session running at the existing framebuffer size and scales in the browser. It does not restart the desktop as a resize fallback.

## Commands

Make targets:

```bash
make dev
make build
make buildx
make webshell
make deploy
make deployx
make test
make typecheck
```

`make build` builds `webshell:latest` locally. Override with `IMAGE=my-image TAG=dev make build`.
Override the single-platform Docker target with `DOCKER_PLATFORM=linux/amd64 make build`.
`make buildx` builds through Docker Buildx for `linux/amd64,linux/arm64` by default. Override with `BUILDX_PLATFORMS=linux/amd64 make buildx`.

`make webshell` builds the image and starts the full desktop container with `WEBSHELL_USER=tim`, a persistent `webshell-home` volume, SSH-key auth from `AUTHORIZED_KEYS`, and systemd enabled as PID 1 so GNOME has DBus/logind/session services. Override with `WEBSHELL_USER=alice AUTHORIZED_KEYS=/path/to/authorized_keys make webshell`.

GNOME requires the systemd container flags used by the Makefile: privileged mode, a private cgroup namespace, executable tmpfs mounts for `/run`, `/run/lock`, and `/tmp`, and `SIGRTMIN+3` as the stop signal. A plain `docker run webshell:latest` still starts the simpler non-systemd WebShell process, but GNOME sessions should be launched through `make webshell` or an equivalent systemd Docker invocation.

On Apple Silicon, `make webshell` defaults the systemd desktop build to `linux/arm64` because Debian systemd service spawning is unreliable under `linux/amd64` emulation. The arm64 desktop image uses Chromium instead of Google Chrome because Google does not publish a Linux arm64 Chrome package.

`make deploy` builds and pushes `ghcr.io/grexie/webshell:latest`. Override with `TAG=v0.1.0 make deploy`.
`make deployx` builds and pushes a multi-architecture manifest to `ghcr.io/grexie/webshell:${TAG}` using Buildx. Tagged GitHub Actions builds run `make deployx` automatically with `TAG` set to the pushed git tag.
It assumes Docker is already logged in to GHCR:

```bash
echo "$GITHUB_TOKEN" | docker login ghcr.io -u USERNAME --password-stdin
```

Frontend:

```bash
npm --workspace packages/website run dev
npm --workspace packages/website run build
npm --workspace packages/website run typecheck
```

Backend:

```bash
go run ./cmd/webshell
go test ./cmd/... ./pkg/... ./packages/website
```

## Docker

```bash
make webshell
```

The production image contains the Go binary with the static frontend embedded and a Debian desktop/runtime layer. It does not contact external identity services. The desktop image includes GNOME, Google Chrome on `linux/amd64`, VS Code, Telegram Desktop, ChatGPT and WhatsApp Chrome app launchers, Codex CLI, Git, Go, Node.js/npm/pnpm/yarn, Python, Neovim, tmux, htop, SSH tools, build tools, database clients, and common networking/debugging utilities.

Mount a home volume for persistence:

```bash
docker run --rm -p 8080:8080 \
  -v webshell-home:/home/tim \
  -v "$HOME/.ssh/authorized_keys:/authorized_keys:ro" \
  -e WEBSHELL_AUTHORIZED_KEYS=/authorized_keys \
  -e WEBSHELL_USER=tim \
  webshell:latest
```

## Static Website Package

`packages/website/website.go` exposes:

```go
type Website interface {
    HTTPHandler() http.Handler
}

func NewWebsite() (Website, error)
```

It embeds `packages/website/out` with `embed.FS` and serves static assets plus `index.html` fallback.

## Screenshots

Placeholders:

- `docs/screenshots/login.png`
- `docs/screenshots/terminal-desktop.png`
- `docs/screenshots/terminal-ipad.png`
- `docs/screenshots/gui-session.png`

## Roadmap

- Encrypted private key import
- `~/.ssh/config` parsing and known_hosts management
- Terminal tabs and richer session restore
- GUI dirty rectangles and adaptive frame streaming
- Clipboard sync and mobile keyboard toolbar
- SFTP browser and port forwarding UI
- HTML widget bridge
