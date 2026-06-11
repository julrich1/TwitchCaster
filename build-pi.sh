#!/usr/bin/env bash
#
# deploy-twitch-caster.sh — cross-compile for the Pi, deploy, and restart the service.
#
# Run from your twitch-caster project directory:
#   ./deploy-twitch-caster.sh
#
set -euo pipefail

REMOTE="pi@streambox.local"
SERVICE="twitch-caster"
BINARY="twitch-caster"

# --- sanity check -----------------------------------------------------------
if ! command -v sshpass >/dev/null 2>&1; then
  echo "Error: sshpass is not installed." >&2
  echo "  Debian/WSL: sudo apt install sshpass" >&2
  echo "  macOS:      brew install hudochenkov/sshpass/sshpass" >&2
  exit 1
fi

# --- prompt for the password once -------------------------------------------
read -rsp "Password for ${REMOTE}: " PASS
echo
export SSHPASS="$PASS"   # consumed by `sshpass -e` for the SSH/scp login

# Run a sudo command on the remote, feeding the password to `sudo -S` via stdin.
remote_sudo() {
  sshpass -e ssh "$REMOTE" "sudo -S $*" <<< "$PASS"
}

# --- build first; if the compile fails we exit before touching the Pi -------
echo "==> Cross-compiling ${BINARY} for linux/arm (GOARM=5)"
GOOS=linux GOARCH=arm GOARM=5 go build

# --- stop, copy, start ------------------------------------------------------
echo "==> Stopping ${SERVICE}"
remote_sudo systemctl stop "$SERVICE"

# Safety net: restart the service if we exit early (e.g. scp fails).
trap 'echo "!! Aborted — restarting ${SERVICE}" >&2; remote_sudo systemctl start "$SERVICE"' EXIT

echo "==> Copying files to ${REMOTE}:"
sshpass -e scp -r "$BINARY" configuration.json static "${REMOTE}:"

echo "==> Starting ${SERVICE}"
remote_sudo systemctl start "$SERVICE"

trap - EXIT   # success — clear the safety net

# --- clean up the local build artifact --------------------------------------
echo "==> Removing local ${BINARY}"
rm -f "$BINARY"

echo "==> Done"