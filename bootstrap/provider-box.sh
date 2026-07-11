#!/usr/bin/env bash
set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${REPO_ROOT}/config/provider-box.env"
ENV_EXAMPLE_FILE="${REPO_ROOT}/config/provider-box.env.example"
RECORDS_FILE="${REPO_ROOT}/config/dns.seed"
TEMPLATE_DIR="${REPO_ROOT}/templates"
BOOTSTRAP_DIR="${REPO_ROOT}/bootstrap"
APT_UPDATED=0

trap 'echo "Error: command failed on line ${LINENO}. See output above for details." >&2' ERR

usage() {
  cat <<USAGE
Usage:
  sudo bash bootstrap/provider-box.sh --ntp
  sudo bash bootstrap/provider-box.sh --rsyslog
  sudo bash bootstrap/provider-box.sh --ca
  sudo bash bootstrap/provider-box.sh --ca --remove
  sudo bash bootstrap/provider-box.sh --depot
  sudo bash bootstrap/provider-box.sh --depot --remove
  sudo bash bootstrap/provider-box.sh --keycloak
  sudo bash bootstrap/provider-box.sh --keycloak --remove
  sudo bash bootstrap/provider-box.sh --authentik
  sudo bash bootstrap/provider-box.sh --authentik --remove
  sudo bash bootstrap/provider-box.sh --netbox
  sudo bash bootstrap/provider-box.sh --netbox --remove
  sudo bash bootstrap/provider-box.sh --s3
  sudo bash bootstrap/provider-box.sh --s3 --remove
  sudo bash bootstrap/provider-box.sh --sftp
  sudo bash bootstrap/provider-box.sh --sftp --remove
  sudo bash bootstrap/provider-box.sh --technitium
  sudo bash bootstrap/provider-box.sh --technitium --remove
  sudo bash bootstrap/provider-box.sh --dns-sync
  sudo bash bootstrap/provider-box.sh --dns-sync --remove
  sudo bash bootstrap/provider-box.sh --dashboard
  sudo bash bootstrap/provider-box.sh --dashboard --remove
  sudo bash bootstrap/provider-box.sh --all
  sudo bash bootstrap/provider-box.sh --all --remove

Note: --all deploys Technitium right after --ca (Technitium needs a step-ca
certificate) and the read-only dashboard last. --dns-sync is NOT included in
--all and must be deployed explicitly after --all so the dashboard FQDN
resolves by name.
USAGE
}

require_root() {
  [[ "$EUID" -eq 0 ]] || { echo "Run as root"; exit 1; }
}

fail() {
  echo "Error: $*" >&2
  exit 1
}

require_env_file() {
  [[ -f "$ENV_FILE" ]] || fail "Missing ${ENV_FILE}"
  check_provider_env_is_current
}

require_template_file() {
  [[ -f "$1" ]] || fail "Missing template: $1"
}

require_module_file() {
  [[ -f "$1" ]] || fail "Missing bootstrap module: $1"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "Required command not found: $1"
}

active_env_vars_in_file() {
  local file="$1"
  local line

  while IFS= read -r line; do
    [[ "${line}" =~ ^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)= ]] || continue
    printf '%s\n' "${BASH_REMATCH[1]}"
  done < "${file}"
}

check_provider_env_is_current() {
  local env_vars missing_vars var

  [[ -f "${ENV_EXAMPLE_FILE}" ]] || fail "Missing ${ENV_EXAMPLE_FILE}"

  env_vars="$(active_env_vars_in_file "${ENV_FILE}")"
  missing_vars=""

  while IFS= read -r var; do
    [[ -n "${var}" ]] || continue
    if ! grep -Fxq "${var}" <<< "${env_vars}"; then
      missing_vars="${missing_vars}${var}"$'\n'
    fi
  done < <(active_env_vars_in_file "${ENV_EXAMPLE_FILE}")

  [[ -z "${missing_vars}" ]] && return 0

  echo "Error: ${ENV_FILE} appears outdated." >&2
  echo "Missing variables from ${ENV_EXAMPLE_FILE}:" >&2
  while IFS= read -r var; do
    [[ -n "${var}" ]] || continue
    echo "  - ${var}" >&2
  done <<< "${missing_vars}"
  echo "Update ${ENV_FILE} using ${ENV_EXAMPLE_FILE}." >&2
  echo "Provider Box does not modify ${ENV_FILE} automatically." >&2
  exit 1
}

