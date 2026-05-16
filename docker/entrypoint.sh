#!/usr/bin/env bash
set -euo pipefail

: "${WEBSHELL_USER:=tim}"
: "${WEBSHELL_UID:=1000}"
: "${WEBSHELL_GID:=1000}"
: "${WEBSHELL_USER_PASSWORD:=}"
: "${WEBSHELL_USER_SHELL:=/bin/bash}"
: "${WEBSHELL_USER_SUDO:=true}"
: "${WEBSHELL_USER_SUDO_NOPASSWD:=true}"
: "${WEBSHELL_HOME:=/home/${WEBSHELL_USER}}"

export WEBSHELL_USER
export WEBSHELL_UID
export WEBSHELL_GID
export WEBSHELL_USER_SHELL
export WEBSHELL_USER_SUDO_NOPASSWD
export WEBSHELL_HOME

if ! getent group "${WEBSHELL_GID}" >/dev/null 2>&1; then
  groupadd -g "${WEBSHELL_GID}" "${WEBSHELL_USER}"
fi

if ! id -u "${WEBSHELL_USER}" >/dev/null 2>&1; then
  useradd \
    --uid "${WEBSHELL_UID}" \
    --gid "${WEBSHELL_GID}" \
    --create-home \
    --home-dir "${WEBSHELL_HOME}" \
    --shell "${WEBSHELL_USER_SHELL}" \
    "${WEBSHELL_USER}"
fi

mkdir -p "${WEBSHELL_HOME}" "${WEBSHELL_HOME}/.ssh" /tmp/.X11-unix /tmp/.ICE-unix /run/dbus "/run/user/${WEBSHELL_UID}"
chown "${WEBSHELL_UID}:${WEBSHELL_GID}" "${WEBSHELL_HOME}" "${WEBSHELL_HOME}/.ssh"
chown "${WEBSHELL_UID}:${WEBSHELL_GID}" "/run/user/${WEBSHELL_UID}"
chmod 700 "${WEBSHELL_HOME}/.ssh"
chmod 700 "/run/user/${WEBSHELL_UID}"
chmod 1777 /tmp/.X11-unix
chmod 1777 /tmp/.ICE-unix

# Desktop applications expect a system bus to exist even when the container is
# not booted with systemd. Start the lightweight DBus daemon if it is available;
# failures should not stop terminal-only WebShell containers.
if [[ ! -d /run/systemd/system ]] && command -v dbus-daemon >/dev/null 2>&1 && [[ ! -S /run/dbus/system_bus_socket ]]; then
  rm -f /run/dbus/pid
  if ! dbus-daemon --system --fork --nopidfile; then
    echo "webshell-entrypoint: warning: failed to start system DBus" >&2
  fi
fi

if [[ -d /run/systemd/system ]]; then
  loginctl enable-linger "${WEBSHELL_USER}" >/dev/null 2>&1 || true
  systemctl start "user@${WEBSHELL_UID}.service" >/dev/null 2>&1 || true
fi

if [[ -n "${WEBSHELL_USER_PASSWORD}" ]]; then
  echo "${WEBSHELL_USER}:${WEBSHELL_USER_PASSWORD}" | chpasswd
fi

sudoers_file="/etc/sudoers.d/webshell-user"
if [[ "${WEBSHELL_USER_SUDO}" == "true" ]]; then
  usermod -aG sudo "${WEBSHELL_USER}"
  if [[ "${WEBSHELL_USER_SUDO_NOPASSWD}" == "true" ]]; then
    printf '%s ALL=(ALL) NOPASSWD:ALL\n' "${WEBSHELL_USER}" >"${sudoers_file}"
    chmod 0440 "${sudoers_file}"
    visudo -cf "${sudoers_file}" >/dev/null
  else
    rm -f "${sudoers_file}"
  fi
else
  rm -f "${sudoers_file}"
fi

if [[ -z "${WEBSHELL_AUTHORIZED_KEYS:-}" ]]; then
  export WEBSHELL_AUTHORIZED_KEYS="${WEBSHELL_HOME}/.ssh/authorized_keys"
fi

if [[ ! -f "${WEBSHELL_HOME}/.bashrc" ]]; then
  touch "${WEBSHELL_HOME}/.bashrc"
fi

if ! grep -q "WebShell shell defaults" "${WEBSHELL_HOME}/.bashrc"; then
  cat >>"${WEBSHELL_HOME}/.bashrc" <<'EOF'

# WebShell shell defaults
export TERM=xterm-256color
export COLORTERM=truecolor
alias ls='ls --color=auto'
alias grep='grep --color=auto'
alias ll='ls -alF --color=auto'
PS1='\[\e[32m\]\u@\h\[\e[0m\]:\[\e[34m\]\w\[\e[0m\]\$ '
EOF
fi

chown "${WEBSHELL_UID}:${WEBSHELL_GID}" "${WEBSHELL_HOME}/.bashrc"

exec "$@"
