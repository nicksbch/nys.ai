#!/usr/bin/env bash
#
# Install garage-ui as a systemd service on the Garage server set up by
# garage-setup.sh. Normally run for you by deploy.sh; to run it by hand, copy a
# linux build of garage-ui to the server and run ON THE SERVER as root:
#   sudo bash install.sh ./garage-ui
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN='[::]:8080' BUCKETS="home photos" bash install.sh ./garage-ui
#
# Safe to re-run: the binary and unit are replaced and the service restarted,
# but an existing /etc/garage-ui.env (S3 key and login) is left untouched.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8080}"            # reachable from the LAN; use 127.0.0.1:8080 for local only
BUCKETS="${BUCKETS:-home lake}"          # buckets the UI's key may read and write
KEY_NAME="${KEY_NAME:-garage-ui-key}"
UI_USER="${UI_USER:-admin}"              # basic-auth user for the web UI
ENDPOINT="${ENDPOINT:-http://127.0.0.1:3900}"

SRC_BIN="${1:-}"
BIN=/usr/local/bin/garage-ui
GARAGE=/usr/local/bin/garage
ENV_FILE=/etc/garage-ui.env
UNIT=/etc/systemd/system/garage-ui.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <garage-ui binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <garage-ui binary>"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"
[[ -x $GARAGE ]] || die "$GARAGE not found: run garage-setup.sh first"
"$GARAGE" status >/dev/null 2>&1 || die "garage is not running (systemctl status garage)"

# --- 1. Binary ---------------------------------------------------------------
chmod +x "$SRC_BIN"
"$SRC_BIN" -h >/dev/null 2>&1 || die "$SRC_BIN is not a working garage-ui binary for $(uname -m)"
log "Installing $BIN"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"

# --- 2. S3 key and login -----------------------------------------------------
if "$GARAGE" key info "$KEY_NAME" >/dev/null 2>&1; then
  log "Key '$KEY_NAME' already exists"
else
  log "Creating key '$KEY_NAME'"
  "$GARAGE" key create "$KEY_NAME" >/dev/null
fi

for b in $BUCKETS; do
  log "Allowing '$KEY_NAME' to read/write bucket '$b'"
  "$GARAGE" bucket allow --read --write "$b" --key "$KEY_NAME"
done

new_login=
if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE"
  info=$("$GARAGE" key info --show-secret "$KEY_NAME")
  key_id=$(grep -oE 'GK[0-9a-f]{24}' <<<"$info" | head -n1 || true)
  secret=$(grep -oE '\b[0-9a-f]{64}\b' <<<"$info" | head -n1 || true)
  [[ -n $key_id && -n $secret ]] || die "could not read key ID/secret from 'garage key info --show-secret $KEY_NAME'"
  new_login="$UI_USER:$(openssl rand -base64 24 | tr -d '/+=')"
  umask 077
  cat >"$ENV_FILE" <<EOF
AWS_ACCESS_KEY_ID=$key_id
AWS_SECRET_ACCESS_KEY=$secret
AWS_REGION=garage
AWS_ENDPOINT_URL=$ENDPOINT
AWS_EC2_METADATA_DISABLED=true
GARAGE_UI_AUTH=$new_login
EOF
  umask 022
fi
# Holds the S3 secret and the UI password. systemd reads it as root, so the
# service itself needs no access.
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# --- 3. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=Garage web UI
After=network-online.target garage.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
ExecStart=$BIN -listen $LISTEN
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
systemctl enable garage-ui >/dev/null 2>&1
systemctl restart garage-ui

port=${LISTEN##*:}
log "Waiting for garage-ui to come up"
auth=$(grep '^GARAGE_UI_AUTH=' "$ENV_FILE" | cut -d= -f2- || true)
for i in {1..30}; do
  if command -v curl >/dev/null; then
    curl -fsS -u "$auth" "http://127.0.0.1:$port/api/buckets" >/dev/null 2>&1 && break
  else
    systemctl is-active --quiet garage-ui && break
  fi
  [[ $i -eq 30 ]] && { journalctl -u garage-ui -n 40 --no-pager; die "garage-ui did not start"; }
  sleep 1
done

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the UI from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

garage-ui is running.
  URL     : http://${host_ip:-localhost}:$port
  Config  : $ENV_FILE
  Logs    : journalctl -u garage-ui -f
EOF
if [[ -n $new_login ]]; then
  cat <<EOF
  Login   : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE
EOF
else
  echo "  Login   : sudo grep GARAGE_UI_AUTH $ENV_FILE"
fi
