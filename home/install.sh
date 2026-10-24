#!/usr/bin/env bash
#
# Install home as a systemd service. Normally run for you by deploy.sh; to run
# it by hand, copy a linux build of home to the server and run ON THE SERVER as
# root:
#   sudo bash install.sh ./home
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN='[::]:8000' SERVICES="vigil=8090 relay=8091/ui" bash install.sh ./home
#
# Safe to re-run: the binary and unit are replaced and the service restarted.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8000}"   # reachable from the LAN; use 127.0.0.1:8000 for local only
SERVICES="${SERVICES:-vigil=8090 relay=8091/ui garage-ui=8080 lake-ui=8092 url-to-bucket=8093 preprocessor=8099 duckpipe=8100 foreman=8096/ui analyst=8098 atlas=8101}"   # name=port[/path]

SRC_BIN="${1:-}"
BIN=/usr/local/bin/home
UNIT=/etc/systemd/system/home.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <home binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <home binary>"
command -v systemctl >/dev/null || die "systemd is required"
[[ $SERVICES != *\"* ]] || die "SERVICES must not contain quotes"

# --- 1. Binary ---------------------------------------------------------------
chmod +x "$SRC_BIN"
"$SRC_BIN" -h >/dev/null 2>&1 || die "$SRC_BIN is not a working home binary for $(uname -m)"
log "Installing $BIN"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"

# --- 2. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=Homepage for the server's web UIs
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN -listen $LISTEN -services "$SERVICES"
DynamicUser=yes
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable home >/dev/null 2>&1
systemctl restart home

port=${LISTEN##*:}
log "Waiting for home to come up"
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet home && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u home -n 40 --no-pager; die "home did not start"; }
  sleep 1
done

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the page from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

home is running.
  URL      : http://${host_ip:-localhost}:$port
  Services : $SERVICES
  Logs     : journalctl -u home -f
EOF
