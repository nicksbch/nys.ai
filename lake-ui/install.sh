#!/usr/bin/env bash
#
# Install lake-ui as a systemd service on the server set up by
# ducklake-setup.sh. Normally run for you by deploy.sh; to run it by hand, copy
# a linux build of lake-ui to the server and run ON THE SERVER as root:
#   sudo bash install.sh ./lake-ui
#
# lake-ui gets its own read-only credentials, since any SQL typed in the page
# runs with them:
#   - S3      : Garage key 'lake-ui-key', read-only on the lake bucket
#   - catalog : PostgreSQL role 'lake_ui', SELECT only on the catalog tables
#
# Override defaults with env vars, e.g.:
#   sudo LISTEN='[::]:8092' bash install.sh ./lake-ui
#
# Safe to re-run: the binary and unit are replaced, the grants refreshed and
# the service restarted, but an existing /etc/lake-ui.env (keys, passwords and
# login) is left untouched.

set -euo pipefail

LISTEN="${LISTEN:-[::]:8092}"            # reachable from the LAN; use 127.0.0.1:8092 for local only
LAKE_BUCKET="${LAKE_BUCKET:-lake}"
KEY_NAME="${KEY_NAME:-lake-ui-key}"
PG_ROLE="${PG_ROLE:-lake_ui}"
UI_USER="${UI_USER:-admin}"              # basic-auth user for the web UI
ENDPOINT="${ENDPOINT:-http://127.0.0.1:3900}"

SRC_BIN="${1:-}"
BIN=/usr/local/bin/lake-ui
DUCKDB=/usr/local/bin/duckdb
GARAGE=/usr/local/bin/garage
DUCKLAKE_ENV=/etc/ducklake.env
ENV_FILE=/etc/lake-ui.env
STATE_DIR=/var/lib/lake-ui
UNIT=/etc/systemd/system/lake-ui.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 <lake-ui binary>)"
[[ -n $SRC_BIN && -f $SRC_BIN ]] || die "usage: sudo bash $0 <lake-ui binary>"
command -v systemctl >/dev/null || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"
command -v curl >/dev/null || die "curl is required (apt install curl)"
[[ -x $GARAGE ]] || die "$GARAGE not found: run garage-setup.sh first"
"$GARAGE" status >/dev/null 2>&1 || die "garage is not running (systemctl status garage)"
[[ -x $DUCKDB && -f $DUCKLAKE_ENV ]] || die "$DUCKDB or $DUCKLAKE_ENV not found: run ducklake-setup.sh first"
[[ $PG_ROLE =~ ^[a-z_][a-z0-9_]*$ ]] || die "PG_ROLE must be a plain lower-case name"
cd /   # psql runs as postgres, which may not be able to read the current directory

# The catalog's database and owner, as ducklake-setup.sh made them.
read -r PG_DB PG_OWNER PG_PORT < <(set -a; . "$DUCKLAKE_ENV"; echo "$PGDATABASE $PGUSER ${PGPORT:-5432}")
[[ -n $PG_DB && -n $PG_OWNER ]] || die "PGDATABASE/PGUSER missing from $DUCKLAKE_ENV"

# --- 1. Binary ---------------------------------------------------------------
chmod +x "$SRC_BIN"
"$SRC_BIN" -h >/dev/null 2>&1 || die "$SRC_BIN is not a working lake-ui binary for $(uname -m)"
log "Installing $BIN"
install -m 0755 -o root -g root "$SRC_BIN" "$BIN"

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
PGHOST=127.0.0.1
PGPORT=$PG_PORT
PGDATABASE=$PG_DB
PGUSER=$PG_ROLE
PGPASSWORD=$(openssl rand -hex 24)
LAKE_UI_CATALOG=ducklake:postgres:dbname=$PG_DB
LAKE_UI_AUTH=$new_login
EOF
  umask 022
fi
# Holds the S3 secret and both passwords. systemd reads it as root, so the
# service itself needs no access.
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"
PG_PASSWORD=$(grep '^PGPASSWORD=' "$ENV_FILE" | cut -d= -f2-)
[[ $PG_PASSWORD =~ ^[0-9a-f]+$ ]] || die "PGPASSWORD in $ENV_FILE must be hex"

# --- 4. Read-only catalog role -----------------------------------------------
# SELECT on every table the owner has now, and by default on the ones it
# creates later (DuckLake adds tables for inlined data as it goes). The
# password is hex, so it is safe inside the SQL literal; it goes in on stdin.
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

# --- 5. systemd unit ---------------------------------------------------------
log "Installing $UNIT"
cat >"$UNIT" <<EOF
[Unit]
Description=lake-ui: query the DuckLake from the browser
After=network-online.target garage.service postgresql.service
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
Environment=HOME=$STATE_DIR
ExecStart=$BIN -listen $LISTEN -state-dir $STATE_DIR -duckdb $DUCKDB
DynamicUser=yes
StateDirectory=lake-ui
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
systemctl enable lake-ui >/dev/null 2>&1
systemctl restart lake-ui

# The first start downloads DuckDB's httpfs, ducklake and postgres extensions.
# A query that lists the lake's tables checks the read-only catalog role and
# the S3 key end to end.
port=${LISTEN##*:}
auth=$(grep '^LAKE_UI_AUTH=' "$ENV_FILE" | cut -d= -f2- || true)
log "Waiting for lake-ui to come up"
for i in {1..90}; do
  if out=$(curl -sS -m 30 -u "$auth" -H 'X-Lake-UI: 1' -H 'Content-Type: application/json' \
      -d '{"sql":"SELECT count(*) AS tables FROM duckdb_tables() WHERE database_name = '\''lake'\''"}' \
      -w '\n%{http_code}' "http://127.0.0.1:$port/api/query" 2>/dev/null); then
    code=${out##*$'\n'}
    [[ $code == 200 ]] && break
    # Up, but the query failed: waiting won't help.
    [[ $code =~ ^[45] ]] && { printf '%s\n' "${out%$'\n'*}"; die "lake-ui is running but can't query the lake (see above)"; }
  fi
  if ! systemctl is-active --quiet lake-ui || [[ $i -eq 90 ]]; then
    journalctl -u lake-ui -n 40 --no-pager
    die "lake-ui did not start"
  fi
  sleep 1
done

# --- Done --------------------------------------------------------------------
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  log "ufw is active: to reach the UI from the LAN run 'sudo ufw allow $port/tcp'"
fi

host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

lake-ui is running.
  URL     : http://${host_ip:-localhost}:$port
  Config  : $ENV_FILE
  Logs    : journalctl -u lake-ui -f
EOF
if [[ -n $new_login ]]; then
  cat <<EOF
  Login   : ${new_login%%:*} / ${new_login#*:}   <- save this; also in $ENV_FILE
EOF
else
  echo "  Login   : sudo grep LAKE_UI_AUTH $ENV_FILE"
fi
