#!/usr/bin/env bash
#
# Install semantic (the Boring Semantic Layer's MCP server over the lake's
# marts) as a systemd service on the server set up by ducklake-setup.sh.
# Normally run for you by deploy.sh; to run it by hand, copy a .tgz of this
# directory's files to the server and run ON THE SERVER as root:
#   sudo bash install.sh ./app.tgz
#
# semantic gets its own read-only credentials, like lake-ui:
#   - S3      : Garage key 'semantic-key', read-only on the lake bucket
#   - catalog : PostgreSQL role 'semantic', SELECT only on the catalog tables
#
# Python and the dependencies come from uv.lock, installed with uv under
# /opt/semantic. The new app is checked against the lake with the service's
# credentials before it replaces the old one.
#
# Safe to re-run: the app and unit are replaced, the grants refreshed and the
# service restarted, but an existing /etc/semantic.env (keys, passwords and
# MCP token) is left untouched.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8097}"            # reachable from the LAN; use 127.0.0.1:8097 for local only
LAKE_BUCKET="${LAKE_BUCKET:-lake}"
KEY_NAME="${KEY_NAME:-semantic-key}"
PG_ROLE="${PG_ROLE:-semantic}"
ENDPOINT="${ENDPOINT:-http://127.0.0.1:3900}"
UV_VERSION=0.9.5

SRC_APP="${1:-}"
OPT=/opt/semantic
APP=$OPT/app
UV=$OPT/bin/uv
GARAGE=/usr/local/bin/garage
DUCKLAKE_ENV=/etc/ducklake.env
ENV_FILE=/etc/semantic.env
UNIT=/etc/systemd/system/semantic.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <app.tgz>)"
[[ -n $SRC_APP && -f $SRC_APP ]] || die "usage: sudo bash $0 <app.tgz>"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"
command -v curl >/dev/null || die "curl is required (apt install curl)"
[[ -x $GARAGE ]] || die "$GARAGE not found: run garage-setup.sh first"
"$GARAGE" status >/dev/null 2>&1 || die "garage is not running (systemctl status garage)"
[[ -f $DUCKLAKE_ENV ]] || die "$DUCKLAKE_ENV not found: run ducklake-setup.sh first"
[[ $PG_ROLE =~ ^[a-z_][a-z0-9_]*$ ]] || die "PG_ROLE must be a plain lower-case name"
cd /   # psql runs as postgres, which may not be able to read the current directory

# The catalog's database and owner, as ducklake-setup.sh made them.
read -r PG_DB PG_OWNER PG_PORT < <(set -a; . "$DUCKLAKE_ENV"; echo "$PGDATABASE $PGUSER ${PGPORT:-5432}")
[[ -n $PG_DB && -n $PG_OWNER ]] || die "PGDATABASE/PGUSER missing from $DUCKLAKE_ENV"

# --- 1. uv -------------------------------------------------------------------
# uv and the Python it installs live under $OPT, readable by the service.
if [[ -x $UV ]] && "$UV" --version 2>/dev/null | grep -q "^uv $UV_VERSION "; then
  log "uv $UV_VERSION already installed"
else
  log "Downloading uv $UV_VERSION"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  curl -fL --proto '=https' --tlsv1.2 \
    "https://github.com/astral-sh/uv/releases/download/$UV_VERSION/uv-$(uname -m)-unknown-linux-gnu.tar.gz" |
    tar -xz -C "$tmp" --strip-components=1
  "$tmp/uv" --version >/dev/null || die "downloaded file is not a working uv binary"
  install -d -m 0755 "$OPT/bin"
  install -m 0755 -o root -g root "$tmp/uv" "$UV"
fi
export UV_PYTHON_INSTALL_DIR=$OPT/python UV_CACHE_DIR=$OPT/cache UV_NO_CONFIG=1

# --- 2. Read-only S3 key -----------------------------------------------------
if "$GARAGE" key info "$KEY_NAME" >/dev/null 2>&1; then
  log "Key '$KEY_NAME' already exists"
else
  log "Creating key '$KEY_NAME'"
  "$GARAGE" key create "$KEY_NAME" >/dev/null
fi
log "Allowing '$KEY_NAME' to read bucket '$LAKE_BUCKET'"
"$GARAGE" bucket allow --read "$LAKE_BUCKET" --key "$KEY_NAME"

# --- 3. Secrets file ---------------------------------------------------------
new_token=
if [[ -e $ENV_FILE ]]; then
  log "$ENV_FILE already exists, leaving it alone"
else
  log "Writing $ENV_FILE"
  info=$("$GARAGE" key info --show-secret "$KEY_NAME")
  key_id=$(grep -oE 'GK[0-9a-f]{24}' <<<"$info" | head -n1 || true)
  secret=$(grep -oE '\b[0-9a-f]{64}\b' <<<"$info" | head -n1 || true)
  [[ -n $key_id && -n $secret ]] || die "could not read key ID/secret from 'garage key info --show-secret $KEY_NAME'"
  new_token=$(openssl rand -hex 24)
  umask 077
  cat >"$ENV_FILE" <<EOF
