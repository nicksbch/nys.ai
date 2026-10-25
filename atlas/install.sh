#!/usr/bin/env bash
#
# Install atlas as a systemd service. Normally run for you by deploy.sh; to
# run it by hand, copy a linux build of atlas to the server and run ON THE
# SERVER as root:
#   sudo bash install.sh ./atlas
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN=127.0.0.1:8101 bash install.sh ./atlas
#
# Safe to re-run: the binary and unit are replaced and the service restarted.
# An existing /etc/atlas.env (the page's login) is left untouched.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8101}"   # the page; use 127.0.0.1:8101 for local only
LINKS="${LINKS:-url-to-bucket=8093 preprocessor=8099 duckpipe=8100 garage-ui=8080 lake-ui=8092 foreman=8096/ui analyst=8098 nexus=8102 relay=8091/ui vigil=8090}"   # name=port[/path]
UI_USER="${UI_USER:-admin}"     # basic-auth user for the page

SRC_BIN="${1:-}"
BIN=/usr/local/bin/atlas
LAKESHAPE=/usr/local/bin/lakeshape
ENV_FILE=/etc/atlas.env        # the page's login; root only
UNIT=/etc/systemd/system/atlas.service
# The configs atlas reads. Each service's install makes its config readable by everyone.
CONFIGS=(/etc/preprocessor.yaml /etc/duckpipe.yaml /etc/lakeshape/project/jobs.yml /etc/lakeshape/project/sources.yml
  /opt/semantic/app/models.yml /etc/mcp-hub/servers.json /etc/foreman/agents /etc/foreman/tasks /etc/analyst/models.json)

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <atlas binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <atlas binary>"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"
[[ $LINKS != *\"* ]] || die "LINKS must not contain quotes"

# --- 1. Binary ---------------------------------------------------------------
chmod +x "$SRC_BIN"
"$SRC_BIN" help >/dev/null 2>&1 || [[ $? -eq 2 ]] || die "$SRC_BIN is not a working atlas binary for $(uname -m)"
log "Installing $BIN"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"

# --- 2. Login ----------------------------------------------------------------
new_login=
if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE"
  new_login="$UI_USER:$(openssl rand -base64 24 | tr -d '/+=')"
  umask 077
  echo "ATLAS_AUTH=$new_login" >"$ENV_FILE"
  umask 022
fi
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# --- 3. Can an unprivileged user read the configs? ---------------------------
log "Checking the configs are readable by everyone"
unreadable=()
for p in "${CONFIGS[@]}"; do
  [[ -e $p ]] || { echo "  missing (not installed?): $p"; continue; }
  runuser -u nobody -- test -r "$p" || unreadable+=("$p")
done
if [[ ${#unreadable[@]} -gt 0 ]]; then
  printf '  not readable: %s\n' "${unreadable[@]}"
  log "WARNING: redeploy those services (their install makes the config readable); atlas shows them as problems until then"
fi
[[ -x $LAKESHAPE ]] || log "WARNING: $LAKESHAPE not found: lakeshape jobs are shown without the tables they build"

# --- 4. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=atlas - how the server's services are wired together
After=network-online.target relay.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
# RELAY_URL and RELAY_TOKEN, if relay is installed: live state and downloads.
EnvironmentFile=-/etc/relay-client.env
ExecStart=$BIN serve -listen $LISTEN -lakeshape $LAKESHAPE -links "$LINKS"
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
systemctl enable atlas >/dev/null 2>&1
systemctl restart atlas

port=${LISTEN##*:}
log "Waiting for atlas to come up"
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet atlas && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u atlas -n 40 --no-pager; die "atlas did not start"; }
  sleep 1
done

# --- 5. What it finds --------------------------------------------------------
# Run like the service: no privileges, with relay's token.
log "Links that don't add up (atlas check)"
systemd-run --quiet --pipe --wait --collect -p DynamicUser=yes -p EnvironmentFile=-/etc/relay-client.env \
  "$BIN" check -lakeshape "$LAKESHAPE" || true

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the page from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

atlas is running.
  URL    : http://${host_ip:-localhost}:$port
  Check  : sudo systemd-run --quiet --pipe --wait --collect -p DynamicUser=yes -p EnvironmentFile=-/etc/relay-client.env $BIN check -lakeshape $LAKESHAPE
  Logs   : journalctl -u atlas -f
EOF
if [[ -n $new_login ]]; then
  echo "  Login  : $new_login   (also in $ENV_FILE; shown only this once)"
else
  echo "  Login  : in $ENV_FILE (ATLAS_AUTH)"
fi
