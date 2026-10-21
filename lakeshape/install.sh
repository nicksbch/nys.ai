#!/usr/bin/env bash
#
# Install lakeshape as a systemd service on the server, with a project, after
# ducklake-setup.sh. Normally run for you by deploy.sh; to run it by hand,
# copy a linux build of lakeshape and a .tgz of your project to the server and
# run ON THE SERVER as root:
#   sudo bash install.sh ./lakeshape ./project.tgz
#
# The service builds the project's models into the lake (the prod target),
# running the jobs in its jobs.yml. Override defaults with env vars, e.g.:
#   sudo LAKE_BUCKET=lake bash install.sh ./lakeshape ./project.tgz
#
# Safe to re-run: the binary, project and unit are replaced. The service is
# restarted when the binary or unit changed (a run in progress rolls back),
# and only reloaded (between runs) when just the project did. An existing
# /etc/lakeshape.env is left untouched.

set -euo pipefail

LAKE_BUCKET="${LAKE_BUCKET:-lake}"         # bucket the lake's data is in
KEY_NAME="${KEY_NAME:-lakeshape-key}"
ENDPOINT="${ENDPOINT:-http://127.0.0.1:3900}"
DUCKDB_VERSION="${DUCKDB_VERSION:-v1.5.6}" # 1.5 or newer; keep in step with ducklake-setup.sh

SRC_BIN="${1:-}"
SRC_PROJECT="${2:-}"
BIN=/usr/local/bin/lakeshape
CTL=/usr/local/bin/lakeshapectl
DUCKDB=/usr/local/bin/duckdb
GARAGE=/usr/local/bin/garage
LAKE_ENV=/etc/ducklake.env
ENV_FILE=/etc/lakeshape.env
STATE_DIR=/var/lib/lakeshape
PROJECT_DIR=$STATE_DIR/project
UNIT=/etc/systemd/system/lakeshape.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <lakeshape binary> <project.tgz>)"
[[ -n $SRC_BIN && -f $SRC_BIN && -n $SRC_PROJECT && -f $SRC_PROJECT ]] || die "usage: sudo bash $0 <lakeshape binary> <project.tgz>"
command -v systemctl >/dev/null || die "systemd is required"
command -v curl >/dev/null || die "curl is required (apt install curl)"
[[ -x $GARAGE ]] || die "$GARAGE not found: run garage-setup.sh first"
"$GARAGE" status >/dev/null 2>&1 || die "garage is not running (systemctl status garage)"
[[ -f $LAKE_ENV ]] || die "$LAKE_ENV not found: run ducklake-setup.sh first"

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
"$SRC_BIN" -h >/dev/null 2>&1 || [[ $? -eq 2 ]] || die "$SRC_BIN is not a working lakeshape binary for $(uname -m)"
new_binary=1
if [[ -x $BIN ]] && cmp -s "$SRC_BIN" "$BIN"; then new_binary=0; fi
log "Installing $BIN and $CTL"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"
# CLI commands must run as the service user, with its credentials and project.
cat >"$CTL" <<EOF
#!/bin/sh
# Run lakeshape commands the way the service runs, e.g.: sudo lakeshapectl trigger nightly
exec systemd-run --quiet --pipe --wait --collect -p EnvironmentFile=$ENV_FILE -p EnvironmentFile=-/etc/relay-client.env \\
  -p User=lakeshape -p Environment=HOME=$STATE_DIR -p Environment=USER=lakeshape \\
  $BIN -project $PROJECT_DIR -state-dir $STATE_DIR -duckdb $DUCKDB "\$@"
EOF
chmod 0755 "$CTL"

# --- 2. Service user ---------------------------------------------------------
if ! id lakeshape >/dev/null 2>&1; then
  log "Creating system user 'lakeshape'"
  useradd --system --home-dir "$STATE_DIR" --no-create-home --shell /usr/sbin/nologin lakeshape
fi
install -d -m 0750 -o lakeshape -g lakeshape "$STATE_DIR"

# --- 3. S3 key and credentials -----------------------------------------------
if "$GARAGE" key info "$KEY_NAME" >/dev/null 2>&1; then
  log "Key '$KEY_NAME' already exists"
else
  log "Creating key '$KEY_NAME'"
  "$GARAGE" key create "$KEY_NAME" >/dev/null
fi
log "Allowing '$KEY_NAME' to read/write bucket '$LAKE_BUCKET'"
"$GARAGE" bucket allow --read --write "$LAKE_BUCKET" --key "$KEY_NAME"

