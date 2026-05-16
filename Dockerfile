FROM node:22-bookworm AS frontend

WORKDIR /src
COPY package.json package-lock.json ./
COPY packages/website/package.json packages/website/package.json
RUN npm ci

COPY packages/website packages/website
ARG WEBSHELL_BUILD_ID=dev
RUN rm -rf packages/website/.next packages/website/out \
  && WEBSHELL_BUILD_ID="${WEBSHELL_BUILD_ID}" npm --workspace packages/website run build

FROM golang:1.24-bookworm AS backend

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd cmd
COPY packages/website packages/website
COPY --from=frontend /src/packages/website/out packages/website/out
COPY pkg pkg
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/webshell ./cmd/webshell

FROM debian:stable AS runtime

ENV DEBIAN_FRONTEND=noninteractive

RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends \
    bash \
    bash-completion \
    build-essential \
    ca-certificates \
    curl \
    dbus-x11 \
    dnsutils \
    gnome-core \
    gnome-flashback \
    gnome-session \
    gnome-shell \
    gnome-terminal \
    gnupg \
    golang-go \
    git \
    htop \
    imagemagick \
    iputils-ping \
    jq \
    less \
    locales \
    man-db \
    metacity \
    nano \
    net-tools \
    neovim \
    nodejs \
    npm \
    openbox \
    openssh-client \
    openssh-server \
    pkg-config \
    postgresql-client \
    python3 \
    python3-pip \
    python3-venv \
    redis-tools \
    screen \
    sqlite3 \
    sudo \
    tmux \
    vim \
    wget \
    x11-xserver-utils \
    xdotool \
    xfce4-panel \
    xfce4-session \
    xfce4-terminal \
    xfdesktop4 \
    xfwm4 \
    xz-utils \
    xterm \
    xvfb \
    zsh; \
  arch="$(dpkg --print-architecture)"; \
  wget -qO- https://packages.microsoft.com/keys/microsoft.asc | gpg --dearmor -o /usr/share/keyrings/packages.microsoft.gpg; \
  echo "deb [arch=${arch} signed-by=/usr/share/keyrings/packages.microsoft.gpg] https://packages.microsoft.com/repos/code stable main" > /etc/apt/sources.list.d/vscode.list; \
  if [ "${arch}" = "amd64" ]; then \
    wget -qO- https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-linux.gpg; \
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-linux.gpg] https://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list; \
  fi; \
  apt-get update; \
  apt-get install -y --no-install-recommends code; \
  if [ "${arch}" = "amd64" ]; then \
    apt-get install -y --no-install-recommends google-chrome-stable; \
  else \
    apt-get install -y --no-install-recommends chromium; \
    ln -sf /usr/bin/chromium /usr/local/bin/google-chrome; \
  fi; \
  wget -qO /tmp/telegram.tar.xz https://telegram.org/dl/desktop/linux; \
  mkdir -p /opt/telegram; \
  tar -xJf /tmp/telegram.tar.xz --strip-components=1 -C /opt/telegram; \
  ln -sf /opt/telegram/Telegram /usr/local/bin/telegram-desktop; \
  rm -f /tmp/telegram.tar.xz; \
  npm install -g pnpm yarn @openai/codex; \
  sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen; \
  locale-gen; \
  mkdir -p /run/sshd /root/.ssh; \
  chmod 700 /root/.ssh; \
  rm -rf /var/lib/apt/lists/*

# Keep additional desktop-session packages in their own layer so existing
# runtime package cache remains useful while the desktop stack settles.
RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends \
    ffmpeg \
    gnome-session-flashback \
    pulseaudio \
    pulseaudio-utils; \
  rm -rf /var/lib/apt/lists/*

# Clipboard integration is intentionally isolated in its own layer so desktop
# package cache remains reusable while the GUI runtime evolves.
RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends xclip; \
  rm -rf /var/lib/apt/lists/*

# Xorg dummy is isolated in its own layer so the large desktop dependency layer
# remains cached while the display backend evolves.
RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends \
    xserver-xorg-core \
    xserver-xorg-video-dummy; \
  printf '%s\n' 'allowed_users=anybody' 'needs_root_rights=no' > /etc/X11/Xwrapper.config; \
  rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=backend /out/webshell /app/webshell
COPY docker/entrypoint.sh /usr/local/bin/webshell-entrypoint
COPY docker/bin/ /usr/local/bin/
COPY docker/applications/*.desktop /usr/local/share/applications/
COPY docker/systemd/*.service /etc/systemd/system/
RUN chmod +x /usr/local/bin/webshell-entrypoint /usr/local/bin/webshell-service-start /usr/local/bin/webshell-systemd-entrypoint /usr/local/bin/webshell-chatgpt /usr/local/bin/webshell-whatsapp /usr/local/bin/webshell-gui-session /usr/local/bin/google-chrome \
  && mkdir -p /etc/systemd/system/multi-user.target.wants \
  && ln -sf /etc/systemd/system/webshell.service /etc/systemd/system/multi-user.target.wants/webshell.service

ENV WEBSHELL_HOST=0.0.0.0
ENV WEBSHELL_PORT=8080
ENV WEBSHELL_USER=webshell
ENV WEBSHELL_UID=1000
ENV WEBSHELL_GID=1000
ENV WEBSHELL_USER_SHELL=/bin/bash
ENV WEBSHELL_USER_SUDO=true
ENV WEBSHELL_USER_SUDO_NOPASSWD=true
ENV WEBSHELL_HOME=/home/webshell
ENV WEBSHELL_GUI_ENABLED=true
ENV WEBSHELL_GUI_DEFAULT=gnome
ENV WEBSHELL_GUI_COMMAND=webshell-gui-session
ENV WEBSHELL_GUI_XSERVER=auto
ENV WEBSHELL_GUI_WIDTH=1280
ENV WEBSHELL_GUI_HEIGHT=720
ENV WEBSHELL_GUI_DEPTH=24
ENV WEBSHELL_GUI_FPS=20
ENV WEBSHELL_GUI_QUALITY=50
ENV WEBSHELL_GNOME_LOCK_ENABLED=false
ENV WEBSHELL_GNOME_KEYRING_ENABLED=true
ENV WEBSHELL_GNOME_KEYRING_COMPONENTS=pkcs11,secrets
ENV WEBSHELL_GNOME_KEYRING_SSH_AGENT=false
ENV WEBSHELL_AUDIO_ENABLED=true
ENV WEBSHELL_AUDIO_SAMPLE_RATE=48000
ENV WEBSHELL_AUDIO_CHANNELS=2
ENV WEBSHELL_AUDIO_CODEC=pcm-s16le
ENV WEBSHELL_AUDIO_BITRATE=96000
ENV WEBSHELL_AUDIO_LATENCY_MS=80
ENV WEBSHELL_E2EE_ENABLED=true
ENV WEBSHELL_E2EE_REQUIRED=true
ENV WEBSHELL_COMPRESSION_ENABLED=false
ENV WEBSHELL_COMPRESSION_CODEC=gzip
ENV WEBSHELL_COMPRESSION_MIN_BYTES=256
ENV WEBSHELL_DEBUG_KEYS=false
ENV WEBSHELL_INSTALL_CHROME=true
ENV WEBSHELL_INSTALL_VSCODE=true
ENV WEBSHELL_INSTALL_TELEGRAM=true
ENV WEBSHELL_INSTALL_CODEX=true
ENV WEBSHELL_INSTALL_CHATGPT=true
ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8
ENV NODE_ENV=production
ENV container=docker

EXPOSE 8080
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/webshell-entrypoint"]
CMD ["/app/webshell"]
