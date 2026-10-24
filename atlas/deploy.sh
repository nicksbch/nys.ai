#!/usr/bin/env bash
#
# Build atlas for the server and install it there as a systemd service (via
# install.sh). Run FROM YOUR MACHINE, in any directory:
#   ./deploy.sh
#
# Needs ssh access to the server and sudo there (you may be asked for your
# password). Override defaults with env vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 LISTEN='[::]:8101' ./deploy.sh
#
# LISTEN, LINKS and UI_USER are passed on to install.sh.
# Safe to re-run to ship a new version.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")"
command -v go >/dev/null || die "go is required to build atlas"

log "Connecting to $HOST"
{ read -r arch; read -r remote; } < <(ssh -p "$SSH_PORT" "$HOST" 'uname -m; mktemp -d')
[[ -n ${remote:-} ]] || die "could not reach $HOST over ssh"
case $arch in
  x86_64) goarch=amd64 ;;
  aarch64 | arm64) goarch=arm64 ;;
  *) die "unsupported server architecture: $arch" ;;
esac

log "Building atlas for linux/$goarch"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -ldflags='-s -w' -o "$out/atlas" .

log "Uploading to $HOST:$remote"
scp -q -P "$SSH_PORT" "$out/atlas" install.sh "$HOST:$remote/"

# Forward only the settings set here, so install.sh defaults apply otherwise.
vars=()
for v in LISTEN LINKS UI_USER; do
  if [[ -n ${!v:-} ]]; then vars+=("$v=$(printf %q "${!v}")"); fi
done

log "Installing on $HOST (sudo)"
ssh -t -p "$SSH_PORT" "$HOST" "sudo env ${vars[*]-} bash $remote/install.sh $remote/atlas; rc=\$?; rm -rf $remote; exit \$rc"
