#!/usr/bin/env bash
#
# Remove duckpipe from the Garage server: stops and removes the service, the
# duckpipe, duckpipectl and duckdb binaries, /etc/duckpipe.env and the S3 key.
# Run FROM YOUR MACHINE, in any directory:
#   ./undeploy.sh
#
# Your tables are kept: /etc/duckpipe.yaml, the catalog in /var/lib/duckpipe,
# the 'duckpipe' user and the lake bucket. Set PURGE=1 to also delete the
# config, the catalog and the user (the lake bucket is never touched):
#   PURGE=1 ./undeploy.sh
#
# Needs ssh access to the server and sudo there. Override defaults with env
# vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 KEY_NAME=duckpipe-key ./undeploy.sh
#
# Safe to re-run: anything already gone is skipped.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"
KEY_NAME="${KEY_NAME:-duckpipe-key}"
PURGE="${PURGE:-0}"

if [[ $PURGE == 1 ]]; then
  what="duckpipe AND its config, catalog and user"
else
  what="duckpipe (config, catalog and lake bucket are kept)"
fi
read -r -p "Remove $what from $HOST? [y/N] " ok
[[ $ok == [yY]* ]] || exit 1

remote_script=$(cat <<EOF
set -euo pipefail
log() { printf '\n==> %s\n' "\$*"; }

if systemctl list-unit-files duckpipe.service >/dev/null 2>&1; then
  log "Stopping and disabling duckpipe"
  systemctl disable --now duckpipe 2>/dev/null || true
fi

log "Removing binaries, unit and env file"
rm -f /usr/local/bin/duckpipe /usr/local/bin/duckpipectl /usr/local/bin/duckdb \
  /etc/systemd/system/duckpipe.service /etc/duckpipe.env
systemctl daemon-reload

if [[ -x /usr/local/bin/garage ]] && /usr/local/bin/garage key info "$KEY_NAME" >/dev/null 2>&1; then
  log "Deleting key '$KEY_NAME'"
  /usr/local/bin/garage key delete --yes "$KEY_NAME"
fi

if [[ $PURGE == 1 ]]; then
  log "Deleting /etc/duckpipe.yaml, /var/lib/duckpipe and user 'duckpipe'"
  rm -rf /etc/duckpipe.yaml /var/lib/duckpipe
  if id duckpipe >/dev/null 2>&1; then userdel duckpipe; fi
else
  log "Kept /etc/duckpipe.yaml, /var/lib/duckpipe and user 'duckpipe' (PURGE=1 removes them)"
fi

printf '\nduckpipe is removed. The lake bucket and its data files are untouched.\n'
EOF
)

printf '\n==> Uninstalling on %s (sudo)\n' "$HOST"
ssh -t -p "$SSH_PORT" "$HOST" "sudo bash -c $(printf %q "$remote_script")"
