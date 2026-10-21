#!/usr/bin/env bash
#
# Single-node Garage (S3-compatible object store) setup for Ubuntu/systemd, x86_64.
# Follows https://garagehq.deuxfleurs.fr/documentation/quick-start/ but with
# persistent storage and a systemd service instead of /tmp and a foreground process.
#
# Run ON THE SERVER as root:
#   sudo bash garage-setup.sh
#
# Override defaults with env vars, e.g.:
#   sudo CAPACITY=60G BUCKET=backups KEY_NAME=backups-key bash garage-setup.sh
#
# Safe to re-run: an existing /etc/garage.toml (and its secrets), layout, bucket
# and key are left untouched.

set -euo pipefail

GARAGE_VERSION="${GARAGE_VERSION:-v2.4.1}"
CAPACITY="${CAPACITY:-200G}"          # space Garage may use for data
ZONE="${ZONE:-dc1}"
BUCKET="${BUCKET:-home}"             # set to "" to skip bucket/key creation
KEY_NAME="${KEY_NAME:-home-key}"
S3_BIND="${S3_BIND:-[::]:3900}"      # reachable from the LAN; use 127.0.0.1:3900 for local only
WEB_BIND="${WEB_BIND:-[::]:3902}"

BIN=/usr/local/bin/garage
CONFIG=/etc/garage.toml
STATE_DIR=/var/lib/garage
UNIT=/etc/systemd/system/garage.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0)"
[[ $(uname -m) == x86_64 ]] || die "this script downloads the x86_64 build; found $(uname -m)"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"

# --- 1. Binary ---------------------------------------------------------------
if [[ -x $BIN ]] && "$BIN" --version 2>/dev/null | grep -q "${GARAGE_VERSION#v}"; then
  log "garage $GARAGE_VERSION already installed"
else
  log "Downloading garage $GARAGE_VERSION"
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  curl -fL --proto '=https' --tlsv1.2 -o "$tmp" \
    "https://garagehq.deuxfleurs.fr/_releases/${GARAGE_VERSION}/x86_64-unknown-linux-musl/garage"
  chmod +x "$tmp"
  "$tmp" --version >/dev/null || die "downloaded file is not a working garage binary"
  install -m 0755 -o root -g root "$tmp" "$BIN"
fi
"$BIN" --version

# --- 2. Service user ---------------------------------------------------------
if ! id garage >/dev/null 2>&1; then
  log "Creating system user 'garage'"
  useradd --system --home-dir "$STATE_DIR" --no-create-home --shell /usr/sbin/nologin garage
fi

# --- 3. Config ---------------------------------------------------------------
if [[ -e $CONFIG ]]; then
  log "$CONFIG already exists, leaving it alone"
else
  log "Writing $CONFIG"
  umask 027
  cat >"$CONFIG" <<EOF
metadata_dir = "$STATE_DIR/meta"
data_dir = "$STATE_DIR/data"
db_engine = "sqlite"

replication_factor = 1

rpc_bind_addr = "127.0.0.1:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$(openssl rand -hex 32)"

[s3_api]
s3_region = "garage"
api_bind_addr = "$S3_BIND"
root_domain = ".s3.garage.localhost"

[s3_web]
bind_addr = "$WEB_BIND"
root_domain = ".web.garage.localhost"
index = "index.html"

[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "$(openssl rand -base64 32)"
metrics_token = "$(openssl rand -base64 32)"
EOF
  umask 022
fi
# Holds the RPC secret and admin tokens: root + the service user only.
chown root:garage "$CONFIG"
chmod 0640 "$CONFIG"

# --- 4. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=Garage Data Store
After=network-online.target
Wants=network-online.target

[Service]
Environment='RUST_LOG=garage=info' 'RUST_BACKTRACE=1'
ExecStart=$BIN server
User=garage
Group=garage
StateDirectory=garage
StateDirectoryMode=0750
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
LimitNOFILE=42000
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now garage

log "Waiting for garage to come up"
for i in {1..30}; do
  "$BIN" status >/dev/null 2>&1 && break
  [[ $i -eq 30 ]] && { journalctl -u garage -n 40 --no-pager; die "garage did not start"; }
  sleep 1
done

# --- 5. Cluster layout -------------------------------------------------------
if "$BIN" status | grep -q "NO ROLE ASSIGNED"; then
  log "Assigning layout (zone=$ZONE capacity=$CAPACITY)"
  node_id=$("$BIN" node id -q | cut -d@ -f1)
  "$BIN" layout assign -z "$ZONE" -c "$CAPACITY" "$node_id"
  next=$("$BIN" layout show | grep -oE 'apply --version [0-9]+' | grep -oE '[0-9]+' | head -n1 || true)
  "$BIN" layout apply --version "${next:-1}"
else
  log "Layout already assigned"
fi
"$BIN" status

# --- 6. Bucket and key -------------------------------------------------------
if [[ -n $BUCKET ]]; then
  if "$BIN" bucket info "$BUCKET" >/dev/null 2>&1; then
    log "Bucket '$BUCKET' already exists"
  else
    log "Creating bucket '$BUCKET'"
    "$BIN" bucket create "$BUCKET"
  fi

  if "$BIN" key info "$KEY_NAME" >/dev/null 2>&1; then
    log "Key '$KEY_NAME' already exists (show secret: sudo garage key info --show-secret $KEY_NAME)"
  else
    log "Creating key '$KEY_NAME' -- save the secret key printed below"
    "$BIN" key create "$KEY_NAME"
  fi

  "$BIN" bucket allow --read --write --owner "$BUCKET" --key "$KEY_NAME"
fi

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach S3 from the LAN run 'sudo ufw allow 3900/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

Garage is running.
  S3 endpoint : http://${host_ip:-localhost}:3900   (region: garage)
  Config      : $CONFIG
  Data        : $STATE_DIR
  Logs        : journalctl -u garage -f
  CLI         : sudo garage status | bucket list | key list

awscli (~/.aws/credentials profile or env):
  AWS_ACCESS_KEY_ID=<Key ID above>
  AWS_SECRET_ACCESS_KEY=<Secret key above>
  AWS_DEFAULT_REGION=garage
  AWS_ENDPOINT_URL=http://${host_ip:-localhost}:3900
EOF
