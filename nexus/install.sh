#!/usr/bin/env bash
#
# Install nexus as a systemd service. Normally run for you by deploy.sh; to
# run it by hand, copy a linux build of nexus and the config directory to
# the server and run ON THE SERVER as root:
#   sudo bash install.sh ./nexus
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN=127.0.0.1:8102 EXTRA_ARGS="-max-steps 8" bash install.sh ./nexus
#
# Safe to re-run: the binary, the config (models.json, prompt.md) and the
# unit are replaced and the service restarted. The config is checked first:
# if it doesn't load, nothing is changed. An existing /etc/nexus.env is left
# untouched, apart from adding mcp-hub's token that it lacks. Conversations
# and their cards in /var/lib/nexus are kept.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8102}"              # the web page; use 127.0.0.1:8102 for local only
EXTRA_ARGS="${EXTRA_ARGS:-}"               # e.g. "-max-steps 6 -timeout 3m"
UI_USER="${UI_USER:-admin}"                # basic-auth user for the web page
HUB="${HUB:-http://127.0.0.1:8094}"        # mcp-hub; nexus uses its "nexus" profile: the tools it can use

SRC_BIN="${1:-}"
SRC_CONFIG=$(dirname "${SRC_BIN:-.}")/config   # from the repo; replaces CONFIG's files
BIN=/usr/local/bin/nexus
ENV_FILE=/etc/nexus.env                   # page login, hub token, model keys; root only
CONFIG=/etc/nexus                         # models.json, prompt.md
UNIT=/etc/systemd/system/nexus.service
HUB_ENV=/etc/mcp-hub.env
LLAMA_ENV=/etc/llama-server.env

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <nexus binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <nexus binary>"
[[ -f $SRC_CONFIG/models.json && -f $SRC_CONFIG/prompt.md ]] || die "$SRC_CONFIG needs models.json and prompt.md"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"

# --- 1. Secrets ----------------------------------------------------------------
env_value() { sed -n "s/^$2=//p" "$1" 2>/dev/null | tail -1 || true; }
hub_token=$(env_value "$HUB_ENV" MCP_HUB_TOKEN)
llama_key=$(env_value "$LLAMA_ENV" LLAMA_API_KEY)
new_login=
if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE"
  new_login="$UI_USER:$(openssl rand -base64 24 | tr -d '/+=')"
  umask 077
  {
    echo "NEXUS_AUTH=$new_login"
    [[ -n $llama_key ]] && echo "LLAMA_API_KEY=$llama_key"
    echo "# For hosted models in models.json:"
    echo "#ANTHROPIC_API_KEY="
  } >"$ENV_FILE"
  umask 022
fi
if [[ -n $hub_token ]] && ! grep -q '^MCP_HUB_TOKEN=' "$ENV_FILE"; then
  log "Adding mcp-hub's token to $ENV_FILE"
  echo "MCP_HUB_TOKEN=$hub_token" >>"$ENV_FILE"
fi
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# --- 2. Check the new config ---------------------------------------------------
# With the new binary and the real secrets, before anything is replaced. A
# config that doesn't load stops here; an unreachable hub profile or model is
# only a warning.
chmod +x "$SRC_BIN"
log "Checking the new config: models, prompt and the hub's nexus profile"
rc=0
(set -a; . "$ENV_FILE"; set +a; "$SRC_BIN" check -config "$SRC_CONFIG" -hub "$HUB") || rc=$?
case $rc in
  0) ;;
  1) log "WARNING: not everything is reachable yet; see above" ;;
  *) die "nexus/config does not load (see above); nothing was installed" ;;
esac

# --- 3. Binary and config ------------------------------------------------------
log "Installing $BIN and $CONFIG"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"
install -d -m 0755 "$CONFIG"
install -m 0644 -o root -g root "$SRC_CONFIG/models.json" "$CONFIG/models.json"
install -m 0644 -o root -g root "$SRC_CONFIG/prompt.md" "$CONFIG/prompt.md"

# --- 4. systemd unit -----------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=nexus - ask the server to do things, in a chat
After=network-online.target mcp-hub.service relay.service llama-server.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
EnvironmentFile=-/etc/relay-client.env
ExecStart=$BIN serve -listen $LISTEN -config $CONFIG -hub $HUB -data /var/lib/nexus $EXTRA_ARGS
DynamicUser=yes
StateDirectory=nexus
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
systemctl enable nexus >/dev/null 2>&1
systemctl restart nexus

port=${LISTEN##*:}
log "Waiting for nexus to come up"
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet nexus && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u nexus -n 40 --no-pager; die "nexus did not start"; }
  sleep 1
done

# --- Done ----------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the page from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

nexus is running.
  Page    : http://${host_ip:-localhost}:$port
  Config  : $CONFIG, from nexus/config/ in the repo (change it there and deploy)
  Secrets : $ENV_FILE
  Data    : /var/lib/nexus (conversations/, cards/)
  Logs    : journalctl -u nexus -f
EOF
if [[ -n $new_login ]]; then
  echo "  Login   : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE"
else
  echo "  Login   : sudo grep NEXUS_AUTH $ENV_FILE"
fi
