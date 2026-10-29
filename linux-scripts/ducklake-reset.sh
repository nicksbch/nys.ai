#!/usr/bin/env bash
#
# Reset the DuckLake to empty: no tables, no snapshots (no time travel), no
# data files. Run FROM YOUR MACHINE, in any directory:
#   ./ducklake-reset.sh
#
# What it does, on the server:
#   1. stops the services that write to the lake (duckpipe, lakeshape)
#   2. drops the catalog's tables in PostgreSQL (database "ducklake"); the
#      database, roles and grants are kept, so lake-ui, semantic and the
#      read-only "ducklake" keep working without re-running their installs
#   3. creates a fresh, empty lake on s3://lake/, the way ducklake-setup.sh does
#   4. deletes every file under s3://lake/: the new catalog knows none of them,
#      so DuckLake's own orphan cleanup removes them all
#   5. starts the services again
#
# duckpipe's load history and lakeshape's run history are lake tables, so they
# go too: duckpipe loads every source file again on its next run, and
# lakeshape's jobs rebuild the models after that. The source buckets (home,
# ...), the S3 keys and /etc/*.env are not touched.
#
# Needs ssh access to the server and sudo there. Override defaults with env
# vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 LAKE_BUCKET=lake ./ducklake-reset.sh
#
# Safe to re-run: on an empty lake it changes nothing.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"
LAKE_BUCKET="${LAKE_BUCKET:-lake}"
PG_DB="${PG_DB:-ducklake}"
PG_USER="${PG_USER:-ducklake}"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# These go into SQL on the server, so only plain names.
[[ $PG_DB =~ ^[a-z_][a-z0-9_]*$ ]] || die "PG_DB must be a plain lower-case name"
[[ $PG_USER =~ ^[a-z_][a-z0-9_]*$ ]] || die "PG_USER must be a plain lower-case name"
[[ $LAKE_BUCKET =~ ^[a-z0-9][a-z0-9.-]*$ ]] || die "LAKE_BUCKET must be a plain bucket name"

cat <<EOF
This DELETES the whole lake on $HOST:
  - every table and snapshot in the catalog (PostgreSQL database '$PG_DB')
  - every file under s3://$LAKE_BUCKET/
It cannot be undone. duckpipe reloads from the source buckets afterwards.
EOF
read -r -p "Type 'reset $LAKE_BUCKET' to go on: " ok
[[ $ok == "reset $LAKE_BUCKET" ]] || { echo "Nothing was changed."; exit 1; }

remote_script=$(cat <<'REMOTE'
set -euo pipefail
log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

DUCKDB=/usr/local/bin/duckdb
ENV_FILE=/etc/ducklake.env
CATALOG="ducklake:postgres:dbname=$PG_DB"   # host/user/password come from the PG* vars in $ENV_FILE

[[ -f $ENV_FILE ]] || die "$ENV_FILE not found: run ducklake-setup.sh first"
[[ -x $DUCKDB ]] || die "$DUCKDB not found: run ducklake-setup.sh first"
cd /   # psql runs as postgres, which may not be able to read the current directory
set -a; . "$ENV_FILE"; set +a
s3_host=${AWS_ENDPOINT_URL#*://}
ssl=false; [[ $AWS_ENDPOINT_URL == https* ]] && ssl=true
# The secret reaches duckdb on stdin, never on a command line.
duck() {
  "$DUCKDB" -bail -batch -list -noheader -init /dev/null :memory: <<EOF
.output /dev/null
LOAD ducklake; LOAD postgres; LOAD httpfs;
CREATE TEMPORARY SECRET ducklake_s3 (TYPE s3, KEY_ID '$AWS_ACCESS_KEY_ID', SECRET '$AWS_SECRET_ACCESS_KEY',
  REGION '$AWS_REGION', ENDPOINT '$s3_host', URL_STYLE 'path', USE_SSL $ssl);
$1
.output
$2
EOF
}

# Only ever delete the bucket this lake was created on.
data_path=$(duck "ATTACH '$CATALOG' AS lake (READ_ONLY);" \
  "SELECT value FROM ducklake_options('lake') WHERE option_name = 'data_path';") ||
  die "could not open the lake (see above); nothing was changed"
[[ -z $data_path || $data_path == "s3://$LAKE_BUCKET/" ]] ||
  die "the lake's data_path is $data_path, not s3://$LAKE_BUCKET/; nothing was changed"

# --- 1. Stop the writers -----------------------------------------------------
stopped=()
for s in duckpipe lakeshape; do
  if systemctl is-active --quiet "$s"; then
    log "Stopping $s"
    systemctl stop "$s"
    stopped+=("$s")
  fi
done

# --- 2. Drop the catalog -----------------------------------------------------
# Only the tables (and any schemas) DuckLake created as $PG_USER. The
# database, its grants and the default privileges for $PG_USER stay, so the
# new catalog tables are readable by the read-only roles again.
log "Dropping the catalog's tables in database '$PG_DB'"
runuser -u postgres -- env -u PGHOST -u PGPORT -u PGDATABASE -u PGUSER -u PGPASSWORD \
  psql -X -q -v ON_ERROR_STOP=1 -d "$PG_DB" <<EOF
DO \$\$
DECLARE r record;
BEGIN
  FOR r IN SELECT nspname FROM pg_namespace
           WHERE nspname <> 'public' AND nspowner = (SELECT oid FROM pg_roles WHERE rolname = '$PG_USER')
  LOOP
    EXECUTE format('DROP SCHEMA %I CASCADE', r.nspname);
  END LOOP;
  FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tableowner = '$PG_USER'
  LOOP
    EXECUTE format('DROP TABLE public.%I CASCADE', r.tablename);
  END LOOP;
END
\$\$;
EOF

# --- 3. New lake, 4. delete the old files ------------------------------------
log "Creating an empty lake on s3://$LAKE_BUCKET/ and deleting the old files"
duck "ATTACH '$CATALOG' AS lake (DATA_PATH 's3://$LAKE_BUCKET/');" "
SELECT 'files before: ' || count(*) FROM glob('s3://$LAKE_BUCKET/**');
CALL ducklake_delete_orphaned_files('lake', cleanup_all => true);
SELECT 'files left  : ' || count(*) FROM glob('s3://$LAKE_BUCKET/**');" ||
  die "deleting the files failed (see above); the catalog is already empty. ${stopped[*]:-nothing} is still stopped: fix it and re-run"

left=$(duck "" "SELECT count(*) FROM glob('s3://$LAKE_BUCKET/**');")
if [[ $left != 0 ]]; then
  log "$left file(s) are still in s3://$LAKE_BUCKET/; DuckLake left them, e.g.:"
  duck "" "SELECT file FROM glob('s3://$LAKE_BUCKET/**') LIMIT 10;"
fi

# --- 5. Start the services again ---------------------------------------------
for s in "${stopped[@]}"; do
  log "Starting $s"
  systemctl start "$s"
done
# Readers may have the old catalog open.
for s in lake-ui semantic; do
  if systemctl is-active --quiet "$s"; then
    log "Restarting $s"
    systemctl restart "$s"
  fi
done

printf '\nThe lake is empty. duckpipe reloads the source files on its next run.\n'
REMOTE
)

printf '\n==> Resetting the lake on %s (sudo)\n' "$HOST"
ssh -t -p "$SSH_PORT" "$HOST" \
  "sudo env LAKE_BUCKET=$LAKE_BUCKET PG_DB=$PG_DB PG_USER=$PG_USER bash -c $(printf %q "$remote_script")"
