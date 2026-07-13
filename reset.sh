#!/usr/bin/env bash
# Reset a Provider Box test host so install.sh can run from a clean slate.
#
# Removes ALL Docker state on this host (containers, images, volumes,
# networks, build cache - not just Provider Box's) and repoints the host
# resolver at systemd-resolved, since removing the Technitium container
# while /etc/resolv.conf targets 127.0.0.1 would leave the host without DNS.
#
# Usage:
#   sudo bash reset.sh          # asks for confirmation
#   sudo bash reset.sh --yes    # no prompt
#   sudo bash reset.sh --data   # ALSO wipe /opt/provider-box (certs, CA,
#                               # postgres data, saved config - everything)
set -Eeuo pipefail

fail() {
  echo "Error: $*" >&2
  exit 1
}

[[ "$EUID" -eq 0 ]] || fail "Run as root: sudo bash reset.sh"

ASSUME_YES=0
WIPE_DATA=0
for arg in "$@"; do
  case "$arg" in
    --yes) ASSUME_YES=1 ;;
    --data) WIPE_DATA=1 ;;
    *) fail "Unknown flag: $arg (supported: --yes, --data)" ;;
  esac
done

echo "This removes ALL Docker containers, images, volumes, networks, and build cache on this host."
[[ "$WIPE_DATA" -eq 1 ]] && echo "It will ALSO delete /opt/provider-box (CA keys, postgres data, saved config)."
if [[ "$ASSUME_YES" -ne 1 ]]; then
  read -r -p "Continue? [y/N] " answer
  [[ "$answer" == "y" || "$answer" == "Y" ]] || { echo "Aborted."; exit 0; }
fi

# Repoint the resolver BEFORE stopping containers: once Technitium is gone,
# a resolv.conf targeting 127.0.0.1 leaves the host without DNS.
if grep -qs "127.0.0.1" /etc/resolv.conf; then
  echo "Pointing /etc/resolv.conf back at systemd-resolved."
  rm -f /etc/resolv.conf
  ln -s /run/systemd/resolve/resolv.conf /etc/resolv.conf
  systemctl restart systemd-resolved || true
fi

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  containers="$(docker ps -aq)"
  if [[ -n "$containers" ]]; then
    echo "Stopping and removing all containers."
    # shellcheck disable=SC2086
    docker rm -f $containers >/dev/null
  fi
  echo "Pruning images, volumes, networks, and build cache."
  docker system prune --all --volumes --force >/dev/null
else
  echo "Docker is not running or not installed; skipping Docker cleanup."
fi

if [[ "$WIPE_DATA" -eq 1 ]]; then
  echo "Deleting /opt/provider-box."
  rm -rf /opt/provider-box
fi

getent hosts deb.debian.org >/dev/null || \
  echo "WARNING: host DNS resolution is not working; check /etc/resolv.conf before re-running install.sh."

echo "Done. Re-run 'sudo bash install.sh' for a fresh deployment."
