#!/usr/bin/env bash
#
# Install preprocessor as a systemd service on the Garage server set up by
# garage-setup.sh. Normally run for you by deploy.sh; to run it by hand, copy a
# linux build of preprocessor and preprocessor.yaml to the server and run ON
# THE SERVER as root:
#   sudo bash install.sh ./preprocessor
#
# Override defaults with env vars, e.g.:
#   sudo BUCKETS="home photos" LISTEN=127.0.0.1:8099 bash install.sh ./preprocessor
#
# Safe to re-run: the binary, /etc/preprocessor.yaml (the jobs, from the repo)
# and the unit are replaced and the service restarted. preprocessor.yaml is
# checked first: if it doesn't load, nothing is changed. An existing
# /etc/preprocessor.env (S3 key, page login) is left untouched.

set -euo pipefail

BUCKETS="${BUCKETS:-home}"   # buckets preprocessor reads archives from and writes entries to
KEY_NAME="${KEY_NAME:-preprocessor-key}"
ENDPOINT="${ENDPOINT:-http://127.0.0.1:3900}"
LISTEN="${LISTEN:-[::]:8099}"   # the web page; use 127.0.0.1:8099 for local only
UI_USER="${UI_USER:-admin}"     # basic-auth user for the web page

SRC_BIN="${1:-}"
SRC_CONFIG="$(dirname "${SRC_BIN:-.}")/preprocessor.yaml"   # from the repo; replaces CONFIG
BIN=/usr/local/bin/preprocessor
CTL=/usr/local/bin/preprocessorctl
GARAGE=/usr/local/bin/garage
ENV_FILE=/etc/preprocessor.env
CONFIG=/etc/preprocessor.yaml
STATE_DIR=/var/lib/preprocessor
UNIT=/etc/systemd/system/preprocessor.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <preprocessor binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <preprocessor binary>"
[[ -f $SRC_CONFIG ]] || die "$SRC_CONFIG not found: copy preprocessor.yaml next to the binary"
command -v systemctl >/dev/null || die "systemd is required"
[[ -x $GARAGE ]] || die "$GARAGE not found: run garage-setup.sh first"
"$GARAGE" status >/dev/null 2>&1 || die "garage is not running (systemctl status garage)"

# --- 1. Binary ---------------------------------------------------------------
chmod +x "$SRC_BIN"
"$SRC_BIN" -h >/dev/null 2>&1 || [[ $? -eq 2 ]] || die "$SRC_BIN is not a working preprocessor binary for $(uname -m)"
# Check the new config with the new binary before anything is replaced.
log "Checking the new preprocessor.yaml"
"$SRC_BIN" -config "$SRC_CONFIG" check || die "preprocessor.yaml does not load (see above); nothing was installed"
log "Installing $BIN and $CTL"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"
# status/retry must run as the service user: as root they could leave a
# root-owned state file behind that the service can't write.
cat >"$CTL" <<EOF
#!/bin/sh
# Run preprocessor commands the way the service runs, e.g.: sudo preprocessorctl status
exec systemd-run --quiet --pipe --wait --collect -p EnvironmentFile=$ENV_FILE \\
  -p User=preprocessor $BIN -config $CONFIG -state-dir $STATE_DIR "\$@"
EOF
chmod 0755 "$CTL"

# --- 2. Service user ---------------------------------------------------------
# A real (system) user rather than DynamicUser, so $STATE_DIR has a stable
# owner across reinstalls.
if ! id preprocessor >/dev/null 2>&1; then
  log "Creating system user 'preprocessor'"
  useradd --system --home-dir "$STATE_DIR" --no-create-home --shell /usr/sbin/nologin preprocessor
fi

# --- 3. S3 key ---------------------------------------------------------------
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
if ! grep -q '^PREPROCESSOR_UI_AUTH=' "$ENV_FILE"; then
  log "Adding a login for the web page to $ENV_FILE"
  new_login="$UI_USER:$(openssl rand -base64 24 | tr -d '/+=')"
  echo "PREPROCESSOR_UI_AUTH=$new_login" >>"$ENV_FILE"
fi
# Holds the S3 secret and the page password. systemd reads it as root, so the
# service itself needs no access.
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# --- 4. Config ---------------------------------------------------------------
log "Installing $CONFIG"
install -m 0640 -o root -g preprocessor "$SRC_CONFIG" "$CONFIG"

# --- 5. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=preprocessor: unzip new S3 archives into another prefix
After=network-online.target garage.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
# RELAY_URL and RELAY_TOKEN, if relay is installed; archives are published to it.
EnvironmentFile=-/etc/relay-client.env
ExecStart=$BIN -config $CONFIG -state-dir $STATE_DIR -listen $LISTEN run
User=preprocessor
Group=preprocessor
# Archives are downloaded to $STATE_DIR/tmp while they are unzipped: it needs
# room for the largest one.
StateDirectory=preprocessor
StateDirectoryMode=0750
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable preprocessor >/dev/null 2>&1
systemctl restart preprocessor

log "Waiting for preprocessor to come up"
for i in {1..30}; do
  journalctl -u preprocessor --since "-2min" --no-pager -q 2>/dev/null | grep -q "watching" && break
  if ! systemctl is-active --quiet preprocessor || [[ $i -eq 30 ]]; then
    journalctl -u preprocessor -n 40 --no-pager
    die "preprocessor did not start; fix preprocessor/preprocessor.yaml in the repo and deploy again"
  fi
  sleep 1
done
port=${LISTEN##*:}
if command -v curl >/dev/null; then
  for i in {1..10}; do
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
    [[ $i -eq 10 ]] && die "the web page is not answering on port $port"
    sleep 1
  done
fi
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the page from the LAN run 'sudo ufw allow $port/tcp'"
fi

# --- Done --------------------------------------------------------------------
host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

preprocessor is running.
  Page    : http://${host_ip:-localhost}:$port   (state, history, poll now, extract again)
  Config  : $CONFIG, from preprocessor/preprocessor.yaml in the repo (change it there and deploy)
  State   : $STATE_DIR/state.json
  Status  : sudo preprocessorctl status
  Retry   : sudo preprocessorctl retry [job]   (after fixing failed archives)
  History : sudo preprocessorctl history [job]
  Logs    : journalctl -u preprocessor -f
  Poll now: sudo systemctl kill -s HUP preprocessor
EOF
if [[ -n $new_login ]]; then
  cat <<EOF
  Login   : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE
EOF
else
  echo "  Login   : sudo grep PREPROCESSOR_UI_AUTH $ENV_FILE"
fi