AWS_ACCESS_KEY_ID=$key_id
AWS_SECRET_ACCESS_KEY=$secret
AWS_REGION=garage
AWS_ENDPOINT_URL=$ENDPOINT
PGHOST=127.0.0.1
PGPORT=$PG_PORT
PGDATABASE=$PG_DB
PGUSER=$PG_ROLE
PGPASSWORD=$(openssl rand -hex 24)
SEMANTIC_CATALOG=ducklake:postgres:dbname=$PG_DB
SEMANTIC_MCP_TOKEN=$new_token
EOF
  umask 022
fi
# Holds the S3 secret, the catalog password and the MCP token. systemd reads
# it as root, so the service itself needs no access.
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"
PG_PASSWORD=$(grep '^PGPASSWORD=' "$ENV_FILE" | cut -d= -f2-)
[[ $PG_PASSWORD =~ ^[0-9a-f]+$ ]] || die "PGPASSWORD in $ENV_FILE must be hex"

# --- 4. Read-only catalog role -----------------------------------------------
# As lake-ui's: SELECT on every table the owner has now, and by default on the
# ones it creates later. The password is hex, so it is safe inside the SQL
# literal; it goes in on stdin.
log "Granting '$PG_ROLE' read-only access to database '$PG_DB'"
runuser -u postgres -- env -u PGHOST -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD \
  psql -X -q -v ON_ERROR_STOP=1 -d "$PG_DB" <<EOF
SELECT 'CREATE ROLE "$PG_ROLE" LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$PG_ROLE')\gexec
ALTER ROLE "$PG_ROLE" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD '$PG_PASSWORD';
GRANT CONNECT ON DATABASE "$PG_DB" TO "$PG_ROLE";
DO \$\$
DECLARE s text;
BEGIN
  FOR s IN SELECT nspname FROM pg_namespace
           WHERE nspname = 'public' OR nspowner = (SELECT oid FROM pg_roles WHERE rolname = '$PG_OWNER')
  LOOP
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', s, '$PG_ROLE');
    EXECUTE format('GRANT SELECT ON ALL TABLES IN SCHEMA %I TO %I', s, '$PG_ROLE');
  END LOOP;
END
\$\$;
ALTER DEFAULT PRIVILEGES FOR ROLE "$PG_OWNER" GRANT SELECT ON TABLES TO "$PG_ROLE";
EOF

# --- 5. App ------------------------------------------------------------------
# Unpacked and synced next to the old one, checked with the service's
# credentials, then swapped in.
log "Installing the app in $APP"
rm -rf "$APP.new" "$APP.old"
install -d -m 0755 "$APP.new"
tar -xzf "$SRC_APP" -C "$APP.new" --no-same-owner
(cd "$APP.new" && "$UV" sync -q --frozen --no-dev --python-preference only-managed) || die "installing the dependencies failed"
chown -R root:root "$APP.new"
chmod -R u=rwX,go=rX "$APP.new" "$OPT/python"

log "Checking the models against the lake"
rc=0
systemd-run --quiet --pipe --wait --collect -p EnvironmentFile=$ENV_FILE \
  -p DynamicUser=yes -p StateDirectory=semantic -p Environment=HOME=/var/lib/semantic \
  "$APP.new/.venv/bin/python" "$APP.new/server.py" check -state-dir /var/lib/semantic || rc=$?
case $rc in
  0) ;;
  1) log "WARNING: the lake could not be reached (see above); installing anyway" ;;
  *) rm -rf "$APP.new"; die "the models do not load against the lake (see above); the previous app is still in place" ;;
esac
# The venv runs from where it lands: its python is a symlink into $OPT/python.
if [[ -d $APP ]]; then mv "$APP" "$APP.old"; fi
mv "$APP.new" "$APP"
rm -rf "$APP.old"

# --- 6. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=semantic: the Boring Semantic Layer over the lake's marts, as an MCP server
After=network-online.target garage.service postgresql.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
Environment=HOME=/var/lib/semantic
ExecStart=$APP/.venv/bin/python $APP/server.py serve -listen $LISTEN -state-dir /var/lib/semantic
DynamicUser=yes
StateDirectory=semantic
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
systemctl enable semantic >/dev/null 2>&1
systemctl restart semantic

port=${LISTEN##*:}
log "Waiting for semantic to come up"
for i in {1..90}; do
  curl -fsS -m 3 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
  if ! systemctl is-active --quiet semantic || [[ $i -eq 90 ]]; then
    journalctl -u semantic -n 40 --no-pager
    die "semantic did not start"
  fi
  sleep 1
done

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the MCP server from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

semantic is running.
  MCP     : http://${host_ip:-localhost}:$port/mcp (token: sudo grep SEMANTIC_MCP_TOKEN $ENV_FILE)
  Models  : $APP/models.yml
  Config  : $ENV_FILE
  Logs    : journalctl -u semantic -f
EOF
if [[ -n $new_token ]]; then
  echo "  New MCP token: deploy mcp-hub to copy it there (ONLY=mcp-hub ./deploy-all.sh)"
fi
