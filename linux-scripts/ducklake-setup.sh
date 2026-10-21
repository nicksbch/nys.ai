#!/usr/bin/env bash
#
# DuckLake (https://ducklake.select/docs/stable/) on the Garage server:
#   - catalog : PostgreSQL (database "ducklake"), reachable from localhost and the LAN
#   - data    : Parquet files in the Garage bucket s3://lake/
#   - client  : DuckDB CLI + "sudo ducklake", which opens the lake attached as "lake"
# PostgreSQL is the catalog DuckLake recommends for several clients, local or
# remote, writing at the same time (duckpipe, your Mac, other apps).
#
# Run ON THE SERVER as root, after garage-setup.sh:
#   sudo bash ducklake-setup.sh
#
# Override defaults with env vars, e.g.:
#   sudo LAN_CIDR=192.168.1.0/24 LAKE_BUCKET=lake bash ducklake-setup.sh
#
# Safe to re-run: /etc/ducklake.env (passwords and S3 key) is kept, the role,
# database, bucket, key and catalog are only created when missing.
#
# If /etc/duckpipe.yaml still points at an SQLite catalog, duckpipe is
# switched to this lake. Tables already in the SQLite catalog are copied over
# first (the SQLite file is kept as a backup).

set -euo pipefail

LAKE_BUCKET="${LAKE_BUCKET:-lake}"
KEY_NAME="${KEY_NAME:-ducklake-key}"
PG_DB="${PG_DB:-ducklake}"
PG_USER="${PG_USER:-ducklake}"
S3_ENDPOINT="${S3_ENDPOINT:-http://127.0.0.1:3900}"
DUCKDB_VERSION="${DUCKDB_VERSION:-v1.5.6}"   # keep in step with duckpipe's install.sh
# LAN that may reach PostgreSQL (the default route's subnet); empty = localhost only.
lan_dev=$(ip -o -4 route show default 2>/dev/null | awk '{for (i=1; i<NF; i++) if ($i == "dev") {print $(i+1); exit}}')
lan_route=$(ip -o -4 route list scope link proto kernel dev "${lan_dev:-lo}" 2>/dev/null | head -n1)
LAN_CIDR="${LAN_CIDR-$(awk '{print $1}' <<<"$lan_route")}"
HOST_IP="${HOST_IP:-$(awk '{for (i=1; i<NF; i++) if ($i == "src") print $(i+1)}' <<<"$lan_route")}"

DUCKDB=/usr/local/bin/duckdb
WRAPPER=/usr/local/bin/ducklake
GARAGE=/usr/local/bin/garage
ENV_FILE=/etc/ducklake.env
CATALOG="ducklake:postgres:dbname=$PG_DB"   # host/user/password come from the PG* vars in $ENV_FILE

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0)"
command -v systemctl >/dev/null || die "systemd is required"
command -v apt-get >/dev/null || die "apt-get is required (Ubuntu/Debian)"
command -v openssl >/dev/null || die "openssl is required (apt install openssl)"
[[ -x $GARAGE ]] || die "$GARAGE not found: run garage-setup.sh first"
"$GARAGE" status >/dev/null 2>&1 || die "garage is not running (systemctl status garage)"
[[ -z $LAN_CIDR || -n $HOST_IP ]] || die "could not find this host's LAN address; set HOST_IP"
cd /   # psql runs as postgres, which may not be able to read the current directory

# --- 1. DuckDB CLI -----------------------------------------------------------
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

# --- 2. PostgreSQL -----------------------------------------------------------
if ! command -v pg_lsclusters >/dev/null; then
  log "Installing PostgreSQL"
  DEBIAN_FRONTEND=noninteractive apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql >/dev/null
fi
PG_VER=$(pg_lsclusters -h | awk '$2 == "main" {print $1}' | sort -V | tail -n1)
[[ -n $PG_VER ]] || die "no 'main' PostgreSQL cluster found (pg_lsclusters)"
PG_CONF_DIR=/etc/postgresql/$PG_VER/main
log "PostgreSQL $PG_VER"

restart=0
listen=localhost
[[ -n $LAN_CIDR ]] && listen="localhost,$HOST_IP"
conf=$PG_CONF_DIR/conf.d/ducklake.conf
want="listen_addresses = '$listen'"
if [[ ! -e $conf ]] || [[ $(cat "$conf") != "$want" ]]; then
  log "Listening on $listen"
  printf '%s\n' "$want" >"$conf"
  restart=1
fi

hba=$PG_CONF_DIR/pg_hba.conf
if [[ -n $LAN_CIDR ]] && ! grep -qE "^host[[:space:]]+$PG_DB[[:space:]]+$PG_USER[[:space:]]+$LAN_CIDR[[:space:]]" "$hba"; then
  log "Allowing $PG_USER@$PG_DB from $LAN_CIDR (password)"
  printf 'host    %s    %s    %s    scram-sha-256\n' "$PG_DB" "$PG_USER" "$LAN_CIDR" >>"$hba"
  restart=1
fi

systemctl enable postgresql >/dev/null 2>&1
if [[ $restart -eq 1 ]]; then
  systemctl restart "postgresql@$PG_VER-main"
