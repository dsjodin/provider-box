#!/usr/bin/env bash
# Provider Box installer: the only shell in the v2 model. Installs Docker if
# absent, does the one-time host preparation the containerized control plane
# cannot do itself (systemd-resolved stub listener, systemd-timesyncd), builds
# the control-plane image from this checkout, and starts it. Everything else -
# config, service selection, deployment - happens in the control plane web UI.
set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTROL_PLANE_IMAGE="${CONTROL_PLANE_IMAGE:-provider-box/control-plane:0.1.0}"
CONTROL_PLANE_NAME="provider-box-control-plane"
CONTROL_PLANE_PORT="${CONTROL_PLANE_PORT:-8445}"

fail() {
  echo "Error: $*" >&2
  exit 1
}

[[ "$EUID" -eq 0 ]] || fail "Run as root: sudo bash install.sh"

# --- Docker (port of the bash docker_pkgs, with the Ubuntu repo fix) --------
install_docker() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    echo "Docker with Compose v2 already installed."
    systemctl enable --now docker
    return 0
  fi
  if command -v docker >/dev/null 2>&1; then
    echo "Docker is installed but Compose v2 is missing; installing docker-compose-plugin."
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y docker-compose-plugin
    systemctl enable --now docker
    docker compose version >/dev/null 2>&1 || fail "docker compose v2 is required but not available."
    return 0
  fi

  echo "Installing Docker CE from Docker's official apt repository."
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl

  local os_id codename
  os_id="$(. /etc/os-release && echo "${ID}")"
  codename="$(. /etc/os-release && echo "${VERSION_CODENAME}")"
  [[ "${os_id}" == "debian" || "${os_id}" == "ubuntu" ]] || \
    fail "Unsupported distribution '${os_id}'; install Docker with Compose v2 manually and re-run."

  install -m 0755 -d /etc/apt/keyrings
  if [[ ! -f /etc/apt/keyrings/docker.asc ]]; then
    curl -fsSL "https://download.docker.com/linux/${os_id}/gpg" -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
  fi
  cat > /etc/apt/sources.list.d/docker.list <<EOF
deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${os_id} ${codename} stable
EOF
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y \
    docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  systemctl enable --now docker
  docker compose version >/dev/null 2>&1 || fail "docker compose v2 is required but not available."
}

# --- One-time host prep ------------------------------------------------------
# Technitium always owns port 53 in the v2 model, so the systemd-resolved stub
# listener is disabled up front (keeping host resolution working through the
# transition); chrony is containerized, so systemd-timesyncd is disabled.
prepare_host() {
  if systemctl is-enabled systemd-resolved >/dev/null 2>&1; then
    echo "Disabling the systemd-resolved DNS stub listener (Technitium will own port 53)."
    install -d -m 0755 /etc/systemd/resolved.conf.d
    cat > /etc/systemd/resolved.conf.d/provider-box.conf <<CONF
# Managed by Provider Box (install.sh). Remove and restart systemd-resolved to undo.
[Resolve]
DNSStubListener=no
CONF
    if [[ -L /etc/resolv.conf && "$(readlink /etc/resolv.conf)" == *stub-resolv.conf ]]; then
      ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf
    fi
    systemctl restart systemd-resolved
    getent hosts deb.debian.org >/dev/null || \
      fail "Host DNS resolution broke after disabling the resolved stub listener; fix /etc/resolv.conf and re-run."
  fi

  if systemctl is-enabled systemd-timesyncd >/dev/null 2>&1; then
    echo "Disabling systemd-timesyncd (chrony runs containerized)."
    systemctl disable --now systemd-timesyncd || true
  fi

  install -d -m 0755 /opt/provider-box /opt/provider-box/control-plane
}

# --- Build and run the control plane -----------------------------------------
run_control_plane() {
  echo "Building ${CONTROL_PLANE_IMAGE} from ${REPO_ROOT}."
  docker build -t "${CONTROL_PLANE_IMAGE}" -f "${REPO_ROOT}/services/control-plane/Dockerfile" "${REPO_ROOT}"

  docker rm -f "${CONTROL_PLANE_NAME}" >/dev/null 2>&1 || true
  echo "Starting the control plane."
  # The TLS paths point at the leaf the CA deployer issues for the control
  # plane; until the CA is deployed they do not exist and the server falls
  # back to plaintext HTTP with a logged warning.
  docker run -d --name "${CONTROL_PLANE_NAME}" \
    --restart unless-stopped \
    --network host \
    -e CONTROL_PLANE_ADDR=":${CONTROL_PLANE_PORT}" \
    -e CONTROL_PLANE_TLS_CERT="/opt/provider-box/control-plane/certs/control-plane.crt" \
    -e CONTROL_PLANE_TLS_KEY="/opt/provider-box/control-plane/certs/control-plane.key" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v /opt/provider-box:/opt/provider-box \
    -v /etc:/host/etc \
    "${CONTROL_PLANE_IMAGE}" >/dev/null
}

install_docker
prepare_host
run_control_plane

host_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
echo
echo "Provider Box control plane is running."
echo "Open http://${host_ip:-<host-ip>}:${CONTROL_PLANE_PORT}/config to upload your configuration,"
echo "then http://${host_ip:-<host-ip>}:${CONTROL_PLANE_PORT}/deploy to deploy services."
echo "The UI has no authentication; use it on a trusted lab network only."
