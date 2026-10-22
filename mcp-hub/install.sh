#!/usr/bin/env bash
#
# Install mcp-hub as a systemd service. Normally run for you by deploy.sh; to
# run it by hand, copy a linux build of mcp-hub and servers.json to the server
# and run ON THE SERVER as root:
#   sudo bash install.sh ./mcp-hub
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN=127.0.0.1:8094 bash install.sh ./mcp-hub
#
# Safe to re-run: the binary, servers.json (from the repo) and the unit are
# replaced and the service restarted. servers.json is checked first: if it
# doesn't load, nothing is changed. An existing /etc/mcp-hub.env is left
# untouched, apart from adding the vigil and semantic MCP tokens that it lacks.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8094}"              # reachable from the LAN; use 127.0.0.1:8094 for local only
EXTRA_ARGS="${EXTRA_ARGS:-}"

SRC_BIN="${1:-}"
SRC_CONFIG=$(dirname "${SRC_BIN:-.}")/servers.json   # from the repo; replaces CONFIG
BIN=/usr/local/bin/mcp-hub                # also the CLI (mcp-hub check)
ENV_FILE=/etc/mcp-hub.env                 # hub token and upstream tokens, root only
CONFIG=/etc/mcp-hub/servers.json          # upstream servers and profiles, no secrets
UNIT=/etc/systemd/system/mcp-hub.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <mcp-hub binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <mcp-hub binary>"
[[ -f $SRC_CONFIG ]] || die "$SRC_CONFIG not found: copy servers.json next to the binary"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"

# --- 1. Tokens ---------------------------------------------------------------
port=${LISTEN##*:}
new_token=
if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE"
  new_token=$(openssl rand -hex 24)
  umask 077
  cat >"$ENV_FILE" <<EOF
# Agents send this as "Authorization: Bearer ...".
MCP_HUB_TOKEN=$new_token
# Upstream tokens, named by token_env in $CONFIG. For example:
#U2B_MCP_TOKEN=
EOF
  umask 022
fi
# Copy the MCP tokens of the upstreams installed on this server.
for app in vigil semantic; do
  var="$(tr a-z A-Z <<<"$app")_MCP_TOKEN"
  token=$(sed -n "s/^$var=//p" "/etc/$app.env" 2>/dev/null || true)
  if [[ -n $token ]] && ! grep -q "^$var=" "$ENV_FILE"; then
    log "Adding $app's MCP token to $ENV_FILE"
    echo "$var=$token" >>"$ENV_FILE"
  fi
done
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# --- 2. Check the new servers.json -------------------------------------------
# With the new binary and the real tokens, before anything is replaced. A
# servers.json that doesn't load stops here; a server that is down is only a
# warning (it may not serve /mcp yet, and the hub keeps trying every 30 s).
chmod +x "$SRC_BIN"
"$SRC_BIN" help >/dev/null 2>&1 || die "$SRC_BIN is not a working mcp-hub binary for $(uname -m)"
log "Checking the new servers.json"
rc=0
(set -a; . "$ENV_FILE"; set +a; "$SRC_BIN" check -config "$SRC_CONFIG") || rc=$?
case $rc in
  0) ;;
  1) log "WARNING: not every server answered; see above" ;;
  *) die "mcp-hub/servers.json does not load (see above); nothing was installed" ;;
esac

# --- 3. Binary and servers.json ----------------------------------------------
log "Installing $BIN and $CONFIG"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"
install -d -m 0755 "$(dirname "$CONFIG")"
install -m 0644 -o root -g root "$SRC_CONFIG" "$CONFIG"

# --- 4. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=mcp-hub - one MCP endpoint for every service
After=network-online.target vigil.service relay.service semantic.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
# RELAY_URL and RELAY_TOKEN, if relay is installed; every call is published there.
EnvironmentFile=-/etc/relay-client.env
ExecStart=$BIN serve -listen $LISTEN -config $CONFIG $EXTRA_ARGS
DynamicUser=yes
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
systemctl enable mcp-hub >/dev/null 2>&1
systemctl restart mcp-hub

log "Waiting for mcp-hub to come up"
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet mcp-hub && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u mcp-hub -n 40 --no-pager; die "mcp-hub did not start"; }
  sleep 1
done

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach mcp-hub from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

mcp-hub is running.
  MCP     : http://${host_ip:-localhost}:$port/mcp  (or /mcp/<profile>; GET / for usage)
  Config  : $CONFIG, from mcp-hub/servers.json in the repo (change it there and deploy)
  Tokens  : $ENV_FILE
  Check   : sudo bash -c 'set -a; . $ENV_FILE; mcp-hub check'
  Logs    : journalctl -u mcp-hub -f
EOF
if [[ -n $new_token ]]; then
  echo "  Token   : $new_token   <- save this; also in $ENV_FILE"
else
  echo "  Token   : sudo grep MCP_HUB_TOKEN $ENV_FILE"
fi
cat <<EOF

From your Mac:
  claude mcp add --transport http hub http://${host_ip:-<server>}:$port/mcp \\
    --header "Authorization: Bearer <token>"
EOF
