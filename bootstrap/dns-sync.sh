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
             PROVIDER_BOX_FQDN CA_DATA_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_fqdn "${PROVIDER_BOX_FQDN}"

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
    "Missing NetBox token at ${nb_token}. It is normally auto-provisioned by --netbox (re-run --netbox); manual SOPS/age placement of a composite nbt_<key>.<token> is the override."
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

dns_sync_url_host() {
  local u="${1#*://}"
  u="${u%%/*}"
  printf '%s' "${u%%:*}"
}

dns_sync_url_port() {
  local url="$1"
  local u="${url#*://}"
  u="${u%%/*}"
  if [[ "${u}" == *:* ]]; then
    printf '%s' "${u##*:}"
  elif [[ "${url}" == https://* ]]; then
    printf '443'
  else
    printf '80'
  fi
}

# The lab FQDNs in DNS_SYNC_*_URL are served by the zone dns-sync itself
# populates, so nothing bootstrap-phase may depend on resolving them. Both
# gates pin the FQDN to 127.0.0.1 (single-node design) with curl --resolve,
# the same idiom as the netbox module's own readiness wait. TLS verification
# stays full against the lab root.
require_netbox_ready_for_dns_sync() {
  local code host port
  host="$(dns_sync_url_host "${DNS_SYNC_NETBOX_URL}")"
  port="$(dns_sync_url_port "${DNS_SYNC_NETBOX_URL}")"
  code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 \
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
    --resolve "${host}:${port}:127.0.0.1" \
    "${DNS_SYNC_NETBOX_URL}/api/" || echo 000)
  [[ "${code}" == "200" || "${code}" == "401" || "${code}" == "403" ]] || \
    fail "NetBox at ${DNS_SYNC_NETBOX_URL} is not reachable on 127.0.0.1 (got ${code}). Run --netbox first."
}

require_technitium_ready_for_dns_sync() {
  local code host port
  host="$(dns_sync_url_host "${DNS_SYNC_TECHNITIUM_URL}")"
  port="$(dns_sync_url_port "${DNS_SYNC_TECHNITIUM_URL}")"
  code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 \
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
    --resolve "${host}:${port}:127.0.0.1" \
    "${DNS_SYNC_TECHNITIUM_URL}/" || echo 000)
  [[ "${code}" == "200" || "${code}" == "301" || "${code}" == "302" ]] || \
    fail "Technitium at ${DNS_SYNC_TECHNITIUM_URL} is not reachable on 127.0.0.1 (got ${code}). Run --technitium first."
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
    --add-host "$(dns_sync_url_host "${DNS_SYNC_NETBOX_URL}"):127.0.0.1" \
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

# Built-in Provider Box service FQDNs cannot live in NetBox as separate IP
# objects (NetBox enforces global IP uniqueness; the canonical host IP is one
# object with PROVIDER_BOX_FQDN as dns_name), so dns-sync synthesizes their A
# records from the environment on every reconcile pass. Same list as the
# unbound backend, via provider_box_builtin_fqdns.
build_dns_sync_builtin_records() {
  local fqdn records=""
  while IFS= read -r fqdn; do
    records="${records:+${records},}${fqdn}=${HOST_IPV4}"
  done < <(provider_box_builtin_fqdns)
  [[ -n "${records}" ]] || fail "No built-in service FQDNs are set; check the *_FQDN variables in provider-box.env."
  DNS_SYNC_BUILTIN_RECORDS="${records}"
  export DNS_SYNC_BUILTIN_RECORDS
}

render_dns_sync_stack() {
  DNS_SYNC_NETBOX_HOST="$(dns_sync_url_host "${DNS_SYNC_NETBOX_URL}")"
  DNS_SYNC_TECHNITIUM_HOST="$(dns_sync_url_host "${DNS_SYNC_TECHNITIUM_URL}")"
  export DNS_SYNC_NETBOX_HOST DNS_SYNC_TECHNITIUM_HOST
  build_dns_sync_builtin_records
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

# Unlike the pinned bootstrap gates above, this check deliberately uses real
# DNS: after the first reconcile the lab zone must be served by Technitium,
# which is exactly the output dns-sync exists to produce.
verify_dns_sync_zone() {
  local attempt
  require_command dig
  echo "Verifying dns-sync populated the lab zone (dig @127.0.0.1 ${PROVIDER_BOX_FQDN})."
  for attempt in $(seq 1 45); do
    if [[ -n "$(dig +short +time=2 +tries=1 @127.0.0.1 -p 53 "${PROVIDER_BOX_FQDN}" A 2>/dev/null)" ]]; then
      echo "dns-sync populated the zone: ${PROVIDER_BOX_FQDN} resolves via Technitium."
      verify_dns_sync_builtin_records
      return 0
    fi
    sleep 2
  done
  fail "dns-sync did not populate the lab zone: no A record for ${PROVIDER_BOX_FQDN} via 127.0.0.1. Check 'docker compose logs' under ${WORKDIR}/dns-sync and confirm NetBox holds the canonical host IP (run --netbox)."
}

# The built-ins land in the same reconcile pass as the canonical host record,
# so once PROVIDER_BOX_FQDN resolves the rest should follow immediately.
verify_dns_sync_builtin_records() {
  local fqdn attempt resolved
  while IFS= read -r fqdn; do
    resolved=""
    for attempt in $(seq 1 15); do
      if [[ -n "$(dig +short +time=2 +tries=1 @127.0.0.1 -p 53 "${fqdn}" A 2>/dev/null)" ]]; then
        resolved=1
        break
      fi
      sleep 2
    done
    [[ -n "${resolved}" ]] || \
      fail "Built-in service record ${fqdn} does not resolve via Technitium. Check 'docker compose logs' under ${WORKDIR}/dns-sync."
  done < <(provider_box_builtin_fqdns)
  echo "All built-in Provider Box service FQDNs resolve via Technitium."
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
  render_dns_sync_stack
  (
    cd "${WORKDIR}/dns-sync"
    docker compose down || true
    docker compose up -d
  )
  verify_dns_sync_running
  verify_dns_sync_zone
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
