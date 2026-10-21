#!/usr/bin/env bash
#
# Install foreman as a systemd service. Normally run for you by deploy.sh; to
# run it by hand, copy a linux build of foreman and the config and examples
# directories to the server and run ON THE SERVER as root:
#   sudo bash install.sh ./foreman
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN=127.0.0.1:8096 EXTRA_ARGS="-concurrency 1" bash install.sh ./foreman
#
# Safe to re-run: the binary, the config (models.json, agents/, tasks/), the
# examples and the unit are replaced and the service restarted. The config is
# checked first: if it doesn't load, nothing is changed. An existing
# /etc/foreman.env is left untouched, apart from adding mcp-hub's token that
# it lacks.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8096}"              # the web page; use 127.0.0.1:8096 for local only
EXTRA_ARGS="${EXTRA_ARGS:-}"               # e.g. "-concurrency 1 -retention 720h"
UI_USER="${UI_USER:-admin}"                # basic-auth user for the web page
HUB="${HUB:-http://127.0.0.1:8094}"        # mcp-hub

SRC_BIN="${1:-}"
SRC_DIR=$(dirname "${SRC_BIN:-.}")
BIN=/usr/local/bin/foreman                # also the CLI (foreman try, foreman check)
ENV_FILE=/etc/foreman.env                 # page login, hub token, model keys; root only
CONFIG=/etc/foreman                       # models.json, agents/, tasks/, examples/
SRC_CONFIG=$SRC_DIR/config                # from the repo; replaces CONFIG's files
UNIT=/etc/systemd/system/foreman.service
HUB_ENV=/etc/mcp-hub.env
LLAMA_ENV=/etc/llama-server.env

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <foreman binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <foreman binary>"
[[ -f $SRC_CONFIG/models.json && -d $SRC_CONFIG/agents && -d $SRC_CONFIG/tasks ]] ||
  die "$SRC_CONFIG needs models.json, agents/ and tasks/"
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
    echo "FOREMAN_AUTH=$new_login"
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
# config that doesn't load stops here; something unreachable (a hub profile,
# a model) is only a warning.
chmod +x "$SRC_BIN"
"$SRC_BIN" help >/dev/null 2>&1 || die "$SRC_BIN is not a working foreman binary for $(uname -m)"
log "Checking the new config: agents, hub profiles and models"
rc=0
(set -a; . "$ENV_FILE"; set +a; "$SRC_BIN" check -config "$SRC_CONFIG" -hub "$HUB") || rc=$?
case $rc in
  0) ;;
  1) log "WARNING: not everything is reachable yet; see above" ;;
  *) die "foreman/config does not load (see above); nothing was installed" ;;
esac

# --- 3. Binary, config and examples --------------------------------------------
log "Installing $BIN"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"
log "Installing the config in $CONFIG"
install -d -m 0755 "$CONFIG"
install -m 0644 -o root -g root "$SRC_CONFIG/models.json" "$CONFIG/models.json"
# Each directory is replaced whole, so a file removed from the repo goes too.
for d in agents tasks examples; do
  src=$SRC_CONFIG/$d
  [[ $d == examples ]] && src=$SRC_DIR/examples
  [[ -d $src ]] || continue
  rm -rf "$CONFIG/$d.new"
  cp -r "$src" "$CONFIG/$d.new"
  chown -R root:root "$CONFIG/$d.new"
  chmod -R u=rwX,go=rX "$CONFIG/$d.new"
  rm -rf "$CONFIG/$d"
  mv "$CONFIG/$d.new" "$CONFIG/$d"
done

# --- 4. systemd unit -----------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=foreman - always-on agents for the server
After=network-online.target relay.service mcp-hub.service llama-server.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
# RELAY_URL and RELAY_TOKEN, if relay is installed: event triggers and results.
EnvironmentFile=-/etc/relay-client.env
ExecStart=$BIN serve -listen $LISTEN -config $CONFIG -hub $HUB $EXTRA_ARGS
DynamicUser=yes
StateDirectory=foreman
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
systemctl enable foreman >/dev/null 2>&1
systemctl restart foreman

port=${LISTEN##*:}
log "Waiting for foreman to come up"
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet foreman && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u foreman -n 40 --no-pager; die "foreman did not start"; }
  sleep 1
done

# --- Done ----------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the page from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

foreman is running.
  Page    : http://${host_ip:-localhost}:$port/ui
  Config  : $CONFIG, from foreman/config/ in the repo (change it there and deploy)
  Secrets : $ENV_FILE
  Data    : /var/lib/foreman (tasks.jsonl, runs/)
  Check   : sudo bash -c 'set -a; . $ENV_FILE; foreman check'
  Logs    : journalctl -u foreman -f
EOF
if [[ -n $new_login ]]; then
  echo "  Login   : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE"
else
  echo "  Login   : sudo grep FOREMAN_AUTH $ENV_FILE"
fi
