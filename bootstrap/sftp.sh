#!/usr/bin/env bash

require_sftp_ca_vars() {
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

require_sftp_vars() {
  local var
  for var in WORKDIR SFTP_FQDN SFTP_PORT SFTP_ADMIN_PORT SFTP_ADMIN_USER SFTP_ADMIN_PASSWORD SFTP_DATA_DIR SFTP_HOME_DIR SFTP_CERT_DIR SFTPGO_IMAGE; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_fqdn "${SFTP_FQDN}"
  validate_var_port "${SFTP_PORT}"
  validate_var_port "${SFTP_ADMIN_PORT}"
  validate_var_not_placeholder "${SFTP_ADMIN_PASSWORD}"
  validate_var_path "${SFTP_DATA_DIR}"
  validate_var_path "${SFTP_HOME_DIR}"
  validate_var_path "${SFTP_CERT_DIR}"
  [[ "${SFTPGO_IMAGE}" == *:* ]] || fail "SFTPGO_IMAGE must include an explicit image tag"
  [[ "${SFTPGO_IMAGE}" != *:latest ]] || fail "SFTPGO_IMAGE must not use the latest tag"
  validate_sftp_backup_user_vars
}

validate_sftp_backup_user_vars() {
  local configured=0

  [[ -n "${SFTP_BACKUP_USERNAME:-}" ]] && configured=1
  [[ -n "${SFTP_BACKUP_PASSWORD:-}" ]] && configured=1
  [[ -n "${SFTP_BACKUP_HOME_DIR:-}" ]] && configured=1

  [[ "${configured}" -eq 1 ]] || return 0

  [[ -n "${SFTP_BACKUP_USERNAME:-}" ]] || fail "SFTP_BACKUP_USERNAME is required when configuring the SFTP backup user"
  [[ -n "${SFTP_BACKUP_PASSWORD:-}" ]] || fail "SFTP_BACKUP_PASSWORD is required when configuring the SFTP backup user"
  [[ -n "${SFTP_BACKUP_HOME_DIR:-}" ]] || fail "SFTP_BACKUP_HOME_DIR is required when configuring the SFTP backup user"
  [[ "${SFTP_BACKUP_USERNAME}" =~ ^[A-Za-z0-9._-]+$ ]] || \
    fail "SFTP_BACKUP_USERNAME may only contain letters, numbers, dots, underscores, and hyphens"
  validate_var_not_placeholder "${SFTP_BACKUP_PASSWORD}"
  validate_var_path "${SFTP_BACKUP_HOME_DIR}"
  [[ "${SFTP_BACKUP_HOME_DIR}" == "${SFTP_DATA_DIR}"/* ]] || \
    fail "SFTP_BACKUP_HOME_DIR must be located under SFTP_DATA_DIR so the SFTPGo container can use it"
}

require_sftp_remove_vars() {
  local var
  for var in WORKDIR SFTP_DATA_DIR SFTP_HOME_DIR SFTP_CERT_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${SFTP_DATA_DIR}"
  validate_var_path "${SFTP_HOME_DIR}"
  validate_var_path "${SFTP_CERT_DIR}"
}

require_ca_ready_for_sftp() {
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

normalize_sftp_certificate_permissions() {
  local cert_dir="$1"
  local cert_file="${cert_dir}/sftpgo.crt"
  local key_file="${cert_dir}/sftpgo.key"
  local chain_file="${cert_dir}/sftpgo-ca-chain.pem"
  local roots_file="${cert_dir}/sftpgo-ca-roots.pem"

  chmod 0755 "${cert_dir}"
  chown 1000:1000 "${cert_dir}"
  [[ -f "${cert_file}" ]] && chmod 0644 "${cert_file}" && chown 1000:1000 "${cert_file}"
  [[ -f "${chain_file}" ]] && chmod 0644 "${chain_file}" && chown 1000:1000 "${chain_file}"
  [[ -f "${roots_file}" ]] && chmod 0644 "${roots_file}" && chown 1000:1000 "${roots_file}"
  [[ -f "${key_file}" ]] && chmod 0600 "${key_file}" && chown 1000:1000 "${key_file}"
}

issue_sftp_certificates() {
  local cert_dir="${SFTP_CERT_DIR}"
  local cert_file="${cert_dir}/sftpgo.crt"
  local key_file="${cert_dir}/sftpgo.key"
  local cert_dir_in_container="/etc/provider-box/sftpgo-certs"
  local password_file_in_container="/home/step/${CA_PASSWORD_FILE#${CA_DATA_DIR}/}"

  install -d -m 0755 "${cert_dir}"
  if [[ "$(stat -c %u "${cert_dir}")" != "1000" ]]; then
    chown 1000:1000 "${cert_dir}"
  fi

  if certificate_matches_dns_identity "${cert_file}" "${key_file}" "${SFTP_FQDN}"; then
    echo "Reusing existing SFTPGo certificate for ${SFTP_FQDN}."
    normalize_sftp_certificate_permissions "${cert_dir}"
    return
  fi

  if [[ -f "${cert_file}" || -f "${key_file}" ]]; then
    echo "Existing SFTPGo certificate is not valid for ${SFTP_FQDN}; issuing replacement."
  else
    echo "Issuing SFTPGo certificate for ${SFTP_FQDN}."
  fi
  rm -f \
    "${cert_file}" \
    "${key_file}" \
    "${cert_dir}/sftpgo-ca-chain.pem" \
    "${cert_dir}/sftpgo-ca-roots.pem" \
    "${cert_dir}/sftpgo-leaf.crt"

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca certificate "${SFTP_FQDN}" "${cert_dir_in_container}/sftpgo-leaf.crt" "${cert_dir_in_container}/sftpgo.key" \
      --san "${SFTP_FQDN}" \
      --not-after "${SERVICE_CERT_DURATION}" \
      --issuer "${CA_PROVISIONER_NAME}" \
      --provisioner-password-file "${password_file_in_container}" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to issue an SFTPGo certificate from step-ca."

  mv "${cert_dir}/sftpgo-leaf.crt" "${cert_dir}/sftpgo.crt" || \
    fail "Failed to store the SFTPGo certificate chain."

  cat "${CA_DATA_DIR}/certs/intermediate_ca.crt" "${CA_DATA_DIR}/certs/root_ca.crt" > "${cert_dir}/sftpgo-ca-chain.pem" || \
    fail "Failed to build the SFTPGo CA chain bundle."

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca roots "${cert_dir_in_container}/sftpgo-ca-roots.pem" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to fetch the step-ca root bundle for SFTPGo."

  normalize_sftp_certificate_permissions "${cert_dir}"
}

wait_for_sftp_admin_https() {
  local attempt http_code
  local sftp_admin_url="https://${SFTP_FQDN}:${SFTP_ADMIN_PORT}/"

  echo "Waiting for SFTPGo admin UI to become ready at ${sftp_admin_url}."

  for attempt in $(seq 1 60); do
    http_code="$(curl --silent --show-error \
      --output /dev/null \
      --write-out '%{http_code}' \
      --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
      --resolve "${SFTP_FQDN}:${SFTP_ADMIN_PORT}:127.0.0.1" \
      --location \
      "${sftp_admin_url}" || true)"

    [[ "${http_code}" == "200" ]] && return 0

    sleep 2
  done

  fail "SFTPGo admin UI did not become ready at ${sftp_admin_url}. Last observed HTTP status: ${http_code}. Check 'docker compose ps' and 'docker compose logs'."
}

sftp_backup_user_configured() {
  [[ -n "${SFTP_BACKUP_USERNAME:-}" && -n "${SFTP_BACKUP_PASSWORD:-}" && -n "${SFTP_BACKUP_HOME_DIR:-}" ]]
}

sftp_json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf '%s' "${value}"
}

sftp_json_string_field() {
  local field="$1"
  sed -n "s/.*\"${field}\":\"\\([^\"]*\\)\".*/\\1/p" | head -n1
}

sftp_backup_home_dir_in_container() {
  local relative="${SFTP_BACKUP_HOME_DIR#${SFTP_DATA_DIR}/}"
  printf '/srv/sftpgo/%s' "${relative}"
}

sftpgo_api_token() {
  local response token

  response="$(curl --silent --show-error --fail \
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
    --resolve "${SFTP_FQDN}:${SFTP_ADMIN_PORT}:127.0.0.1" \
    --user "${SFTP_ADMIN_USER}:${SFTP_ADMIN_PASSWORD}" \
    "https://${SFTP_FQDN}:${SFTP_ADMIN_PORT}/api/v2/token")" || \
    fail "Failed to obtain an SFTPGo API token for ${SFTP_ADMIN_USER}."

  token="$(printf '%s' "${response}" | sftp_json_string_field access_token)"
  [[ -n "${token}" ]] || fail "Failed to extract the SFTPGo API token from the token response."
  printf '%s' "${token}"
}

ensure_sftp_backup_user() {
  local token user_url http_code payload backup_home_in_container

  sftp_backup_user_configured || return 0

  install -d -m 0755 "${SFTP_BACKUP_HOME_DIR}"
  chown -R 1000:1000 "${SFTP_BACKUP_HOME_DIR}"
  chmod 0755 "${SFTP_BACKUP_HOME_DIR}"

  token="$(sftpgo_api_token)"
  user_url="https://${SFTP_FQDN}:${SFTP_ADMIN_PORT}/api/v2/users/${SFTP_BACKUP_USERNAME}"

  http_code="$(curl --silent --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
    --resolve "${SFTP_FQDN}:${SFTP_ADMIN_PORT}:127.0.0.1" \
    -H "Authorization: Bearer ${token}" \
    "${user_url}" || true)"

  [[ "${http_code}" == "200" ]] && return 0
  [[ "${http_code}" == "404" ]] || \
    fail "Failed to check SFTPGo backup user ${SFTP_BACKUP_USERNAME}. HTTP status: ${http_code}"

  backup_home_in_container="$(sftp_backup_home_dir_in_container)"
  payload="{\"username\":\"$(sftp_json_escape "${SFTP_BACKUP_USERNAME}")\",\"password\":\"$(sftp_json_escape "${SFTP_BACKUP_PASSWORD}")\",\"home_dir\":\"$(sftp_json_escape "${backup_home_in_container}")\",\"status\":1,\"permissions\":{\"/\":[\"*\"]},\"filesystem\":{\"provider\":0}}"

  http_code="$(curl --silent --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
    --resolve "${SFTP_FQDN}:${SFTP_ADMIN_PORT}:127.0.0.1" \
    -H "Authorization: Bearer ${token}" \
    -H "Content-Type: application/json" \
    --data "${payload}" \
    "https://${SFTP_FQDN}:${SFTP_ADMIN_PORT}/api/v2/users" || true)"

  [[ "${http_code}" == "201" ]] || \
    fail "Failed to create SFTPGo backup user ${SFTP_BACKUP_USERNAME}. HTTP status: ${http_code}"
}

