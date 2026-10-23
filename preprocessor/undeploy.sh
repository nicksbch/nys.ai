#!/usr/bin/env bash
#
# Remove preprocessor from the Garage server: stops and removes the service, the
# preprocessor and preprocessorctl binaries, /etc/preprocessor.env and the S3 key.
# Run FROM YOUR MACHINE, in any directory:
#   ./undeploy.sh
#
# Kept: /etc/preprocessor.yaml, the state in /var/lib/preprocessor, the
# 'preprocessor' user and every file it unzipped. Set PURGE=1 to also delete
# the config, the state and the user (buckets are never touched):
#   PURGE=1 ./undeploy.sh
#
# Needs ssh access to the server and sudo there. Override defaults with env
# vars, e.g.:
#   HOST=nicolas@192.168.1.169 SSH_PORT=2222 KEY_NAME=preprocessor-key ./undeploy.sh
#
# Safe to re-run: anything already gone is skipped.

set -euo pipefail

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"
KEY_NAME="${KEY_NAME:-preprocessor-key}"
PURGE="${PURGE:-0}"

if [[ $PURGE == 1 ]]; then
  what="preprocessor AND its config, state and user"
else
  what="preprocessor (config, state and unzipped files are kept)"
fi
read -r -p "Remove $what from $HOST? [y/N] " ok
[[ $ok == [yY]* ]] || exit 1

remote_script=$(cat <<EOF
set -euo pipefail
log() { printf '\n==> %s\n' "\$*"; }

if systemctl list-unit-files preprocessor.service >/dev/null 2>&1; then
  log "Stopping and disabling preprocessor"
  systemctl disable --now preprocessor 2>/dev/null || true
fi

log "Removing binaries, unit and env file"
rm -f /usr/local/bin/preprocessor /usr/local/bin/preprocessorctl \
  /etc/systemd/system/preprocessor.service /etc/preprocessor.env
systemctl daemon-reload

if [[ -x /usr/local/bin/garage ]] && /usr/local/bin/garage key info "$KEY_NAME" >/dev/null 2>&1; then
  log "Deleting key '$KEY_NAME'"
  /usr/local/bin/garage key delete --yes "$KEY_NAME"
fi

if [[ $PURGE == 1 ]]; then
  log "Deleting /etc/preprocessor.yaml, /var/lib/preprocessor and user 'preprocessor'"
  rm -rf /etc/preprocessor.yaml /var/lib/preprocessor
  if id preprocessor >/dev/null 2>&1; then userdel preprocessor; fi
else
  log "Kept /etc/preprocessor.yaml, /var/lib/preprocessor and user 'preprocessor' (PURGE=1 removes them)"
fi

printf '\npreprocessor is removed. Buckets and the files it unzipped are untouched.\n'
EOF
)

printf '\n==> Uninstalling on %s (sudo)\n' "$HOST"
ssh -t -p "$SSH_PORT" "$HOST" "sudo bash -c $(printf %q "$remote_script")"
