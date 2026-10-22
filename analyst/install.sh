#!/usr/bin/env bash
#
# Install analyst as a systemd service. Normally run for you by deploy.sh; to
# run it by hand, copy a linux build of analyst and the config directory to
# the server and run ON THE SERVER as root:
#   sudo bash install.sh ./analyst
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN=127.0.0.1:8098 EXTRA_ARGS="-max-steps 8" bash install.sh ./analyst
#
# Safe to re-run: the binary, the config (models.json, prompt.md) and the
# unit are replaced and the service restarted. The config is checked first:
# if it doesn't load, nothing is changed. An existing /etc/analyst.env is left
# untouched, apart from adding mcp-hub's token that it lacks. Conversations
# and reports in /var/lib/analyst are kept.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8098}"              # the web page; use 127.0.0.1:8098 for local only
EXTRA_ARGS="${EXTRA_ARGS:-}"               # e.g. "-max-steps 8 -timeout 5m"
UI_USER="${UI_USER:-admin}"                # basic-auth user for the web page
HUB="${HUB:-http://127.0.0.1:8094}"        # mcp-hub; analyst uses its "analyst" profile

SRC_BIN="${1:-}"
SRC_CONFIG=$(dirname "${SRC_BIN:-.}")/config   # from the repo; replaces CONFIG's files
BIN=/usr/local/bin/analyst
ENV_FILE=/etc/analyst.env                 # page login, hub token, model keys; root only
CONFIG=/etc/analyst                       # models.json, prompt.md
UNIT=/etc/systemd/system/analyst.service
HUB_ENV=/etc/mcp-hub.env
LLAMA_ENV=/etc/llama-server.env

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <analyst binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <analyst binary>"
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
    echo "ANALYST_AUTH=$new_login"
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
log "Checking the new config: models, prompt and the hub's analyst profile"
rc=0
(set -a; . "$ENV_FILE"; set +a; "$SRC_BIN" check -config "$SRC_CONFIG" -hub "$HUB") || rc=$?
case $rc in
  0) ;;
  1) log "WARNING: not everything is reachable yet; see above" ;;
  *) die "analyst/config does not load (see above); nothing was installed" ;;
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
Description=analyst - ask questions about the lake through the semantic layer
After=network-online.target mcp-hub.service llama-server.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
ExecStart=$BIN serve -listen $LISTEN -config $CONFIG -hub $HUB -data /var/lib/analyst $EXTRA_ARGS
DynamicUser=yes
StateDirectory=analyst
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
systemctl enable analyst >/dev/null 2>&1
systemctl restart analyst

port=${LISTEN##*:}
log "Waiting for analyst to come up"
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet analyst && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u analyst -n 40 --no-pager; die "analyst did not start"; }
  sleep 1
done

# --- Done ----------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the page from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

analyst is running.
  Page    : http://${host_ip:-localhost}:$port
  Config  : $CONFIG, from analyst/config/ in the repo (change it there and deploy)
  Secrets : $ENV_FILE
  Data    : /var/lib/analyst (conversations/, reports/)
  Logs    : journalctl -u analyst -f
EOF
if [[ -n $new_login ]]; then
  echo "  Login   : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE"
else
  echo "  Login   : sudo grep ANALYST_AUTH $ENV_FILE"
fi