require_package_installed() {
  local pkg
  for pkg in "$@"; do
    dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q "install ok installed" || \
      fail "Package '${pkg}' is not installed. Check apt output for details."
  done
}

load_env() {
  # shellcheck disable=SC1090
  set -a
  source "$ENV_FILE"
  set +a
}

apt_update_once() {
  require_command apt-get
  if [[ "$APT_UPDATED" -eq 0 ]]; then
    apt-get update || fail "apt-get update failed"
    APT_UPDATED=1
  fi
}

install_pkg() {
  local packages=("$@")
  DEBIAN_FRONTEND=noninteractive apt-get install -y "${packages[@]}" || \
    fail "Failed to install packages: ${packages[*]}"
  require_package_installed "${packages[@]}"
}

common_pkgs() {
  apt_update_once
  install_pkg ca-certificates curl openssl bind9-dnsutils ufw gettext-base
  require_command dig
}

# step-ca needs jq to rewrite ca.json's db/crl stanzas after the container
# self-initializes (the init flow always writes a badger stanza first).
ca_pkgs() {
  apt_update_once
  install_pkg jq
}

ntp_pkgs() {
  apt_update_once
  install_pkg chrony
}

rsyslog_pkgs() {
  apt_update_once
  install_pkg rsyslog
}

docker_pkgs() {
  apt_update_once

  # If Docker + Compose v2 already works, do not touch Docker packages.
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    echo "Docker with Compose v2 already installed; ensuring docker service is enabled/running."
    systemctl enable --now docker
    return 0
  fi

  # If Docker exists but Compose v2 is missing, install only the Docker CE Compose plugin.
  if command -v docker >/dev/null 2>&1; then
    echo "Docker is installed but Compose v2 is missing; installing docker-compose-plugin."
    install_pkg docker-compose-plugin
    systemctl enable --now docker
    docker compose version >/dev/null 2>&1 || \
      fail "docker compose v2 is required but not available."
    return 0
  fi

  # Docker is not installed, install Docker CE from Docker's official Debian repo.
  install_pkg ca-certificates curl

  install -m 0755 -d /etc/apt/keyrings

  if [[ ! -f /etc/apt/keyrings/docker.asc ]]; then
    curl -fsSL https://download.docker.com/linux/debian/gpg \
      -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
  fi

  local debian_codename
  debian_codename="$(. /etc/os-release && echo "${VERSION_CODENAME}")"

  cat > /etc/apt/sources.list.d/docker.list <<EOF
deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian ${debian_codename} stable
EOF

  apt-get update || fail "apt-get update failed after adding Docker CE repository"

  install_pkg docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

  require_command docker
  systemctl enable --now docker

  docker compose version >/dev/null 2>&1 || \
    fail "docker compose v2 is required but not available."
}

# Single source of truth for the built-in Provider Box service FQDNs, consumed
# by the dns-sync built-in record synthesis. Prints one FQDN per line; unset
# services are skipped.
provider_box_builtin_fqdns() {
  local var
  for var in PROVIDER_BOX_FQDN DNS_FQDN CA_FQDN DEPOT_FQDN KEYCLOAK_FQDN AUTHENTIK_FQDN NETBOX_FQDN S3_FQDN SFTP_FQDN SYSLOG_FQDN CONTROL_PLANE_FQDN; do
    [[ -n "${!var:-}" ]] && printf '%s\n' "${!var}"
  done
  return 0
}

render_template() {
  require_command envsubst
  require_template_file "$1"
  envsubst < "$1" > "$2"
}