if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE (the catalog's PG* settings come from $LAKE_ENV)"
  info=$("$GARAGE" key info --show-secret "$KEY_NAME")
  key_id=$(grep -oE 'GK[0-9a-f]{24}' <<<"$info" | head -n1 || true)
  secret=$(grep -oE '\b[0-9a-f]{64}\b' <<<"$info" | head -n1 || true)
  [[ -n $key_id && -n $secret ]] || die "could not read key ID/secret from 'garage key info --show-secret $KEY_NAME'"
  umask 077
  {
    grep -E '^PG(HOST|PORT|DATABASE|USER|PASSWORD)=' "$LAKE_ENV"
    printf '%s\n' "AWS_ACCESS_KEY_ID=$key_id" "AWS_SECRET_ACCESS_KEY=$secret" "AWS_REGION=garage" \
      "AWS_ENDPOINT_URL=$ENDPOINT" "AWS_EC2_METADATA_DISABLED=true"
  } >"$ENV_FILE"
  umask 022
fi
# Holds the secrets. systemd reads it as root, so the service needs no access.
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# --- 4. Project --------------------------------------------------------------
# Unpacked next to the old one, then swapped in, so the service never reads a
# half-written project.
log "Installing the project in $PROJECT_DIR"
rm -rf "$PROJECT_DIR.new" "$PROJECT_DIR.old"
install -d -m 0750 -o root -g lakeshape "$PROJECT_DIR.new"
tar -xzf "$SRC_PROJECT" -C "$PROJECT_DIR.new" --no-same-owner
chown -R root:lakeshape "$PROJECT_DIR.new"
chmod -R u=rwX,g=rX,o= "$PROJECT_DIR.new"
new_project=1
if [[ -d $PROJECT_DIR ]] && diff -rq "$PROJECT_DIR" "$PROJECT_DIR.new" >/dev/null; then new_project=0; fi
if [[ -d $PROJECT_DIR ]]; then mv "$PROJECT_DIR" "$PROJECT_DIR.old"; fi
mv "$PROJECT_DIR.new" "$PROJECT_DIR"
if ! "$CTL" -target prod ls >/dev/null; then
  rm -rf "$PROJECT_DIR"
  if [[ -d $PROJECT_DIR.old ]]; then mv "$PROJECT_DIR.old" "$PROJECT_DIR"; fi
  die "the project does not load on the server (see above); the previous one is back in place"
fi
rm -rf "$PROJECT_DIR.old"

# --- 5. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT.new" <<EOF
[Unit]
Description=lakeshape: build SQL models into the DuckLake
After=network-online.target garage.service postgresql.service relay.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
# RELAY_URL and RELAY_TOKEN, if relay is installed: jobs' event triggers come
# from it, and runs are published to it.
EnvironmentFile=-/etc/relay-client.env
Environment=HOME=$STATE_DIR
ExecStart=$BIN -project $PROJECT_DIR -state-dir $STATE_DIR -duckdb $DUCKDB serve
ExecReload=/bin/kill -HUP \$MAINPID
User=lakeshape
Group=lakeshape
StateDirectory=lakeshape
StateDirectoryMode=0750
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
Restart=on-failure
RestartSec=10
# Time for a run in progress to roll back before it is killed.
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
EOF
new_unit=1
if [[ -f $UNIT ]] && cmp -s "$UNIT" "$UNIT.new"; then new_unit=0; fi
mv "$UNIT.new" "$UNIT"
systemctl daemon-reload
systemctl enable lakeshape >/dev/null 2>&1

since=$(date '+%Y-%m-%d %H:%M:%S')
wait_for=""
if ! systemctl is-active --quiet lakeshape || [[ $new_binary -eq 1 || $new_unit -eq 1 ]]; then
  log "Starting lakeshape"
  systemctl restart lakeshape
  wait_for="serving"
elif [[ $new_project -eq 1 ]]; then
  log "Reloading lakeshape (it picks up the project between runs)"
  systemctl reload lakeshape
else
  log "Nothing changed"
fi

# The first start downloads DuckDB's ducklake, postgres and httpfs extensions.
if [[ -n $wait_for ]]; then
  log "Waiting for lakeshape"
  for i in {1..60}; do
    journalctl -u lakeshape --since "$since" --no-pager -q 2>/dev/null | grep -q "$wait_for" && break
    if ! systemctl is-active --quiet lakeshape || [[ $i -eq 60 ]]; then
      journalctl -u lakeshape -n 40 --no-pager
      die "lakeshape did not start; see the log above"
    fi
    sleep 1
  done
fi

# --- Done --------------------------------------------------------------------
cat <<EOF

lakeshape is running.
  Project : $PROJECT_DIR   (ship a new one with deploy.sh)
  Models  : sudo lakeshapectl ls -target prod
  Run now : sudo lakeshapectl trigger <job> [-full-refresh]
  History : sudo lakeshapectl history [model]
  Logs    : journalctl -u lakeshape -f
  Reload  : sudo systemctl reload lakeshape   (between runs)
EOF