else
  systemctl start "postgresql@$PG_VER-main"
fi
for i in {1..30}; do
  runuser -u postgres -- pg_isready -q && break
  [[ $i -eq 30 ]] && { journalctl -u "postgresql@$PG_VER-main" -n 40 --no-pager; die "postgresql did not start"; }
  sleep 1
done

# --- 3. Bucket and S3 key ----------------------------------------------------
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
"$GARAGE" bucket allow --read --write "$LAKE_BUCKET" --key "$KEY_NAME"

# --- 4. Secrets file ---------------------------------------------------------
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
PGHOST=127.0.0.1
PGPORT=5432
PGDATABASE=$PG_DB
PGUSER=$PG_USER
PGPASSWORD=$(openssl rand -hex 24)
AWS_ACCESS_KEY_ID=$key_id
AWS_SECRET_ACCESS_KEY=$secret
AWS_REGION=garage
AWS_ENDPOINT_URL=$S3_ENDPOINT
EOF
  umask 022
fi
chown root:root "$ENV_FILE"
chmod 0600 "$ENV_FILE"
set -a; . "$ENV_FILE"; set +a

# --- 5. Catalog role and database --------------------------------------------
# The password is hex, so it is safe inside the SQL literal. It goes in on
# stdin, never on a command line. The PG* vars from $ENV_FILE are dropped so
# psql connects as the postgres superuser over the local socket.
log "Ensuring role '$PG_USER' and database '$PG_DB'"
runuser -u postgres -- env -u PGHOST -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD \
  psql -X -q -v ON_ERROR_STOP=1 -d postgres <<EOF
SELECT 'CREATE ROLE "$PG_USER" LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$PG_USER')\gexec
ALTER ROLE "$PG_USER" LOGIN PASSWORD '$PGPASSWORD';
SELECT 'CREATE DATABASE "$PG_DB" OWNER "$PG_USER"' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$PG_DB')\gexec
EOF

# --- 6. ducklake command -----------------------------------------------------
log "Installing $WRAPPER"
cat >"$WRAPPER" <<EOF
#!/usr/bin/env bash
# Open the DuckLake in the DuckDB CLI, attached as "lake":
#   sudo ducklake                         interactive
#   sudo ducklake -c "SHOW ALL TABLES"    one query
# The secrets come from $ENV_FILE and reach duckdb through an -init pipe,
# so they never show up in a process list.
set -euo pipefail
set -a; . $ENV_FILE; set +a
exec $DUCKDB -init <(cat <<SQL
.output /dev/null
CREATE TEMPORARY SECRET ducklake_s3 (TYPE s3, KEY_ID '\$AWS_ACCESS_KEY_ID', SECRET '\$AWS_SECRET_ACCESS_KEY',
  REGION '\$AWS_REGION', ENDPOINT '\${AWS_ENDPOINT_URL#*://}', URL_STYLE 'path', USE_SSL \$([[ \$AWS_ENDPOINT_URL == https* ]] && echo true || echo false));
ATTACH '$CATALOG' AS lake;
USE lake;
.output
SQL
) "\$@"
EOF
chmod 0755 "$WRAPPER"

