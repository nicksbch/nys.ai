#!/usr/bin/env bash
#
# Test semantic locally and install it on the DuckLake server as a systemd
# service (via install.sh). Run FROM YOUR MACHINE, in any directory:
#   ./deploy.sh
#
# Needs uv locally, ssh access to the server and sudo there (you may be asked
# for your password). Override defaults with env vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 LISTEN='[::]:8097' ./deploy.sh
#
# LISTEN, LAKE_BUCKET, KEY_NAME, PG_ROLE and ENDPOINT are passed on to
# install.sh. Safe to re-run to ship a new version.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")"
command -v uv >/dev/null || die "uv is required (https://docs.astral.sh/uv/)"

log "Testing"
uv run -q --frozen pytest -q || die "the tests fail; nothing was deployed"

log "Connecting to $HOST"
remote=$(ssh -p "$SSH_PORT" "$HOST" 'mktemp -d') || die "could not reach $HOST over ssh"

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
COPYFILE_DISABLE=1 tar --no-xattrs -czf "$out/app.tgz" .python-version pyproject.toml uv.lock server.py models.yml

log "Uploading to $HOST:$remote"
scp -q -P "$SSH_PORT" "$out/app.tgz" install.sh "$HOST:$remote/"

# Forward only the settings set here, so install.sh defaults apply otherwise.
vars=()
for v in LISTEN LAKE_BUCKET KEY_NAME PG_ROLE ENDPOINT; do
  if [[ -n ${!v:-} ]]; then vars+=("$v=$(printf %q "${!v}")"); fi
done

log "Installing on $HOST (sudo)"
ssh -t -p "$SSH_PORT" "$HOST" "sudo env ${vars[*]-} bash $remote/install.sh $remote/app.tgz; rc=\$?; rm -rf $remote; exit \$rc"
