#!/usr/bin/env bash
#
# Build preprocessor for the Garage server and install it there as a systemd
# service (via install.sh). Run FROM YOUR MACHINE, in any directory:
#   ./deploy.sh
#
# Needs ssh access to the server and sudo there (you may be asked for your
# password). Override defaults with env vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 BUCKETS="home" ./deploy.sh
#
# BUCKETS, KEY_NAME, ENDPOINT, LISTEN and UI_USER are passed on to
# install.sh. Safe to re-run to ship a new version. preprocessor.yaml (the
# jobs) is shipped too and replaces the server's: change it here, then deploy.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")"
command -v go >/dev/null || die "go is required to build preprocessor"
[[ -f preprocessor.yaml ]] || die "preprocessor.yaml not found next to deploy.sh"
go run . -config preprocessor.yaml check >/dev/null || die "preprocessor.yaml does not load (see above); nothing was deployed"

log "Connecting to $HOST"
{ read -r arch; read -r remote; } < <(ssh -p "$SSH_PORT" "$HOST" 'uname -m; mktemp -d')
[[ -n ${remote:-} ]] || die "could not reach $HOST over ssh"
case $arch in
  x86_64) goarch=amd64 ;;
  aarch64 | arm64) goarch=arm64 ;;
  *) die "unsupported server architecture: $arch" ;;
esac

log "Building preprocessor for linux/$goarch"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -ldflags='-s -w' -o "$out/preprocessor" .

log "Uploading to $HOST:$remote"
scp -q -P "$SSH_PORT" "$out/preprocessor" install.sh preprocessor.yaml "$HOST:$remote/"

# Forward only the settings set here, so install.sh defaults apply otherwise.
vars=()
for v in BUCKETS KEY_NAME ENDPOINT LISTEN UI_USER; do
  if [[ -n ${!v:-} ]]; then vars+=("$v=$(printf %q "${!v}")"); fi
done

log "Installing on $HOST (sudo)"
ssh -t -p "$SSH_PORT" "$HOST" "sudo env ${vars[*]-} bash $remote/install.sh $remote/preprocessor; rc=\$?; rm -rf $remote; exit \$rc"
