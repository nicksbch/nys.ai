#!/usr/bin/env bash
#
# Build lakeshape for the server and install it there, with your project, as
# a systemd service (via install.sh). Run FROM YOUR MACHINE, in any directory:
#   ./deploy.sh
#
# PROJECT is the project directory (project.yaml, models/, jobs.yml, ...),
# ../analytics next to this script by default; relative paths are from the
# directory you run it in.
# Files and folders starting with "." (.git, ...) are not copied.
#
# Needs ssh access to the server and sudo there (you may be asked for your
# password). Override defaults with env vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 PROJECT=~/other-project ./deploy.sh
#
# LAKE_BUCKET, KEY_NAME, ENDPOINT and DUCKDB_VERSION are passed on to
# install.sh. Safe to re-run to ship a new version: a new binary restarts the
# service (a run in progress rolls back); a new project only reloads it,
# between runs.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"
PROJECT="${PROJECT:-$(dirname "$0")/../analytics}"

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ -f $PROJECT/project.yaml ]] || die "$PROJECT/project.yaml not found (set PROJECT to your project directory)"
PROJECT=$(cd "$PROJECT" && pwd)
cd "$(dirname "$0")"
command -v go >/dev/null || die "go is required to build lakeshape"

log "Checking the project"
go run . -project "$PROJECT" -target prod ls >/dev/null || die "the project does not load (see above); nothing was deployed"

log "Connecting to $HOST"
{ read -r arch; read -r remote; } < <(ssh -p "$SSH_PORT" "$HOST" 'uname -m; mktemp -d')
[[ -n ${remote:-} ]] || die "could not reach $HOST over ssh"
case $arch in
  x86_64) goarch=amd64 ;;
  aarch64 | arm64) goarch=arm64 ;;
  *) die "unsupported server architecture: $arch" ;;
esac

log "Building lakeshape for linux/$goarch"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -ldflags='-s -w' -o "$out/lakeshape" .
# '.?*', not '.*': macOS tar would match the archive's root "." and leave it empty.
COPYFILE_DISABLE=1 tar -C "$PROJECT" --exclude='.?*' -czf "$out/project.tgz" .
tar -tzf "$out/project.tgz" | grep -qx './project.yaml' || die "the project archive has no project.yaml"

log "Uploading to $HOST:$remote"
scp -q -P "$SSH_PORT" "$out/lakeshape" "$out/project.tgz" install.sh "$HOST:$remote/"

# Forward only the settings set here, so install.sh defaults apply otherwise.
vars=()
for v in LAKE_BUCKET KEY_NAME ENDPOINT DUCKDB_VERSION; do
  if [[ -n ${!v:-} ]]; then vars+=("$v=$(printf %q "${!v}")"); fi
done

log "Installing on $HOST (sudo)"
ssh -t -p "$SSH_PORT" "$HOST" "sudo env ${vars[*]-} bash $remote/install.sh $remote/lakeshape $remote/project.tgz; rc=\$?; rm -rf $remote; exit \$rc"
