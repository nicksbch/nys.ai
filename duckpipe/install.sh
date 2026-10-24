#!/usr/bin/env bash
#
# Install duckpipe as a systemd service on the Garage server set up by
# garage-setup.sh. Normally run for you by deploy.sh; to run it by hand, copy a
# linux build of duckpipe and duckpipe.yaml to the server and run ON THE SERVER
# as root:
#   sudo bash install.sh ./duckpipe
#
# It also installs the DuckDB CLI (/usr/local/bin/duckdb), which duckpipe runs
# its loads with and which you can use to query the tables.
#
# Override defaults with env vars, e.g.:
#   sudo SOURCE_BUCKETS="home photos" LAKE_BUCKET=lake LISTEN=127.0.0.1:8100 bash install.sh ./duckpipe
#
# Safe to re-run: the binary, /etc/duckpipe.yaml (the pipes, from the repo)
# and the unit are replaced and the service restarted. duckpipe.yaml is checked
# first: if it doesn't load, nothing is changed. An existing /etc/duckpipe.env
# (S3 key) is left untouched.

set -euo pipefail

SOURCE_BUCKETS="${SOURCE_BUCKETS:-home}"   # buckets duckpipe may read raw files from
LAKE_BUCKET="${LAKE_BUCKET:-lake}"         # bucket the tables are written to (created if missing)
KEY_NAME="${KEY_NAME:-duckpipe-key}"
ENDPOINT="${ENDPOINT:-http://127.0.0.1:3900}"
DUCKDB_VERSION="${DUCKDB_VERSION:-v1.5.6}" # 1.5 or newer
LISTEN="${LISTEN:-[::]:8100}"              # the web page; use 127.0.0.1:8100 for local only
UI_USER="${UI_USER:-admin}"                # basic-auth user for the web page

SRC_BIN="${1:-}"
SRC_CONFIG="$(dirname "${SRC_BIN:-.}")/duckpipe.yaml"   # from the repo; replaces CONFIG
BIN=/usr/local/bin/duckpipe
DUCKDB=/usr/local/bin/duckdb
CTL=/usr/local/bin/duckpipectl
GARAGE=/usr/local/bin/garage
ENV_FILE=/etc/duckpipe.env
CONFIG=/etc/duckpipe.yaml
STATE_DIR=/var/lib/duckpipe
UNIT=/etc/systemd/system/duckpipe.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <duckpipe binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <duckpipe binary>"
[[ -f $SRC_CONFIG ]] || die "$SRC_CONFIG not found: copy duckpipe.yaml next to the binary"
command -v systemctl >/dev/null || die "systemd is required"
command -v curl >/dev/null || die "curl is required (apt install curl)"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"
[[ -x $GARAGE ]] || die "$GARAGE not found: run garage-setup.sh first"
"$GARAGE" status >/dev/null 2>&1 || die "garage is not running (systemctl status garage)"

# --- 1. Binaries -------------------------------------------------------------
if [[ -x $DUCKDB ]] && "$DUCKDB" --version 2>/dev/null | grep -q "^$DUCKDB_VERSION "; then
  log "duckdb $DUCKDB_VERSION already installed"
else
  case $(uname -m) in
    x86_64) darch=amd64 ;;
    aarch64 | arm64) darch=arm64 ;;
    *) die "no DuckDB CLI build for $(uname -m)" ;;
  esac
  log "Downloading duckdb $DUCKDB_VERSION"
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  curl -fL --proto '=https' --tlsv1.2 \
    "https://github.com/duckdb/duckdb/releases/download/${DUCKDB_VERSION}/duckdb_cli-linux-${darch}.gz" | gunzip >"$tmp"
  chmod +x "$tmp"
  "$tmp" --version >/dev/null || die "downloaded file is not a working duckdb binary"
  install -m 0755 -o root -g root "$tmp" "$DUCKDB"
fi
"$DUCKDB" --version

chmod +x "$SRC_BIN"
"$SRC_BIN" -h >/dev/null 2>&1 || [[ $? -eq 2 ]] || die "$SRC_BIN is not a working duckpipe binary for $(uname -m)"
# Check the new config with the new binary before anything is replaced.
log "Checking the new duckpipe.yaml"
"$SRC_BIN" -config "$SRC_CONFIG" check || die "duckpipe.yaml does not load (see above); nothing was installed"
log "Installing $BIN and $CTL"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"
# status/retry must run as the service user with the service's key: as root
# they could leave root-owned catalog files behind that the service can't open.
cat >"$CTL" <<EOF
#!/bin/sh
# Run duckpipe commands the way the service runs, e.g.: sudo duckpipectl status
exec systemd-run --quiet --pipe --wait --collect -p EnvironmentFile=$ENV_FILE \\
  -p User=duckpipe -p Environment=HOME=$STATE_DIR $BIN -config $CONFIG -state-dir $STATE_DIR -duckdb $DUCKDB "\$@"
