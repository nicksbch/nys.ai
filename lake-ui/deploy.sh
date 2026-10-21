#!/usr/bin/env bash
#
# Build lake-ui for the DuckLake server and install it there as a systemd
# service (via install.sh). Run FROM YOUR MACHINE, in any directory:
#   ./deploy.sh
#
# Needs ssh access to the server and sudo there (you may be asked for your
# password). Override defaults with env vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 LISTEN='[::]:8092' ./deploy.sh
#
# LISTEN, LAKE_BUCKET, KEY_NAME, PG_ROLE, UI_USER and ENDPOINT are passed on to
# install.sh.
# Safe to re-run to ship a new version.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")"
command -v go >/dev/null || die "go is required to build lake-ui"

log "Connecting to $HOST"
{ read -r arch; read -r remote; } < <(ssh -p "$SSH_PORT" "$HOST" 'uname -m; mktemp -d')
[[ -n ${remote:-} ]] || die "could not reach $HOST over ssh"
case $arch in
  x86_64) goarch=amd64 ;;
  aarch64 | arm64) goarch=arm64 ;;
  *) die "unsupported server architecture: $arch" ;;
esac

log "Building lake-ui for linux/$goarch"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -ldflags='-s -w' -o "$out/lake-ui" .

log "Uploading to $HOST:$remote"
scp -q -P "$SSH_PORT" "$out/lake-ui" install.sh "$HOST:$remote/"

# Forward only the settings set here, so install.sh defaults apply otherwise.
vars=()
for v in LISTEN LAKE_BUCKET KEY_NAME PG_ROLE UI_USER ENDPOINT; do
  if [[ -n ${!v:-} ]]; then vars+=("$v=$(printf %q "${!v}")"); fi
done

log "Installing on $HOST (sudo)"
ssh -t -p "$SSH_PORT" "$HOST" "sudo env ${vars[*]-} bash $remote/install.sh $remote/lake-ui; rc=\$?; rm -rf $remote; exit \$rc"
