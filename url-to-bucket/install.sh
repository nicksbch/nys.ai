#!/usr/bin/env bash
#
# Install url-to-bucket as a systemd service that takes download requests
# from relay and serves a web page to make them and follow their progress. Normally run for you by deploy.sh; to run it by hand, copy a
# linux build of url-to-bucket to the server and run ON THE SERVER as root:
#   sudo bash install.sh ./url-to-bucket
#
# Override defaults with env vars, e.g.:
#   sudo BUCKETS="home isos" WORKERS=3 bash install.sh ./url-to-bucket
#
# Safe to re-run: the binary and unit are replaced, BUCKETS are granted again
# and the service restarted, but an existing /etc/url-to-bucket.env (S3 key
# and page login) is left untouched, apart from adding a login it lacks.

set -euo pipefail

BUCKETS="${BUCKETS:-home}"                  # buckets the service's key may write to
KEY_NAME="${KEY_NAME:-url-to-bucket-key}"
WORKERS="${WORKERS:-2}"                     # downloads at the same time
ENDPOINT="${ENDPOINT:-http://127.0.0.1:3900}"
LISTEN="${LISTEN:-[::]:8093}"               # the web page; use 127.0.0.1:8093 for local only
UI_USER="${UI_USER:-admin}"                 # basic-auth user for the web page

SRC_BIN="${1:-}"
BIN=/usr/local/bin/url-to-bucket          # also the CLI, for every user on the server
GARAGE=/usr/local/bin/garage
ENV_FILE=/etc/url-to-bucket.env
RELAY_ENV=/etc/relay-client.env
UNIT=/etc/systemd/system/url-to-bucket.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <url-to-bucket binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <url-to-bucket binary>"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"
[[ -x $GARAGE ]] || die "$GARAGE not found: run garage-setup.sh first"
"$GARAGE" status >/dev/null 2>&1 || die "garage is not running (systemctl status garage)"
[[ -r $RELAY_ENV ]] || die "$RELAY_ENV not found: deploy relay first"

# --- 1. Binary ---------------------------------------------------------------
chmod +x "$SRC_BIN"
"$SRC_BIN" -h >/dev/null 2>&1 || die "$SRC_BIN is not a working url-to-bucket binary for $(uname -m)"
log "Installing $BIN"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"

# --- 2. S3 key ---------------------------------------------------------------
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

if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE"
  info=$("$GARAGE" key info --show-secret "$KEY_NAME")
  key_id=$(grep -oE 'GK[0-9a-f]{24}' <<<"$info" | head -n1 || true)
  secret=$(grep -oE '\b[0-9a-f]{64}\b' <<<"$info" | head -n1 || true)
  [[ -n $key_id && -n $secret ]] || die "could not read key ID/secret from 'garage key info --show-secret $KEY_NAME'"
  umask 077
  cat >"$ENV_FILE" <<EOF
AWS_ACCESS_KEY_ID=$key_id
AWS_SECRET_ACCESS_KEY=$secret
AWS_REGION=garage
AWS_ENDPOINT_URL=$ENDPOINT
AWS_EC2_METADATA_DISABLED=true
EOF
  umask 022
fi
new_login=
if ! grep -q '^URL_TO_BUCKET_UI_AUTH=' "$ENV_FILE"; then
  log "Adding a login for the web page to $ENV_FILE"
  new_login="$UI_USER:$(openssl rand -base64 24 | tr -d '/+=')"
  echo "URL_TO_BUCKET_UI_AUTH=$new_login" >>"$ENV_FILE"
fi
# Holds the S3 secret and the page password. systemd reads it as root, so the
# service needs no access.
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# --- 3. systemd unit ---------------------------------------------------------
# The relay URL and token come from $RELAY_ENV, which the binary reads itself.
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=url-to-bucket - downloads URLs requested through relay into S3, with a web page
After=network-online.target garage.service relay.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
ExecStart=$BIN serve -workers $WORKERS -listen $LISTEN
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
systemctl enable url-to-bucket >/dev/null 2>&1
systemctl restart url-to-bucket

port=${LISTEN##*:}
log "Waiting for url-to-bucket to come up"
inv=$(systemctl show -p InvocationID --value url-to-bucket)
for i in {1..15}; do
  journalctl _SYSTEMD_INVOCATION_ID="$inv" --no-pager 2>/dev/null | grep -q "reading requests" && break
  [[ $i -eq 15 ]] && { journalctl -u url-to-bucket -n 40 --no-pager; die "url-to-bucket did not connect to relay"; }
  sleep 1
done
if command -v curl >/dev/null; then
  curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 || die "the web page is not answering on port $port"
fi
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the page from the LAN run 'sudo ufw allow $port/tcp'"
fi

# --- Done --------------------------------------------------------------------
host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

url-to-bucket is running.
  Page    : http://${host_ip:-localhost}:$port
  Request : relay pub url-to-bucket.download requested -run-id ID -data '{"url":"…","bucket":"${BUCKETS%% *}"}'
  Result  : relay wait -run-id ID -type ok,failed
  Buckets : $BUCKETS (others fail with access denied; add with BUCKETS=…)
  Config  : $ENV_FILE
  Logs    : journalctl -u url-to-bucket -f
EOF
if [[ -n $new_login ]]; then
  cat <<EOF
  Login   : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE
EOF
else
  echo "  Login   : sudo grep URL_TO_BUCKET_UI_AUTH $ENV_FILE"
fi