EOF
chmod 0755 "$CTL"

# --- 2. Service user ---------------------------------------------------------
# A real (system) user rather than DynamicUser, so the catalog in $STATE_DIR
# has a stable owner you can query as: sudo -u duckpipe duckdb
if ! id duckpipe >/dev/null 2>&1; then
  log "Creating system user 'duckpipe'"
  useradd --system --home-dir "$STATE_DIR" --no-create-home --shell /usr/sbin/nologin duckpipe
fi

# --- 3. Buckets and S3 key ---------------------------------------------------
if "$GARAGE" bucket info "$LAKE_BUCKET" >/dev/null 2>&1; then
  log "Bucket '$LAKE_BUCKET' already exists"
else
  log "Creating bucket '$LAKE_BUCKET'"
  "$GARAGE" bucket create "$LAKE_BUCKET"
fi

if "$GARAGE" key info "$KEY_NAME" >/dev/null 2>&1; then
  log "Key '$KEY_NAME' already exists"
else
  log "Creating key '$KEY_NAME'"
  "$GARAGE" key create "$KEY_NAME" >/dev/null
fi

for b in $SOURCE_BUCKETS; do
  [[ $b == "$LAKE_BUCKET" ]] && continue
  log "Allowing '$KEY_NAME' to read bucket '$b'"
  "$GARAGE" bucket allow --read "$b" --key "$KEY_NAME"
done
log "Allowing '$KEY_NAME' to read/write bucket '$LAKE_BUCKET'"
"$GARAGE" bucket allow --read --write "$LAKE_BUCKET" --key "$KEY_NAME"

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
if ! grep -q '^DUCKPIPE_UI_AUTH=' "$ENV_FILE"; then
  log "Adding a login for the web page to $ENV_FILE"
  new_login="$UI_USER:$(openssl rand -base64 24 | tr -d '/+=')"
  echo "DUCKPIPE_UI_AUTH=$new_login" >>"$ENV_FILE"
fi
# Holds the S3 secret and the page password. systemd reads it as root, so the
# service itself needs no access.
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# --- 4. Config ---------------------------------------------------------------
log "Installing $CONFIG"
# Readable by everyone: it holds no secrets (those are in $ENV_FILE), and
# atlas reads it to show how the services connect.
install -m 0644 -o root -g root "$SRC_CONFIG" "$CONFIG"

# --- 5. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=duckpipe: load new S3 files into DuckLake tables
After=network-online.target garage.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
# RELAY_URL and RELAY_TOKEN, if relay is installed; loads are published to it.
EnvironmentFile=-/etc/relay-client.env
Environment=HOME=$STATE_DIR
ExecStart=$BIN -config $CONFIG -state-dir $STATE_DIR -duckdb $DUCKDB -listen $LISTEN run
User=duckpipe
Group=duckpipe
StateDirectory=duckpipe
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
systemctl enable duckpipe >/dev/null 2>&1
systemctl restart duckpipe

# The first start downloads DuckDB's httpfs, ducklake and sqlite extensions.
log "Waiting for duckpipe to come up"
for i in {1..60}; do
  journalctl -u duckpipe --since "-2min" --no-pager -q 2>/dev/null | grep -q "watching" && break
  if ! systemctl is-active --quiet duckpipe || [[ $i -eq 60 ]]; then
    journalctl -u duckpipe -n 40 --no-pager
    die "duckpipe did not start; fix duckpipe/duckpipe.yaml in the repo and deploy again"
  fi
  sleep 1
done
port=${LISTEN##*:}
for i in {1..10}; do
  curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  [[ $i -eq 10 ]] && die "the web page is not answering on port $port"
  sleep 1
done
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the page from the LAN run 'sudo ufw allow $port/tcp'"
fi

# --- Done --------------------------------------------------------------------
host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

duckpipe is running.
  Page    : http://${host_ip:-localhost}:$port   (what is loading, history, stop all)
  Config  : $CONFIG, from duckpipe/duckpipe.yaml in the repo (change it there and deploy)
  Catalog : $STATE_DIR
  Status  : sudo duckpipectl status
  Retry   : sudo duckpipectl retry [pipe]   (after fixing failed files)
  History : sudo duckpipectl history [pipe]
  Logs    : journalctl -u duckpipe -f
  Poll now: sudo systemctl kill -s HUP duckpipe
EOF
if [[ -n $new_login ]]; then
  cat <<EOF
  Login   : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE
EOF
else
  echo "  Login   : sudo grep DUCKPIPE_UI_AUTH $ENV_FILE"
fi
