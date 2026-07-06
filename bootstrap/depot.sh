#!/usr/bin/env bash

require_depot_ca_vars() {
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

require_depot_vars() {
  local var
  for var in WORKDIR DEPOT_FQDN DEPOT_HTTP_PORT DEPOT_HTTPS_PORT DEPOT_DIR DEPOT_DATA_DIR DEPOT_CERT_DIR DEPOT_AUTH_DIR DEPOT_BASIC_AUTH_USER DEPOT_BASIC_AUTH_PASSWORD DEPOT_IMAGE; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_fqdn "${DEPOT_FQDN}"
  validate_var_port "${DEPOT_HTTP_PORT}"
  validate_var_port "${DEPOT_HTTPS_PORT}"
  validate_var_path "${DEPOT_DIR}"
  validate_var_path "${DEPOT_DATA_DIR}"
  validate_var_path "${DEPOT_CERT_DIR}"
  validate_var_path "${DEPOT_AUTH_DIR}"
  [[ "${DEPOT_HTTP_PORT}" != "${DEPOT_HTTPS_PORT}" ]] || \
    fail "DEPOT_HTTP_PORT and DEPOT_HTTPS_PORT must be different"
  [[ -n "${DEPOT_BASIC_AUTH_USER}" ]] || fail "DEPOT_BASIC_AUTH_USER must not be empty"
  validate_var_not_placeholder "${DEPOT_BASIC_AUTH_PASSWORD}"
  [[ "${DEPOT_IMAGE}" == *:* ]] || fail "DEPOT_IMAGE must include an explicit image tag"
  [[ "${DEPOT_IMAGE}" != *:latest ]] || fail "DEPOT_IMAGE must not use the latest tag"
  [[ "${DEPOT_DATA_DIR}" != "${WORKDIR}/depot" && "${DEPOT_DATA_DIR}" != "${WORKDIR}/depot/"* ]] || \
    fail "DEPOT_DATA_DIR must not be inside ${WORKDIR}/depot so --remove preserves depot content"
  [[ "${DEPOT_CERT_DIR}" != "${WORKDIR}/depot" && "${DEPOT_CERT_DIR}" != "${WORKDIR}/depot/"* ]] || \
    fail "DEPOT_CERT_DIR must not be inside ${WORKDIR}/depot so --remove preserves depot certificates"
}

require_depot_remove_vars() {
  local var
  for var in WORKDIR DEPOT_DIR DEPOT_DATA_DIR DEPOT_CERT_DIR DEPOT_AUTH_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${DEPOT_DIR}"
  validate_var_path "${DEPOT_DATA_DIR}"
  validate_var_path "${DEPOT_CERT_DIR}"
  validate_var_path "${DEPOT_AUTH_DIR}"
}

depot_pkgs() {
  apt_update_once
  install_pkg apache2-utils
  require_command htpasswd
}

require_ca_ready_for_depot() {
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

normalize_depot_certificate_permissions() {
  local cert_dir="$1"
  local cert_file="${cert_dir}/depot.crt"
  local key_file="${cert_dir}/depot.key"
  local chain_file="${cert_dir}/depot-ca-chain.pem"
  local roots_file="${cert_dir}/depot-ca-roots.pem"

  chmod 0755 "${cert_dir}"
  chown 1000:1000 "${cert_dir}"
  [[ -f "${cert_file}" ]] && chmod 0644 "${cert_file}" && chown 1000:1000 "${cert_file}"
  [[ -f "${chain_file}" ]] && chmod 0644 "${chain_file}" && chown 1000:1000 "${chain_file}"
  [[ -f "${roots_file}" ]] && chmod 0644 "${roots_file}" && chown 1000:1000 "${roots_file}"
  [[ -f "${key_file}" ]] && chmod 0600 "${key_file}" && chown 1000:1000 "${key_file}"
}

bootstrap_depot_layout() {
  install -d -m 0755 \
    "${WORKDIR}/depot" \
    "${DEPOT_DIR}" \
    "${DEPOT_DATA_DIR}" \
    "${DEPOT_CERT_DIR}" \
    "${DEPOT_AUTH_DIR}" \
    "${DEPOT_DATA_DIR}/PROD/COMP" \
    "${DEPOT_DATA_DIR}/PROD/metadata" \
    "${DEPOT_DATA_DIR}/PROD/vsan/hcl"
}

issue_depot_certificates() {
  local cert_dir="${DEPOT_CERT_DIR}"
  local cert_file="${cert_dir}/depot.crt"
  local key_file="${cert_dir}/depot.key"
  local cert_dir_in_container="/etc/provider-box/depot-certs"
  local password_file_in_container="/home/step/${CA_PASSWORD_FILE#${CA_DATA_DIR}/}"

  install -d -m 0755 "${cert_dir}"
  if [[ "$(stat -c %u "${cert_dir}")" != "1000" ]]; then
    chown 1000:1000 "${cert_dir}"
  fi

  if certificate_matches_dns_identity "${cert_file}" "${key_file}" "${DEPOT_FQDN}"; then
    echo "Reusing existing depot certificate for ${DEPOT_FQDN}."
    normalize_depot_certificate_permissions "${cert_dir}"
    return
  fi

  if [[ -f "${cert_file}" || -f "${key_file}" ]]; then
    echo "Existing depot certificate is not valid for ${DEPOT_FQDN}; issuing replacement."
  else
    echo "Issuing depot certificate for ${DEPOT_FQDN}."
  fi
  rm -f \
    "${cert_file}" \
    "${key_file}" \
    "${cert_dir}/depot-ca-chain.pem" \
    "${cert_dir}/depot-ca-roots.pem" \
    "${cert_dir}/depot-leaf.crt"

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca certificate "${DEPOT_FQDN}" "${cert_dir_in_container}/depot-leaf.crt" "${cert_dir_in_container}/depot.key" \
      --san "${DEPOT_FQDN}" \
      --not-after "${SERVICE_CERT_DURATION}" \
      --issuer "${CA_PROVISIONER_NAME}" \
      --provisioner-password-file "${password_file_in_container}" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to issue a depot certificate from step-ca."

  mv "${cert_dir}/depot-leaf.crt" "${cert_dir}/depot.crt" || \
    fail "Failed to store the depot certificate chain."

  cat "${CA_DATA_DIR}/certs/intermediate_ca.crt" "${CA_DATA_DIR}/certs/root_ca.crt" > "${cert_dir}/depot-ca-chain.pem" || \
    fail "Failed to build the depot CA chain bundle."

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca roots "${cert_dir_in_container}/depot-ca-roots.pem" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to fetch the step-ca root bundle for the depot."

  normalize_depot_certificate_permissions "${cert_dir}"
}

render_depot_basic_auth() {
  local auth_file="${DEPOT_AUTH_DIR}/htpasswd"

  install -d -m 0755 "${DEPOT_AUTH_DIR}"
  htpasswd -bc "${auth_file}" "${DEPOT_BASIC_AUTH_USER}" "${DEPOT_BASIC_AUTH_PASSWORD}" >/dev/null || \
    fail "Failed to generate the depot htpasswd file."
  chmod 0644 "${auth_file}"
}

render_depot_stack() {
  DOLLAR='$'
  export DOLLAR
  render_template "${TEMPLATE_DIR}/depot-nginx.conf.tpl" "${WORKDIR}/depot/nginx.conf"
  render_template "${TEMPLATE_DIR}/docker-compose.depot.yml.tpl" "${WORKDIR}/depot/docker-compose.yml"
}

verify_depot_healthz() {
  local scheme="$1"
  local port="$2"
  local curl_args
  local healthz_url response

  healthz_url="${scheme}://${DEPOT_FQDN}:${port}/healthz"
  curl_args=(
    --silent
    --show-error
    --fail
    --resolve "${DEPOT_FQDN}:${port}:127.0.0.1"
  )

  if [[ "${scheme}" == "https" ]]; then
    curl_args+=(--cacert "${CA_DATA_DIR}/certs/root_ca.crt")
  fi

  response="$(curl "${curl_args[@]}" "${healthz_url}" || true)"
  [[ "${response}" == "ok" ]] || return 1
}

wait_for_depot_http() {
  local attempt
  local depot_http_url="http://${DEPOT_FQDN}:${DEPOT_HTTP_PORT}/healthz"

  echo "Waiting for depot HTTP endpoint to become ready at ${depot_http_url}."

  for attempt in $(seq 1 60); do
    if verify_depot_healthz "http" "${DEPOT_HTTP_PORT}"; then
      return 0
    fi
    sleep 2
  done

  fail "Depot HTTP endpoint did not become ready at ${depot_http_url}. Check 'docker compose ps' and 'docker compose logs'."
}

wait_for_depot_https() {
  local attempt
  local depot_https_url="https://${DEPOT_FQDN}:${DEPOT_HTTPS_PORT}/healthz"

  echo "Waiting for depot HTTPS endpoint to become ready at ${depot_https_url}."

  for attempt in $(seq 1 60); do
    if verify_depot_healthz "https" "${DEPOT_HTTPS_PORT}"; then
      return 0
    fi
    sleep 2
  done

  fail "Depot HTTPS endpoint did not become ready at ${depot_https_url}. Check 'docker compose ps' and 'docker compose logs'."
}

do_depot() {
  require_depot_vars
  require_depot_ca_vars
  common_pkgs
  docker_pkgs
  depot_pkgs
  require_ca_ready_for_depot
  bootstrap_depot_layout
  issue_depot_certificates
  render_depot_basic_auth
  render_depot_stack
  (
    cd "${WORKDIR}/depot"
    docker compose down || true
    docker compose up -d
  )
  ufw allow "${DEPOT_HTTP_PORT}/tcp" || true
  ufw allow "${DEPOT_HTTPS_PORT}/tcp" || true
  wait_for_depot_http
  wait_for_depot_https
  echo "Depot is ready on http://${DEPOT_FQDN}:${DEPOT_HTTP_PORT} and https://${DEPOT_FQDN}:${DEPOT_HTTPS_PORT}."
}

remove_depot() {
  local runtime_dir="${WORKDIR}/depot"
  local compose_file="${runtime_dir}/docker-compose.yml"

  require_depot_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    (
      cd "${runtime_dir}"
      docker compose down || true
    )
  fi

  rm -rf "${runtime_dir}"
  rm -f "${DEPOT_AUTH_DIR}/htpasswd"
  echo "Removed depot containers and runtime files under ${runtime_dir}. Persistent depot content in ${DEPOT_DATA_DIR} and certificates in ${DEPOT_CERT_DIR} were preserved."
}
