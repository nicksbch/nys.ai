#!/usr/bin/env bash
#
# Remove lakeshape from the server: stops and removes the service, the
# lakeshape and lakeshapectl binaries, /etc/lakeshape.env and the S3 key.
# Run FROM YOUR MACHINE, in any directory:
#   ./undeploy.sh
#
# The tables lakeshape built stay in the lake, and so does its run history.
# The deployed project (/var/lib/lakeshape) and the 'lakeshape' user are kept
# too; PURGE=1 deletes them:
#   PURGE=1 ./undeploy.sh
#
# The DuckDB CLI is left alone: ducklake-setup.sh and the other tools use it.
# Needs ssh access to the server and sudo there. Override defaults with env
# vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 KEY_NAME=lakeshape-key ./undeploy.sh
#
# Safe to re-run: anything already gone is skipped.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"
KEY_NAME="${KEY_NAME:-lakeshape-key}"
PURGE="${PURGE:-0}"

if [[ $PURGE == 1 ]]; then
  what="lakeshape AND its deployed project and user"
else
  what="lakeshape (the deployed project and the lake's tables are kept)"
fi
read -r -p "Remove $what from $HOST? [y/N] " ok
[[ $ok == [yY]* ]] || exit 1

remote_script=$(cat <<EOF
set -euo pipefail
log() { printf '\n==> %s\n' "\$*"; }

if systemctl list-unit-files lakeshape.service >/dev/null 2>&1; then
  log "Stopping and disabling lakeshape"
  systemctl disable --now lakeshape 2>/dev/null || true
fi

log "Removing binaries, unit and env file"
rm -f /usr/local/bin/lakeshape /usr/local/bin/lakeshapectl \
  /etc/systemd/system/lakeshape.service /etc/lakeshape.env
systemctl daemon-reload

if [[ -x /usr/local/bin/garage ]] && /usr/local/bin/garage key info "$KEY_NAME" >/dev/null 2>&1; then
  log "Deleting key '$KEY_NAME'"
  /usr/local/bin/garage key delete --yes "$KEY_NAME"
fi

if [[ $PURGE == 1 ]]; then
  log "Deleting /var/lib/lakeshape and user 'lakeshape'"
  rm -rf /var/lib/lakeshape
  if id lakeshape >/dev/null 2>&1; then userdel lakeshape; fi
else
  log "Kept /var/lib/lakeshape and user 'lakeshape' (PURGE=1 removes them)"
fi

printf '\nlakeshape is removed. The tables it built and its run history are still in the lake.\n'
EOF
)

printf '\n==> Uninstalling on %s (sudo)\n' "$HOST"
ssh -t -p "$SSH_PORT" "$HOST" "sudo bash -c $(printf %q "$remote_script")"
