#!/usr/bin/env bash

require_technitium_ca_vars() {
  local var
  for var in CA_FQDN CA_PORT CA_DATA_DIR CA_PROVISIONER_NAME SERVICE_CERT_DURATION CA_IMAGE; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_fqdn "${CA_FQDN}"
  validate_var_port "${CA_PORT}"
  validate_var_path "${CA_DATA_DIR}"
  validate_service_cert_duration "${SERVICE_CERT_DURATION}"
  [[ "${CA_IMAGE}" == *:* ]] || fail "CA_IMAGE must include an explicit image tag"
  [[ "${CA_IMAGE}" != *:latest ]] || fail "CA_IMAGE must not use the latest tag"
  resolve_ca_password_file
  validate_var_path "${CA_PASSWORD_FILE}"
  [[ "${CA_PASSWORD_FILE}" == "${CA_DATA_DIR}"/* ]] || \
    fail "CA_PASSWORD_FILE must be located under CA_DATA_DIR so the step-ca container can read it"
}

require_technitium_vars() {
  local var
  for var in WORKDIR DNS_FQDN TECHNITIUM_HTTP_PORT TECHNITIUM_HTTPS_PORT TECHNITIUM_DATA_DIR TECHNITIUM_CERT_DIR TECHNITIUM_IMAGE DNS_SYNC_SECRETS_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${DNS_SYNC_SECRETS_DIR}"
  validate_var_path "${WORKDIR}"
  validate_var_fqdn "${DNS_FQDN}"
  validate_var_port "${TECHNITIUM_HTTP_PORT}"
  validate_var_port "${TECHNITIUM_HTTPS_PORT}"
  validate_var_path "${TECHNITIUM_DATA_DIR}"
  validate_var_path "${TECHNITIUM_CERT_DIR}"
  [[ "${TECHNITIUM_HTTP_PORT}" != "${TECHNITIUM_HTTPS_PORT}" ]] || \
    fail "TECHNITIUM_HTTP_PORT and TECHNITIUM_HTTPS_PORT must be different"
  [[ "${TECHNITIUM_IMAGE}" == *:* ]] || fail "TECHNITIUM_IMAGE must include an explicit image tag"
  [[ "${TECHNITIUM_IMAGE}" != *:latest ]] || fail "TECHNITIUM_IMAGE must not use the latest tag"
  [[ "${TECHNITIUM_DATA_DIR}" != "${WORKDIR}/technitium" && "${TECHNITIUM_DATA_DIR}" != "${WORKDIR}/technitium/"* ]] || \
    fail "TECHNITIUM_DATA_DIR must not be inside ${WORKDIR}/technitium so --remove preserves Technitium content"
  [[ "${TECHNITIUM_CERT_DIR}" != "${WORKDIR}/technitium" && "${TECHNITIUM_CERT_DIR}" != "${WORKDIR}/technitium/"* ]] || \
    fail "TECHNITIUM_CERT_DIR must not be inside ${WORKDIR}/technitium so --remove preserves Technitium certificates"
}

require_technitium_remove_vars() {
  local var
  for var in WORKDIR TECHNITIUM_DATA_DIR TECHNITIUM_CERT_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${TECHNITIUM_DATA_DIR}"
  validate_var_path "${TECHNITIUM_CERT_DIR}"
}

require_ca_ready_for_technitium() {
  [[ -f "${CA_DATA_DIR}/config/ca.json" ]] || \
    fail "step-ca is not initialized. Run --ca first."
  [[ -f "${CA_DATA_DIR}/certs/root_ca.crt" ]] || \
    fail "Missing step-ca root certificate in ${CA_DATA_DIR}/certs/root_ca.crt. Run --ca first."
  [[ -f "${CA_DATA_DIR}/certs/intermediate_ca.crt" ]] || \
    fail "Missing step-ca intermediate certificate in ${CA_DATA_DIR}/certs/intermediate_ca.crt. Run --ca first."
  [[ -f "${CA_PASSWORD_FILE}" ]] || \
    fail "Missing CA password file: ${CA_PASSWORD_FILE}. Run --ca first."

  curl --silent --show-error --fail \
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
    --resolve "${CA_FQDN}:${CA_PORT}:127.0.0.1" \
    "https://${CA_FQDN}:${CA_PORT}/roots.pem" >/dev/null || \
    fail "step-ca is not reachable on https://${CA_FQDN}:${CA_PORT}. Run --ca first and ensure the CA is healthy."
}

normalize_technitium_certificate_permissions() {
  local cert_dir="$1"
  local cert_file="${cert_dir}/technitium.crt"
  local key_file="${cert_dir}/technitium.key"
  local chain_file="${cert_dir}/technitium-ca-chain.pem"
  local roots_file="${cert_dir}/technitium-ca-roots.pem"

  chmod 0755 "${cert_dir}"
  chown 1000:1000 "${cert_dir}"
  [[ -f "${cert_file}" ]] && chmod 0644 "${cert_file}" && chown 1000:1000 "${cert_file}"
  [[ -f "${chain_file}" ]] && chmod 0644 "${chain_file}" && chown 1000:1000 "${chain_file}"
  [[ -f "${roots_file}" ]] && chmod 0644 "${roots_file}" && chown 1000:1000 "${roots_file}"
  [[ -f "${key_file}" ]] && chmod 0600 "${key_file}" && chown 1000:1000 "${key_file}"
}

bootstrap_technitium_layout() {
  install -d -m 0755 \
    "${WORKDIR}/technitium" \
    "${TECHNITIUM_DATA_DIR}" \
    "${TECHNITIUM_CERT_DIR}"
  chown 1000:1000 "${TECHNITIUM_DATA_DIR}"
}

issue_technitium_certificates() {
  local cert_dir="${TECHNITIUM_CERT_DIR}"
  local cert_file="${cert_dir}/technitium.crt"
  local key_file="${cert_dir}/technitium.key"
  local cert_dir_in_container="/etc/provider-box/technitium-certs"
  local password_file_in_container="/home/step/${CA_PASSWORD_FILE#${CA_DATA_DIR}/}"

  install -d -m 0755 "${cert_dir}"
  if [[ "$(stat -c %u "${cert_dir}")" != "1000" ]]; then
    chown 1000:1000 "${cert_dir}"
  fi

  if certificate_matches_dns_identity "${cert_file}" "${key_file}" "${DNS_FQDN}"; then
    echo "Reusing existing Technitium certificate for ${DNS_FQDN}."
    normalize_technitium_certificate_permissions "${cert_dir}"
    return
  fi

  if [[ -f "${cert_file}" || -f "${key_file}" ]]; then
    echo "Existing Technitium certificate is not valid for ${DNS_FQDN}; issuing replacement."
  else
    echo "Issuing Technitium certificate for ${DNS_FQDN}."
  fi
  rm -f \
    "${cert_file}" \
    "${key_file}" \
    "${cert_dir}/technitium-ca-chain.pem" \
    "${cert_dir}/technitium-ca-roots.pem" \
    "${cert_dir}/technitium-leaf.crt"

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca certificate "${DNS_FQDN}" "${cert_dir_in_container}/technitium-leaf.crt" "${cert_dir_in_container}/technitium.key" \
      --san "${DNS_FQDN}" \
      --not-after "${SERVICE_CERT_DURATION}" \
      --issuer "${CA_PROVISIONER_NAME}" \
      --provisioner-password-file "${password_file_in_container}" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to issue a Technitium certificate from step-ca."

  mv "${cert_dir}/technitium-leaf.crt" "${cert_dir}/technitium.crt" || \
    fail "Failed to store the Technitium certificate chain."

  cat "${CA_DATA_DIR}/certs/intermediate_ca.crt" "${CA_DATA_DIR}/certs/root_ca.crt" > "${cert_dir}/technitium-ca-chain.pem" || \
    fail "Failed to build the Technitium CA chain bundle."

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca roots "${cert_dir_in_container}/technitium-ca-roots.pem" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to fetch the step-ca root bundle for Technitium."

  normalize_technitium_certificate_permissions "${cert_dir}"
}

# Technitium's web service requires its TLS certificate as PKCS#12
# (webServiceTlsCertificatePath in the settings API, verified against 13.4.2).
# Convert the step-ca PEM material into technitium.pfx inside the already
# mounted cert dir, with a generated password persisted like the repo's other
# managed secrets. Rebuilt whenever the PEM material is newer (cert reissue).
build_technitium_pfx() {
  local cert_dir="${TECHNITIUM_CERT_DIR}"
  local pfx_file="${cert_dir}/technitium.pfx"
  local pfx_password_file="${cert_dir}/technitium-pfx-password"
  local pfx_password

  require_command openssl
  if [[ ! -f "${pfx_password_file}" ]]; then
    install -m 0600 /dev/null "${pfx_password_file}"
    openssl rand -base64 24 | tr -d '\n' > "${pfx_password_file}"
    echo "Generated Technitium PKCS#12 password at: ${pfx_password_file}"
  fi
  chmod 0600 "${pfx_password_file}"
  chown 1000:1000 "${pfx_password_file}"
  pfx_password="$(cat "${pfx_password_file}")"

  if [[ ! -f "${pfx_file}" || "${cert_dir}/technitium.crt" -nt "${pfx_file}" || "${cert_dir}/technitium.key" -nt "${pfx_file}" ]]; then
    echo "Building the Technitium PKCS#12 bundle at ${pfx_file}."
    openssl pkcs12 -export \
      -in "${cert_dir}/technitium.crt" \
      -inkey "${cert_dir}/technitium.key" \
      -out "${pfx_file}" \
      -passout "pass:${pfx_password}" || \
      fail "Failed to build the Technitium PKCS#12 bundle."
  fi
  chmod 0600 "${pfx_file}"
  chown 1000:1000 "${pfx_file}"
}

render_technitium_stack() {
  render_template "${TEMPLATE_DIR}/docker-compose.technitium.yml.tpl" "${WORKDIR}/technitium/docker-compose.yml"
}

# Deploying --technitium is the explicit opt-in to running DNS on this host:
# the module takes port 53 deliberately, which requires disabling the
# systemd-resolved stub listener on stock Ubuntu. Host resolution is verified
# before and after every transition so bootstrap never proceeds with broken
# DNS. --technitium --remove restores the stock configuration.

TECHNITIUM_RESOLVED_DROPIN="/etc/systemd/resolved.conf.d/provider-box.conf"
TECHNITIUM_RESOLV_CONF_MARKER="Managed by Provider Box (--technitium)"

verify_host_resolution() {
  local context="$1"
  getent hosts deb.debian.org >/dev/null || \
    fail "Host DNS resolution is broken ${context}. Fix /etc/resolv.conf before re-running --technitium."
}

disable_resolved_stub_listener() {
  echo "Disabling the systemd-resolved DNS stub listener so Technitium can bind port 53."
  install -d -m 0755 /etc/systemd/resolved.conf.d
  cat > "${TECHNITIUM_RESOLVED_DROPIN}" <<CONF
# ${TECHNITIUM_RESOLV_CONF_MARKER}. Removed by --technitium --remove.
[Resolve]
DNSStubListener=no
CONF

  # Keep /etc/resolv.conf functional through the transition: the stub file
  # points at 127.0.0.53, which stops answering once the stub listener is off.
  if [[ -L /etc/resolv.conf && "$(readlink /etc/resolv.conf)" == *stub-resolv.conf ]]; then
    ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf
  fi

  systemctl restart systemd-resolved || fail "Failed to restart systemd-resolved."
  verify_host_resolution "after disabling the systemd-resolved stub listener"
}

preflight_technitium_port_53() {
  local listeners

  require_command ss
  listeners="$(ss -H -lntup 'sport = :53' 2>/dev/null || true)"
  [[ -n "${listeners}" ]] || return 0

  if grep -q "systemd-resolve" <<< "${listeners}"; then
    disable_resolved_stub_listener
    listeners="$(ss -H -lntup 'sport = :53' 2>/dev/null || true)"
  fi

  [[ -z "${listeners}" ]] || fail "Port 53 is already in use and Provider Box will not stop the holder automatically. Stop the conflicting service (for example a leftover unbound or dnsmasq) and re-run --technitium. Current listeners:
${listeners}"
}

point_host_resolver_at_technitium() {
  echo "Pointing the host resolver at Technitium (127.0.0.1)."
  rm -f /etc/resolv.conf
  cat > /etc/resolv.conf <<RESOLV
# ${TECHNITIUM_RESOLV_CONF_MARKER}. Removed by --technitium --remove.
nameserver 127.0.0.1
search ${SEARCH_DOMAIN}
RESOLV
  verify_host_resolution "after pointing /etc/resolv.conf at Technitium"
}

restore_host_resolver() {
  [[ -f "${TECHNITIUM_RESOLVED_DROPIN}" ]] || return 0

  echo "Restoring the systemd-resolved stub listener and /etc/resolv.conf."
  rm -f "${TECHNITIUM_RESOLVED_DROPIN}"
  if grep -qs "${TECHNITIUM_RESOLV_CONF_MARKER}" /etc/resolv.conf || \
     [[ "$(readlink /etc/resolv.conf 2>/dev/null)" == "/run/systemd/resolve/resolv.conf" ]]; then
    ln -sf ../run/systemd/resolve/stub-resolv.conf /etc/resolv.conf
  fi
  systemctl restart systemd-resolved || fail "Failed to restart systemd-resolved."
  verify_host_resolution "after restoring systemd-resolved"
}

verify_technitium_dns_listener() {
  local attempt
  echo "Waiting for Technitium DNS listener on 127.0.0.1:53."
  for attempt in $(seq 1 60); do
    if dig +short +time=1 +tries=1 @127.0.0.1 -p 53 "${DNS_FQDN}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  fail "Technitium DNS listener did not become ready on 127.0.0.1:53. Check 'docker compose ps' and 'docker compose logs'."
}

technitium_json_string_field() {
  local field="$1"
  sed -n "s/.*\"${field}\":\"\\([^\"]*\\)\".*/\\1/p" | head -n1
}

# Authenticate to the local Technitium API with the first-boot admin
# credentials and print a session token. Bootstrap-phase calls go over HTTP on
# 127.0.0.1 only.
technitium_api_login_token() {
  local console_url="http://127.0.0.1:${TECHNITIUM_HTTP_PORT}"
  local login_response token

  login_response="$(curl --silent --show-error \
    "${console_url}/api/user/login?user=admin&pass=admin" || true)"
  token="$(printf '%s' "${login_response}" | technitium_json_string_field token)"
  [[ -n "${token}" ]] || \
    fail "Failed to authenticate to the Technitium API. Response: ${login_response}"
  printf '%s' "${token}"
}

# Configure Technitium's upstream forwarder so it can resolve external names
# and act as the sole nameserver for the host and lab. Authenticates with the
# first-boot admin credentials (the module tells the operator to change the
# password from the console afterwards) and sets forwarders over UDP. The
# settings API is idempotent, so re-running with the same value succeeds. The
# forwarder setting persists in the Technitium data dir and is removed with it.
configure_technitium_forwarder() {
  local console_url token set_response status recursion
  console_url="http://127.0.0.1:${TECHNITIUM_HTTP_PORT}"

  token="$(technitium_api_login_token)"

  set_response="$(curl --silent --show-error --get \
    --data-urlencode "token=${token}" \
    --data-urlencode "forwarders=${DNS_FORWARDER}" \
    --data-urlencode "forwarderProtocol=Udp" \
    "${console_url}/api/settings/set" || true)"
  status="$(printf '%s' "${set_response}" | technitium_json_string_field status)"
  [[ "${status}" == "ok" ]] || \
    fail "Failed to set the Technitium upstream forwarder ${DNS_FORWARDER}. Response: ${set_response}"

  # Leave the default recursion policy (AllowOnlyForPrivateNetworks) in place,
  # but confirm recursion is not disabled or external names cannot be resolved.
  recursion="$(printf '%s' "${set_response}" | technitium_json_string_field recursion)"
  [[ "${recursion}" != "Deny" ]] || \
    fail "Technitium recursion is disabled (recursion=Deny); external names cannot be resolved. Enable recursion for the lab networks in the Technitium console."

  echo "Technitium upstream forwarder set to ${DNS_FORWARDER} (UDP)."
}

verify_technitium_external_resolution() {
  local attempt
  echo "Verifying Technitium can resolve external names via ${DNS_FORWARDER}."
  for attempt in $(seq 1 30); do
    if [[ -n "$(dig +short +time=2 +tries=1 @127.0.0.1 -p 53 one.one.one.one A 2>/dev/null)" ]]; then
      echo "Technitium resolved an external name."
      return 0
    fi
    sleep 2
  done
  fail "Technitium cannot resolve external names — check DNS_FORWARDER reachability."
}

# Enable the web service TLS listener with the step-ca PKCS#12 bundle. The
# settings API parameters (webServiceEnableTls, webServiceTlsPort,
# webServiceTlsCertificatePath, webServiceTlsCertificatePassword) were
# verified against the pinned 13.4.2 image; see TECHNITIUM_API.md. The port is
# the container-internal 53443, which the compose template publishes as
# TECHNITIUM_HTTPS_PORT.
configure_technitium_web_tls() {
  local console_url token pfx_password set_response status
  console_url="http://127.0.0.1:${TECHNITIUM_HTTP_PORT}"

  token="$(technitium_api_login_token)"
  pfx_password="$(cat "${TECHNITIUM_CERT_DIR}/technitium-pfx-password")"

  set_response="$(curl --silent --show-error --get \
    --data-urlencode "token=${token}" \
    --data-urlencode "webServiceEnableTls=true" \
    --data-urlencode "webServiceTlsPort=53443" \
    --data-urlencode "webServiceTlsCertificatePath=/etc/provider-box/technitium-certs/technitium.pfx" \
    --data-urlencode "webServiceTlsCertificatePassword=${pfx_password}" \
    "${console_url}/api/settings/set" || true)"
  status="$(printf '%s' "${set_response}" | technitium_json_string_field status)"
  [[ "${status}" == "ok" ]] || \
    fail "Failed to enable Technitium web service TLS. Response: ${set_response}"

  echo "Technitium web service TLS enabled with the step-ca certificate."
}

verify_technitium_https() {
  local attempt
  local https_url="https://${DNS_FQDN}:${TECHNITIUM_HTTPS_PORT}/"

  echo "Verifying Technitium HTTPS at ${https_url} with the step-ca chain."
  for attempt in $(seq 1 30); do
    if curl --silent --show-error --fail \
      --output /dev/null \
      --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
      --resolve "${DNS_FQDN}:${TECHNITIUM_HTTPS_PORT}:127.0.0.1" \
      "${https_url}" 2>/dev/null; then
      echo "Technitium HTTPS is serving the step-ca-issued certificate."
      return 0
    fi
    sleep 2
  done
  fail "Technitium HTTPS did not become ready at ${https_url} with the step-ca certificate. Check 'docker compose logs'."
}

# Create a non-expiring API token via /api/user/createToken (verified against
# 13.4.2; see TECHNITIUM_API.md) and store it where --dns-sync expects its
# token file. Idempotent: a stored token is validated with a zones/list probe
# and reused when Technitium still accepts it.
provision_technitium_api_token() {
  local console_url token_file stored status create_response api_token
  console_url="http://127.0.0.1:${TECHNITIUM_HTTP_PORT}"
  token_file="${DNS_SYNC_SECRETS_DIR}/technitium.token"

  install -d -m 0700 "${DNS_SYNC_SECRETS_DIR}"

  if [[ -s "${token_file}" ]]; then
    stored="$(cat "${token_file}")"
    status="$(curl --silent --show-error --get \
      --data-urlencode "token=${stored}" \
      "${console_url}/api/zones/list" | technitium_json_string_field status || true)"
    if [[ "${status}" == "ok" ]]; then
      echo "Reusing existing Technitium API token: ${token_file}"
      chmod 0600 "${token_file}"
      chown 1000:1000 "${token_file}"
      return 0
    fi
    echo "Stored Technitium API token is no longer valid; creating a replacement."
  fi

  create_response="$(curl --silent --show-error \
    "${console_url}/api/user/createToken?user=admin&pass=admin&tokenName=provider-box-dns-sync" || true)"
  api_token="$(printf '%s' "${create_response}" | technitium_json_string_field token)"
  [[ -n "${api_token}" ]] || \
    fail "Failed to create a Technitium API token. Response: ${create_response}"

  install -m 0600 /dev/null "${token_file}"
  printf '%s' "${api_token}" > "${token_file}"
  chmod 0600 "${token_file}"
  chown 1000:1000 "${token_file}"
  echo "Provisioned a Technitium API token for dns-sync at: ${token_file}"
}

do_technitium() {
  require_technitium_vars
  require_technitium_ca_vars
  resolve_dns_forwarder
  common_pkgs
  docker_pkgs
  require_ca_ready_for_technitium
  bootstrap_technitium_layout
  issue_technitium_certificates
  build_technitium_pfx
  render_technitium_stack
  (
    cd "${WORKDIR}/technitium"
    docker compose down || true
  )
  preflight_technitium_port_53
  (
    cd "${WORKDIR}/technitium"
    docker compose up -d
  )
  ufw allow 53/tcp || true
  ufw allow 53/udp || true
  ufw allow "${TECHNITIUM_HTTP_PORT}/tcp" || true
  ufw allow "${TECHNITIUM_HTTPS_PORT}/tcp" || true
  verify_technitium_dns_listener
  configure_technitium_forwarder
  verify_technitium_external_resolution
  configure_technitium_web_tls
  verify_technitium_https
  provision_technitium_api_token
  point_host_resolver_at_technitium
  echo "Technitium is ready. Web console: http://${DNS_FQDN}:${TECHNITIUM_HTTP_PORT} and https://${DNS_FQDN}:${TECHNITIUM_HTTPS_PORT}"
  echo "Web service HTTPS is enabled with the step-ca-issued certificate."
  echo "An API token for dns-sync is stored at ${DNS_SYNC_SECRETS_DIR}/technitium.token."
}

remove_technitium() {
  local runtime_dir="${WORKDIR}/technitium"
  local compose_file="${runtime_dir}/docker-compose.yml"

  require_technitium_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    (
      cd "${runtime_dir}"
      docker compose down || true
    )
  fi

  rm -rf "${runtime_dir}"
  restore_host_resolver
  echo "Removed Technitium containers and runtime files under ${runtime_dir}. Persistent data in ${TECHNITIUM_DATA_DIR} and certificates in ${TECHNITIUM_CERT_DIR} were preserved."
}