# --- 7. Create the lake ------------------------------------------------------
# DATA_PATH is stored in the catalog the first time; later ATTACHes reuse it.
log "Creating / checking the lake (catalog $PG_DB, data s3://$LAKE_BUCKET/)"
s3_host=${AWS_ENDPOINT_URL#*://}
ssl=false; [[ $AWS_ENDPOINT_URL == https* ]] && ssl=true
"$DUCKDB" -bail -batch -init /dev/null :memory: <<EOF
.output /dev/null
INSTALL ducklake; INSTALL postgres; INSTALL httpfs;
CREATE TEMPORARY SECRET ducklake_s3 (TYPE s3, KEY_ID '$AWS_ACCESS_KEY_ID', SECRET '$AWS_SECRET_ACCESS_KEY',
  REGION '$AWS_REGION', ENDPOINT '$s3_host', URL_STYLE 'path', USE_SSL $ssl);
ATTACH '$CATALOG' AS lake (DATA_PATH 's3://$LAKE_BUCKET/');
.output
SELECT option_name, value FROM ducklake_options('lake') WHERE option_name IN ('version', 'data_path');
SELECT count(*) AS files_in_bucket FROM glob('s3://$LAKE_BUCKET/**');
EOF

# --- 8. duckpipe -------------------------------------------------------------
# A duckpipe installed before this lake keeps its tables in an SQLite
# catalog. They are copied into this lake (load history included, so no file
# is loaded twice) in one transaction that only commits when every table's
# row count matches; then duckpipe is switched over. The SQLite file is kept
# as a backup. Snapshots from before the copy (time travel) are not carried
# over.
DP_CONFIG=/etc/duckpipe.yaml
DP_ENV=/etc/duckpipe.env
if [[ -f $DP_CONFIG ]] && grep -qE '^[[:space:]]*catalog:[[:space:]]*ducklake:sqlite:' "$DP_CONFIG"; then
  sqlite_file=$(sed -nE 's/^[[:space:]]*catalog:[[:space:]]*ducklake:sqlite:([^[:space:]#]+).*/\1/p' "$DP_CONFIG")
  if [[ ! -f $DP_ENV ]]; then
    log "$DP_ENV not found -- run duckpipe's install.sh first, then re-run this script"
  else
    dp_active=0
    systemctl is-active --quiet duckpipe && dp_active=1
    if [[ -e $sqlite_file ]]; then
      log "Copying duckpipe's tables from $sqlite_file into this lake (duckpipe is stopped meanwhile)"
      systemctl stop duckpipe
      old="ducklake:sqlite:$sqlite_file"
      # One row-count check per table, written by DuckDB with the names quoted.
      checks=$("$DUCKDB" -bail -batch -list -noheader -init /dev/null :memory: <<EOF
.output /dev/null
INSTALL sqlite; LOAD sqlite; LOAD ducklake;
ATTACH '$old' AS old (READ_ONLY);
.output
SELECT format('SELECT CASE WHEN (SELECT count(*) FROM old.{0}) <> (SELECT count(*) FROM lake.{0}) THEN error(''row counts differ for {1}'') END;', id, replace(id, '''', ''''''))
FROM (SELECT '"' || replace(schema_name, '"', '""') || '"."' || replace(table_name, '"', '""') || '"' AS id
      FROM duckdb_tables() WHERE database_name = 'old');
EOF
      ) || { [[ $dp_active -eq 1 ]] && systemctl start duckpipe; die "could not read duckpipe's catalog $sqlite_file"; }
      if ! "$DUCKDB" -bail -batch -init /dev/null :memory: <<EOF
.output /dev/null
LOAD sqlite; LOAD ducklake; LOAD postgres; LOAD httpfs;
CREATE TEMPORARY SECRET ducklake_s3 (TYPE s3, KEY_ID '$AWS_ACCESS_KEY_ID', SECRET '$AWS_SECRET_ACCESS_KEY',
  REGION '$AWS_REGION', ENDPOINT '$s3_host', URL_STYLE 'path', USE_SSL $ssl);
ATTACH '$old' AS old (READ_ONLY);
ATTACH '$CATALOG' AS lake;
SELECT CASE WHEN count(*) > 0 THEN error('this lake already has tables, so duckpipe''s were not copied in; move them over by hand') END
FROM duckdb_tables() WHERE database_name = 'lake';
BEGIN;
COPY FROM DATABASE old TO lake;
$checks
COMMIT;
EOF
      then
        [[ $dp_active -eq 1 ]] && systemctl start duckpipe
        die "copying duckpipe's tables failed (see above): nothing was changed, duckpipe still uses $sqlite_file"
      fi
      log "Copied $(grep -c . <<<"$checks") table(s); $sqlite_file is kept as a backup"
    fi
    log "Pointing duckpipe at this lake"
    sed -i -E "s|^([[:space:]]*catalog:[[:space:]]*)ducklake:sqlite:[^[:space:]#]+|\1$CATALOG host=127.0.0.1 user=$PG_USER|" "$DP_CONFIG"
    sed -i -E "s|^([[:space:]]*data_path:[[:space:]]*)s3://[^[:space:]#]*|\1s3://$LAKE_BUCKET/|" "$DP_CONFIG"
    grep -q '^PGPASSWORD=' "$DP_ENV" || printf 'PGPASSWORD=%s\n' "$PGPASSWORD" >>"$DP_ENV"
    "$GARAGE" bucket allow --read --write "$LAKE_BUCKET" --key duckpipe-key >/dev/null 2>&1 || true
    if [[ $dp_active -eq 1 ]]; then systemctl restart duckpipe; fi
  fi
fi

# --- Done --------------------------------------------------------------------
if [[ -n $LAN_CIDR ]] && command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  ufw status | grep -qE "^5432/tcp .*ALLOW.*$LAN_CIDR" ||
    log "ufw is active: to reach the catalog from the LAN run 'sudo ufw allow from $LAN_CIDR to any port 5432 proto tcp'"
fi

cat <<EOF

DuckLake is ready.
  Catalog  : PostgreSQL $PG_VER, database '$PG_DB', user '$PG_USER' ($listen:5432)
  Data     : s3://$LAKE_BUCKET/ (Garage, key '$KEY_NAME')
  Secrets  : $ENV_FILE   (sudo cat $ENV_FILE)
  On server: sudo ducklake        (DuckDB CLI with the lake attached as "lake")

From another machine (DuckDB 1.5+), with the values from $ENV_FILE:
  INSTALL ducklake; INSTALL postgres;
  CREATE SECRET garage (TYPE s3, KEY_ID '<AWS_ACCESS_KEY_ID>', SECRET '<AWS_SECRET_ACCESS_KEY>',
    REGION 'garage', ENDPOINT '${HOST_IP:-<server>}:3900', URL_STYLE 'path', USE_SSL false);
  ATTACH 'ducklake:postgres:dbname=$PG_DB host=${HOST_IP:-<server>} user=$PG_USER password=<PGPASSWORD>' AS lake;
  USE lake;
EOF
