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
  for var in WORKDIR DNS_FQDN TECHNITIUM_HTTP_PORT TECHNITIUM_HTTPS_PORT TECHNITIUM_DATA_DIR TECHNITIUM_CERT_DIR TECHNITIUM_IMAGE; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

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
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca roots "${cert_dir_in_container}/technitium-ca-roots.pem" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to fetch the step-ca root bundle for Technitium."

  normalize_technitium_certificate_permissions "${cert_dir}"
}

render_technitium_stack() {
  render_template "${TEMPLATE_DIR}/docker-compose.technitium.yml.tpl" "${WORKDIR}/technitium/docker-compose.yml"
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

do_technitium() {
  require_technitium_vars
  require_technitium_ca_vars
  common_pkgs
  docker_pkgs
  require_ca_ready_for_technitium
  bootstrap_technitium_layout
  issue_technitium_certificates
  render_technitium_stack
  (
    cd "${WORKDIR}/technitium"
    docker compose down || true
    docker compose up -d
  )
  ufw allow 53/tcp || true
  ufw allow 53/udp || true
  ufw allow "${TECHNITIUM_HTTP_PORT}/tcp" || true
  ufw allow "${TECHNITIUM_HTTPS_PORT}/tcp" || true
  verify_technitium_dns_listener
  echo "Technitium is ready. Web console: http://${DNS_FQDN}:${TECHNITIUM_HTTP_PORT}"
  echo "Step-ca-issued certificate is mounted at /etc/provider-box/technitium-certs inside the container."
  echo "Capture the API token from the console before enabling seed apply or dns-sync."
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
  echo "Removed Technitium containers and runtime files under ${runtime_dir}. Persistent data in ${TECHNITIUM_DATA_DIR} and certificates in ${TECHNITIUM_CERT_DIR} were preserved."
}
