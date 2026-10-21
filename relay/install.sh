#!/usr/bin/env bash
#
# Install relay as a systemd service. Normally run for you by deploy.sh; to
# run it by hand, copy a linux build of relay to the server and run ON THE
# SERVER as root:
#   sudo bash install.sh ./relay
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN=127.0.0.1:8091 EXTRA_ARGS="-retention 2160h" bash install.sh ./relay
#
# Safe to re-run: the binary and unit are replaced and the service restarted,
# but existing /etc/relay.env and /etc/relay-client.env are left untouched.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8091}"              # reachable from the LAN; use 127.0.0.1:8091 for local only
EXTRA_ARGS="${EXTRA_ARGS:-}"               # e.g. "-retention 2160h"

SRC_BIN="${1:-}"
BIN=/usr/local/bin/relay                  # also the CLI, for every user on the server
ENV_FILE=/etc/relay.env                   # server secrets, root only
CLIENT_ENV=/etc/relay-client.env          # what applications and the CLI need
UNIT=/etc/systemd/system/relay.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <relay binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <relay binary>"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"

# --- 1. Binary ---------------------------------------------------------------
chmod +x "$SRC_BIN"
"$SRC_BIN" help >/dev/null 2>&1 || die "$SRC_BIN is not a working relay binary for $(uname -m)"
log "Installing $BIN"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"

# --- 2. Token ----------------------------------------------------------------
port=${LISTEN##*:}
if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE and $CLIENT_ENV"
  token=$(openssl rand -hex 24)
  umask 077
  cat >"$ENV_FILE" <<EOF
RELAY_TOKEN=$token
EOF
  umask 022
  # Local apps, agents and users publish and read with the same token.
  cat >"$CLIENT_ENV" <<EOF
RELAY_URL=http://127.0.0.1:$port
RELAY_TOKEN=$token
EOF
fi
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"
[[ -e $CLIENT_ENV ]] && chmod 0644 "$CLIENT_ENV"

# --- 3. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=relay - event log with subscriptions
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
ExecStart=$BIN serve -listen $LISTEN $EXTRA_ARGS
DynamicUser=yes
StateDirectory=relay
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true
NoNewPrivileges=true
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable relay >/dev/null 2>&1
systemctl restart relay

log "Waiting for relay to come up"
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet relay && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u relay -n 40 --no-pager; die "relay did not start"; }
  sleep 1
done

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach relay from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

relay is running.
  API     : http://${host_ip:-localhost}:$port  (GET / for usage)
  CLI     : relay help   (reads $CLIENT_ENV)
  Config  : $ENV_FILE (server), $CLIENT_ENV (for applications)
  Logs    : journalctl -u relay -f
EOF