do_sftp() {
  require_sftp_vars
  require_sftp_ca_vars
  common_pkgs
  docker_pkgs
  require_ca_ready_for_sftp
  issue_sftp_certificates
  install -d -m 0755 "${WORKDIR}/sftpgo" "${SFTP_DATA_DIR}" "${SFTP_HOME_DIR}" "${SFTP_CERT_DIR}"
  chown -R 1000:1000 "${SFTP_DATA_DIR}" "${SFTP_HOME_DIR}"
  chmod 0755 "${SFTP_DATA_DIR}" "${SFTP_HOME_DIR}"
  render_template "${TEMPLATE_DIR}/docker-compose.sftpgo.yml.tpl" "${WORKDIR}/sftpgo/docker-compose.yml"
  (
    cd "${WORKDIR}/sftpgo"
    docker compose down || true
    docker compose up -d
  )
  ufw allow "${SFTP_PORT}/tcp" || true
  ufw allow "${SFTP_ADMIN_PORT}/tcp" || true
  wait_for_sftp_admin_https
  ensure_sftp_backup_user
}

remove_sftp() {
  local runtime_dir="${WORKDIR}/sftpgo"
  local compose_file="${runtime_dir}/docker-compose.yml"

  require_sftp_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    (
      cd "${runtime_dir}"
      docker compose down || true
    )
  fi

  rm -rf "${runtime_dir}"
  echo "Removed SFTPGo containers and runtime files. Persistent data in ${SFTP_DATA_DIR}, ${SFTP_HOME_DIR}, and ${SFTP_CERT_DIR} was preserved."
}
