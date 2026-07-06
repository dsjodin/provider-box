#!/usr/bin/env bash

# dns-sync deliberately does NOT touch /etc/resolv.conf or disable
# systemd-resolved. That is the 1.0 --unbound trap (do_unbound rewrote
# resolv.conf unconditionally) and the technitium-dns_design.md sec 9 rule
# explicitly forbids repeating it. If the operator wants clients to use
# Technitium, they manage resolv.conf themselves.

require_dns_sync_vars() {
  local var
  for var in WORKDIR REPO_ROOT DNS_SYNC_IMAGE DNS_SYNC_DIR DNS_SYNC_SECRETS_DIR \
             DNS_SYNC_NETBOX_URL DNS_SYNC_TECHNITIUM_URL DNS_SYNC_INTERVAL \
             CA_DATA_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${DNS_SYNC_DIR}"
  validate_var_path "${DNS_SYNC_SECRETS_DIR}"
  validate_var_path "${CA_DATA_DIR}"
  [[ "${DNS_SYNC_IMAGE}" == *:* ]] || fail "DNS_SYNC_IMAGE must include an explicit image tag"
  [[ "${DNS_SYNC_IMAGE}" != *:latest ]] || fail "DNS_SYNC_IMAGE must not use the latest tag"
  [[ "${DNS_SYNC_SECRETS_DIR}" != "${WORKDIR}/dns-sync" && "${DNS_SYNC_SECRETS_DIR}" != "${WORKDIR}/dns-sync/"* ]] || \
    fail "DNS_SYNC_SECRETS_DIR must not be inside ${WORKDIR}/dns-sync so --remove preserves the operator's secrets"
  [[ "${DNS_SYNC_NETBOX_URL}" =~ ^https?:// ]] || fail "DNS_SYNC_NETBOX_URL must be an http(s):// URL"
  [[ "${DNS_SYNC_TECHNITIUM_URL}" =~ ^https?:// ]] || fail "DNS_SYNC_TECHNITIUM_URL must be an http(s):// URL"
  [[ "${DNS_SYNC_INTERVAL}" =~ ^[0-9]+[smh]$ ]] || \
    fail "DNS_SYNC_INTERVAL must look like 30s, 5m, or 1h"
}

require_dns_sync_remove_vars() {
  local var
  for var in WORKDIR DNS_SYNC_DIR DNS_SYNC_SECRETS_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done
  validate_var_path "${WORKDIR}"
  validate_var_path "${DNS_SYNC_DIR}"
  validate_var_path "${DNS_SYNC_SECRETS_DIR}"
}

require_dns_sync_secrets() {
  local nb_token="${DNS_SYNC_SECRETS_DIR}/netbox.token"
  local tt_token="${DNS_SYNC_SECRETS_DIR}/technitium.token"

  [[ -f "${nb_token}" ]] || fail \
    "Missing NetBox token at ${nb_token}. Place the decrypted token there (SOPS/age) before running --dns-sync."
  [[ -f "${tt_token}" ]] || fail \
    "Missing Technitium token at ${tt_token}. Place the decrypted token there (SOPS/age) before running --dns-sync."

  [[ -s "${nb_token}" ]] || fail "${nb_token} is empty"
  [[ -s "${tt_token}" ]] || fail "${tt_token} is empty"

  chmod 0600 "${nb_token}" "${tt_token}"
  chown 1000:1000 "${nb_token}" "${tt_token}"
}

require_ca_root_for_dns_sync() {
  [[ -f "${CA_DATA_DIR}/certs/root_ca.crt" ]] || \
    fail "Missing step-ca root certificate in ${CA_DATA_DIR}/certs/root_ca.crt. Run --ca first."
}

require_netbox_ready_for_dns_sync() {
  local code
  code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 \
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
    "${DNS_SYNC_NETBOX_URL}/api/" || echo 000)
  [[ "${code}" == "200" || "${code}" == "401" || "${code}" == "403" ]] || \
    fail "NetBox at ${DNS_SYNC_NETBOX_URL} is not reachable (got ${code}). Run --netbox first."
}

require_technitium_ready_for_dns_sync() {
  local code
  code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 \
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
    "${DNS_SYNC_TECHNITIUM_URL}/" || echo 000)
  [[ "${code}" == "200" || "${code}" == "301" || "${code}" == "302" ]] || \
    fail "Technitium at ${DNS_SYNC_TECHNITIUM_URL} is not reachable (got ${code}). Run --technitium first."
}

bootstrap_dns_sync_layout() {
  install -d -m 0755 "${WORKDIR}/dns-sync" "${DNS_SYNC_DIR}"
  install -d -m 0700 "${DNS_SYNC_SECRETS_DIR}"
  chown 1000:1000 "${DNS_SYNC_DIR}" "${DNS_SYNC_SECRETS_DIR}"
}

build_dns_sync_image() {
  local src="${REPO_ROOT}/services/dns-sync"
  [[ -d "${src}" ]] || fail "dns-sync source not found at ${src}"
  echo "Building ${DNS_SYNC_IMAGE} from ${src}"
  docker build -t "${DNS_SYNC_IMAGE}" "${src}" || \
    fail "Failed to build dns-sync image"
}

apply_dns_seed_to_netbox() {
  local seed_file="${REPO_ROOT}/config/dns.seed"
  if [[ ! -f "${seed_file}" ]]; then
    echo "No config/dns.seed; skipping NetBox seed import."
    return
  fi
  echo "Importing config/dns.seed into NetBox (idempotent)"
  docker run --rm \
    --user 1000:1000 \
    -e NETBOX_URL="${DNS_SYNC_NETBOX_URL}" \
    -e NETBOX_TOKEN_FILE="/run/provider-box/secrets/netbox.token" \
    -e NETBOX_CA_BUNDLE="/etc/provider-box/certs/root_ca.crt" \
    -v "${DNS_SYNC_SECRETS_DIR}:/run/provider-box/secrets:ro" \
    -v "${CA_DATA_DIR}/certs/root_ca.crt:/etc/provider-box/certs/root_ca.crt:ro" \
    -v "${seed_file}:/etc/provider-box/dns.seed:ro" \
    --network host \
    "${DNS_SYNC_IMAGE}" \
    dns-seed netbox-import /etc/provider-box/dns.seed || \
    fail "Failed to import dns.seed into NetBox"
}

apply_technitium_forwarder() {
  if [[ -z "${TECHNITIUM_FORWARDER:-}" ]]; then
    echo "TECHNITIUM_FORWARDER not set; skipping forwarder configuration."
    return
  fi
  echo "Setting Technitium upstream forwarder: ${TECHNITIUM_FORWARDER}"
  docker run --rm \
    --user 1000:1000 \
    -e TECHNITIUM_URL="${DNS_SYNC_TECHNITIUM_URL}" \
    -e TECHNITIUM_TOKEN_FILE="/run/provider-box/secrets/technitium.token" \
    -e TECHNITIUM_CA_BUNDLE="/etc/provider-box/certs/root_ca.crt" \
    -e TECHNITIUM_FORWARDER="${TECHNITIUM_FORWARDER}" \
    -v "${DNS_SYNC_SECRETS_DIR}:/run/provider-box/secrets:ro" \
    -v "${CA_DATA_DIR}/certs/root_ca.crt:/etc/provider-box/certs/root_ca.crt:ro" \
    --network host \
    "${DNS_SYNC_IMAGE}" \
    dns-seed set-forwarder || \
    fail "Failed to set Technitium forwarder"
}

render_dns_sync_stack() {
  render_template "${TEMPLATE_DIR}/docker-compose.dns-sync.yml.tpl" "${WORKDIR}/dns-sync/docker-compose.yml"
}

verify_dns_sync_running() {
  local attempt
  echo "Waiting for dns-sync container to report healthy state."
  for attempt in $(seq 1 30); do
    if [[ "$(docker inspect -f '{{.State.Running}}' dns-sync-dns-sync-1 2>/dev/null)" == "true" ]] || \
       docker compose -f "${WORKDIR}/dns-sync/docker-compose.yml" ps --status running --quiet | grep -q .; then
      return 0
    fi
    sleep 2
  done
  fail "dns-sync container did not enter running state. Check 'docker compose logs' under ${WORKDIR}/dns-sync."
}

do_dns_sync() {
  require_dns_sync_vars
  common_pkgs
  docker_pkgs
  require_ca_root_for_dns_sync
  require_netbox_ready_for_dns_sync
  require_technitium_ready_for_dns_sync
  bootstrap_dns_sync_layout
  require_dns_sync_secrets
  build_dns_sync_image
  apply_dns_seed_to_netbox
  apply_technitium_forwarder
  render_dns_sync_stack
  (
    cd "${WORKDIR}/dns-sync"
    docker compose down || true
    docker compose up -d
  )
  verify_dns_sync_running
  echo "dns-sync is running. Reconcile interval: ${DNS_SYNC_INTERVAL}."
  echo "Logs: docker compose -f ${WORKDIR}/dns-sync/docker-compose.yml logs -f"
}

remove_dns_sync() {
  local runtime_dir="${WORKDIR}/dns-sync"
  local compose_file="${runtime_dir}/docker-compose.yml"

  require_dns_sync_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    (
      cd "${runtime_dir}"
      docker compose down || true
    )
  fi

  rm -rf "${runtime_dir}"
  echo "Removed dns-sync containers and runtime files under ${runtime_dir}. Operator secrets in ${DNS_SYNC_SECRETS_DIR} were preserved."
}