validate_ipv4() {
  local ip="$1"
  [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1

  local octet
  IFS='.' read -r -a octets <<< "$ip"
  for octet in "${octets[@]}"; do
    (( octet >= 0 && octet <= 255 )) || return 1
  done
}

validate_cidr() {
  local cidr="$1"
  local ip="${cidr%/*}"
  local prefix="${cidr##*/}"
  [[ "$cidr" == */* ]] || return 1
  validate_ipv4 "$ip" || return 1
  [[ "$prefix" =~ ^[0-9]+$ ]] || return 1
  (( 10#${prefix} >= 0 && 10#${prefix} <= 32 )) || return 1
}

validate_fqdn() {
  local fqdn="$1"
  [[ "$fqdn" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]]
}

validate_path() {
  local path="$1"
  [[ "$path" = /* ]]
}

validate_var_ipv4() {
  validate_ipv4 "$1" || fail "Invalid IPv4 address: $1"
}

validate_var_cidr() {
  validate_cidr "$1" || fail "Invalid CIDR value: $1"
}

validate_var_fqdn() {
  validate_fqdn "$1" || fail "Invalid FQDN value: $1"
}

validate_var_path() {
  validate_path "$1" || fail "Path must be absolute: $1"
}

validate_var_not_placeholder() {
  [[ "$1" != CHANGE_ME* ]] || fail "Replace placeholder value before continuing"
}

# A safe unquoted SQL identifier (role/db/user name). These names are
# interpolated directly into SQL by the CA module, so restrict them to
# [A-Za-z_][A-Za-z0-9_]* to keep that interpolation injection-free.
validate_pg_identifier() {
  [[ "$2" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || \
    fail "$1 must be a valid PostgreSQL identifier ([A-Za-z_][A-Za-z0-9_]*): got '$2'"
}

validate_service_cert_duration() {
  local value="$1"

  [[ -n "${value}" ]] || fail "SERVICE_CERT_DURATION must not be empty"
  validate_var_not_placeholder "${value}"
  [[ "${value}" =~ ^[0-9]+h$ ]] || \
    fail "SERVICE_CERT_DURATION must use an hour duration such as 8760h"
}

validate_ca_password_value() {
  local value="$1"
  local normalized="${value,,}"

  [[ -n "${value}" ]] || fail "CA_PASSWORD must not be empty"
  [[ "${value}" != CHANGE_ME* ]] || fail "Replace placeholder CA_PASSWORD before continuing"
  [[ "${normalized}" != change-me* ]] || fail "Replace placeholder CA_PASSWORD before continuing"
}

default_ca_password_file() {
  printf '%s/secrets/password.txt' "${CA_DATA_DIR}"
}

resolve_ca_password_file() {
  if [[ -z "${CA_PASSWORD_FILE:-}" ]]; then
    CA_PASSWORD_FILE="$(default_ca_password_file)"
  fi

  export CA_PASSWORD_FILE
}

validate_port() {
  local port="$1"
  [[ "$port" =~ ^[0-9]+$ ]] || return 1
  (( port >= 1 && port <= 65535 ))
}

validate_var_port() {
  validate_port "$1" || fail "Invalid TCP port: $1"
}

certificate_matches_dns_identity() {
  local cert_file="$1"
  local key_file="$2"
  local fqdn="$3"
  local sans

  [[ -f "${cert_file}" && -f "${key_file}" ]] || return 1
  openssl x509 -in "${cert_file}" -noout -checkend 0 >/dev/null 2>&1 || return 1
  cmp -s \
    <(openssl x509 -in "${cert_file}" -noout -pubkey 2>/dev/null) \
    <(openssl pkey -in "${key_file}" -pubout 2>/dev/null) || return 1

  sans="$(openssl x509 -in "${cert_file}" -noout -ext subjectAltName 2>/dev/null || true)"
  printf '%s\n' "${sans}" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | grep -Fxq "DNS:${fqdn}"
}

extract_ipv4_from_value() {
  local value="$1"
  printf '%s' "${value%%/*}"
}

value_has_cidr() {
  [[ "$1" == */* ]]
}

parse_dns_record_line() {
  local line="$1"
  local extra

  read -r DNS_RECORD_FQDN DNS_RECORD_TARGET extra <<< "$line"
  [[ -n "${DNS_RECORD_FQDN}" && -n "${DNS_RECORD_TARGET}" && -z "${extra}" ]]
}

ipv4_to_int() {
  local ip="$1"
  local a b c d
  IFS='.' read -r a b c d <<< "$ip"
  printf '%u' "$(( (a << 24) | (b << 16) | (c << 8) | d ))"
}

int_to_ipv4() {
  local value="$1"
  printf '%d.%d.%d.%d' \
    $(( (value >> 24) & 255 )) \
    $(( (value >> 16) & 255 )) \
    $(( (value >> 8) & 255 )) \
    $(( value & 255 ))
}

cidr_to_network() {
  local cidr="$1"
  local ip="${cidr%/*}"
  local prefix="${cidr##*/}"
  local ip_int mask network_int

  ip_int="$(ipv4_to_int "${ip}")"
  if (( prefix == 0 )); then
    mask=0
  else
    mask=$(( (0xFFFFFFFF << (32 - prefix)) & 0xFFFFFFFF ))
  fi
  network_int=$(( ip_int & mask ))
  printf '%s/%s' "$(int_to_ipv4 "${network_int}")" "${prefix}"
}

derive_host_ip_fields() {
  HOST_IPV4="$(extract_ipv4_from_value "${HOST_IP}")"
  HOST_NETWORK_CIDR="$(cidr_to_network "${HOST_IP}")"
  export HOST_IPV4 HOST_NETWORK_CIDR
}

validate_records_file() {
  local line line_no=0 fqdn ip_value
  while IFS= read -r line; do
    line_no=$((line_no + 1))
    [[ -z "$line" || "$line" = \#* ]] && continue

    parse_dns_record_line "$line" || \
      fail "Invalid record format in ${RECORDS_FILE}:${line_no}. Expected: <fqdn> <ip> or <fqdn> <ip/cidr>"

    fqdn="${DNS_RECORD_FQDN}"
    ip_value="${DNS_RECORD_TARGET}"
    validate_fqdn "$fqdn" || fail "Invalid FQDN in ${RECORDS_FILE}:${line_no}: ${fqdn}"
    if value_has_cidr "${ip_value}"; then
      validate_cidr "${ip_value}" || fail "Invalid CIDR in ${RECORDS_FILE}:${line_no}: ${ip_value}"
    else
      validate_ipv4 "${ip_value}" || fail "Invalid IP in ${RECORDS_FILE}:${line_no}: ${ip_value}"
    fi
  done < "$RECORDS_FILE"
}

require_common_vars() {
  local var
  for var in HOST_IP SEARCH_DOMAIN; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_cidr "${HOST_IP}"
  derive_host_ip_fields
  validate_var_fqdn "${SEARCH_DOMAIN}"
}

require_allow_net_vars() {
  local var
  for var in ALLOW_NET_1 ALLOW_NET_2 ALLOW_NET_3; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_cidr "${ALLOW_NET_1}"
  validate_var_cidr "${ALLOW_NET_2}"
  validate_var_cidr "${ALLOW_NET_3}"
}

require_dns_vars() {
  local var
  for var in DNS_FQDN DNS_FORWARDER; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_fqdn "${DNS_FQDN}"
  validate_var_ipv4 "${DNS_FORWARDER}"
}

# DNS_FORWARDER is the upstream forwarder Technitium uses for external
# resolution. Validates and exports it for template rendering and API calls.
resolve_dns_forwarder() {
  [[ -n "${DNS_FORWARDER:-}" ]] || fail "Missing required variable: DNS_FORWARDER"
  validate_var_ipv4 "${DNS_FORWARDER}"
  export DNS_FORWARDER
}

require_ntp_vars() {
  local var
  for var in CHRONY_SERVER_1 CHRONY_SERVER_2 CHRONY_SERVER_3; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_fqdn "${CHRONY_SERVER_1}"
  validate_var_fqdn "${CHRONY_SERVER_2}"
  validate_var_fqdn "${CHRONY_SERVER_3}"
}

require_env_vars() {
  require_common_vars
  require_allow_net_vars
  require_dns_vars
  require_ntp_vars
}

require_keycloak_vars() {
  local var
  for var in WORKDIR KEYCLOAK_DIR KEYCLOAK_FQDN KEYCLOAK_PORT KEYCLOAK_IMAGE KEYCLOAK_ADMIN_USER KEYCLOAK_ADMIN_PASSWORD KEYCLOAK_BOOTSTRAP_REALM_NAME KEYCLOAK_BOOTSTRAP_GROUP_NAME KEYCLOAK_BOOTSTRAP_CLIENT_ID KEYCLOAK_BOOTSTRAP_CLIENT_SECRET KEYCLOAK_BOOTSTRAP_CLIENT_REDIRECT_URIS; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${KEYCLOAK_DIR}"
  validate_var_fqdn "${KEYCLOAK_FQDN}"
  validate_var_port "${KEYCLOAK_PORT}"
  [[ "${KEYCLOAK_IMAGE}" == *:* ]] || fail "KEYCLOAK_IMAGE must include an explicit image tag"
  [[ "${KEYCLOAK_IMAGE}" != *:latest ]] || fail "KEYCLOAK_IMAGE must not use the latest tag"
  validate_var_not_placeholder "${KEYCLOAK_ADMIN_PASSWORD}"
  validate_var_not_placeholder "${KEYCLOAK_BOOTSTRAP_REALM_NAME}"
  validate_var_not_placeholder "${KEYCLOAK_BOOTSTRAP_GROUP_NAME}"
  validate_var_not_placeholder "${KEYCLOAK_BOOTSTRAP_CLIENT_ID}"
  validate_var_not_placeholder "${KEYCLOAK_BOOTSTRAP_CLIENT_SECRET}"
}

require_module_file "${BOOTSTRAP_DIR}/ntp.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/ntp.sh"

require_module_file "${BOOTSTRAP_DIR}/keycloak.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/keycloak.sh"

require_module_file "${BOOTSTRAP_DIR}/authentik.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/authentik.sh"

require_module_file "${BOOTSTRAP_DIR}/netbox.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/netbox.sh"

require_module_file "${BOOTSTRAP_DIR}/s3.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/s3.sh"

require_module_file "${BOOTSTRAP_DIR}/sftp.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/sftp.sh"

require_module_file "${BOOTSTRAP_DIR}/rsyslog.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/rsyslog.sh"

require_module_file "${BOOTSTRAP_DIR}/ca.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/ca.sh"

require_module_file "${BOOTSTRAP_DIR}/depot.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/depot.sh"

require_module_file "${BOOTSTRAP_DIR}/technitium.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/technitium.sh"

require_module_file "${BOOTSTRAP_DIR}/dns-sync.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/dns-sync.sh"

require_module_file "${BOOTSTRAP_DIR}/dashboard.sh"
# shellcheck disable=SC1090
source "${BOOTSTRAP_DIR}/dashboard.sh"

require_root

TARGET_SERVICE=""
REMOVE_MODE=0

[[ $# -ge 1 && $# -le 2 ]] || { usage; exit 1; }

for arg in "$@"; do
  case "$arg" in
    --remove)
      [[ "${REMOVE_MODE}" -eq 0 ]] || fail "Duplicate --remove flag"
      REMOVE_MODE=1
      ;;
    --ntp|--rsyslog|--ca|--depot|--keycloak|--authentik|--netbox|--s3|--sftp|--technitium|--dns-sync|--dashboard|--all)
      [[ -z "${TARGET_SERVICE}" ]] || fail "Specify exactly one service flag"
      TARGET_SERVICE="$arg"
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 1
      ;;
  esac
done

[[ -n "${TARGET_SERVICE}" ]] || fail "No service flag provided"

case "${TARGET_SERVICE}" in
  --ntp)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      fail "Removal is not implemented for --ntp"
    fi
    require_common_vars
    require_allow_net_vars
    require_ntp_vars
    do_ntp
    ;;
  --rsyslog)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      fail "Removal is not implemented for --rsyslog"
    fi
    require_common_vars
    do_rsyslog
    ;;
  --ca)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_ca
    else
      require_common_vars
      do_ca
    fi
    ;;
  --depot)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_depot
    else
      require_common_vars
      do_depot
    fi
    ;;
  --keycloak)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_keycloak
    else
      require_common_vars
      do_keycloak
    fi
    ;;
  --authentik)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_authentik
    else
      require_common_vars
      do_authentik
    fi
    ;;
  --netbox)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_netbox
    else
      require_common_vars
      do_netbox
    fi
    ;;
  --s3)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_s3
    else
      require_common_vars
      do_s3
    fi
    ;;
  --sftp)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_sftp
    else
      require_common_vars
      do_sftp
    fi
    ;;
  --technitium)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_technitium
    else
      require_common_vars
      do_technitium
    fi
    ;;
  --dns-sync)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_dns_sync
    else
      require_common_vars
      do_dns_sync
    fi
    ;;
  --dashboard)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_dashboard
    else
      require_common_vars
      do_dashboard
    fi
    ;;
  --all)
    require_env_file
    load_env
    if [[ "${REMOVE_MODE}" -eq 1 ]]; then
      remove_dashboard
      remove_sftp
      remove_s3
      remove_netbox
      remove_authentik
      remove_keycloak
      remove_depot
      remove_ca
    else
      require_env_vars
      do_ntp
      do_rsyslog
      do_ca
      # Technitium depends on step-ca for its certificate, so the DNS stage
      # runs after the CA is up.
      do_technitium
      do_depot
      do_keycloak
      do_authentik
      do_netbox
      do_s3
      do_sftp
      # The dashboard is a read-only view of the services above, so it runs
      # last. Its scoped upstream tokens are optional (panels degrade to
      # "not configured"), so --all stays coherent when they are unset. DNS
      # resolution of CONTROL_PLANE_FQDN lands after the post---all --dns-sync run.
      do_dashboard
    fi
    ;;
esac
