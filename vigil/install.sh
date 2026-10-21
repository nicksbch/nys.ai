#!/usr/bin/env bash
#
# Install vigil as a systemd service. Normally run for you by deploy.sh; to
# run it by hand, copy a linux build of vigil to the server and run ON THE
# SERVER as root:
#   sudo bash install.sh ./vigil
#
# Override defaults with env vars, e.g.:
#   sudo DISKS="/ /var/lib/garage /mnt/data" EXPECT="url-to-bucket/download=24h" bash install.sh ./vigil
#
# Safe to re-run: the binary and unit are replaced and the service restarted,
# but existing /etc/vigil.env and /etc/vigil-client.env are left untouched,
# apart from adding an MCP token that /etc/vigil.env lacks.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8090}"              # reachable from the LAN; use 127.0.0.1:8090 for local only
DISKS="${DISKS:-/ /var/lib/garage}"        # paths whose filesystems to watch; missing ones are skipped
EXPECT="${EXPECT:-}"                       # e.g. "url-to-bucket/download=24h etl/ingest=1h"
UI_USER="${UI_USER:-admin}"                # basic-auth user for the dashboard
WEBHOOK="${WEBHOOK:-}"                     # Slack/Mattermost/Discord webhook for alerts (first run only)
EXTRA_ARGS="${EXTRA_ARGS:-}"               # e.g. "-disk-warn 90 -cpu-warn 95"

SRC_BIN="${1:-}"
BIN=/usr/local/bin/vigil
ENV_FILE=/etc/vigil.env                   # server secrets, root only
CLIENT_ENV=/etc/vigil-client.env          # what applications need to report events
UNIT=/etc/systemd/system/vigil.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <vigil binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <vigil binary>"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"

# --- 1. Binary ---------------------------------------------------------------
chmod +x "$SRC_BIN"
"$SRC_BIN" -h >/dev/null 2>&1 || die "$SRC_BIN is not a working vigil binary for $(uname -m)"
log "Installing $BIN"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"

# --- 2. Login and event token ------------------------------------------------
port=${LISTEN##*:}
new_login=
if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE and $CLIENT_ENV"
  new_login="$UI_USER:$(openssl rand -base64 24 | tr -d '/+=')"
  token=$(openssl rand -hex 24)
  umask 077
  cat >"$ENV_FILE" <<EOF
VIGIL_AUTH=$new_login
VIGIL_TOKEN=$token
VIGIL_WEBHOOK=$WEBHOOK
EOF
  umask 022
  # The token only allows posting events, so local apps and users may read it.
  cat >"$CLIENT_ENV" <<EOF
VIGIL_URL=http://127.0.0.1:$port
VIGIL_TOKEN=$token
EOF
fi
# The MCP token reads everything vigil knows, so unlike VIGIL_TOKEN it stays
# in the root-only file. Added to older installs on their next deploy.
new_mcp=
if ! grep -q '^VIGIL_MCP_TOKEN=' "$ENV_FILE"; then
  log "Adding an MCP token to $ENV_FILE"
  echo "VIGIL_MCP_TOKEN=$(openssl rand -hex 24)" >>"$ENV_FILE"
  new_mcp=1
fi
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"
[[ -e $CLIENT_ENV ]] && chmod 0644 "$CLIENT_ENV"

# --- 3. systemd unit ---------------------------------------------------------
disks=()
for d in $DISKS; do
  if [[ -e $d ]]; then disks+=("$d"); else log "Skipping disk $d: it does not exist"; fi
done
[[ ${#disks[@]} -gt 0 ]] || die "none of DISKS exist: $DISKS"
args="-listen $LISTEN -disks $(IFS=,; echo "${disks[*]}")"
for e in $EXPECT; do args+=" -expect $e"; done
args+=" $EXTRA_ARGS"

log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=vigil - server and application monitor
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
# RELAY_URL and RELAY_TOKEN, if relay is installed; app events are read from it.
EnvironmentFile=-/etc/relay-client.env
ExecStart=$BIN $args
DynamicUser=yes
StateDirectory=vigil
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
systemctl enable vigil >/dev/null 2>&1
systemctl restart vigil

log "Waiting for vigil to come up"
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet vigil && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u vigil -n 40 --no-pager; die "vigil did not start"; }
  sleep 1
done

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the dashboard from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

vigil is running.
  Dashboard : http://${host_ip:-localhost}:$port
  MCP       : http://${host_ip:-localhost}:$port/mcp (token: sudo grep VIGIL_MCP_TOKEN $ENV_FILE)
  Watching  : ${disks[*]}
  Config    : $ENV_FILE (server), $CLIENT_ENV (for applications)
  Logs      : journalctl -u vigil -f
EOF
if [[ -n $new_login ]]; then
  echo "  Login     : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE"
else
  echo "  Login     : sudo grep VIGIL_AUTH $ENV_FILE"
fi
if [[ -n $new_mcp ]]; then
  echo "  New MCP token: give it to mcp-hub (VIGIL_MCP_TOKEN in /etc/mcp-hub.env)"
fi
