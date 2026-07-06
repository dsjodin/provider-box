#!/usr/bin/env bash

require_keycloak_ca_vars() {
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

require_keycloak_remove_vars() {
  local var
  for var in WORKDIR KEYCLOAK_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${KEYCLOAK_DIR}"
}

json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf '%s' "${value}"
}

validate_keycloak_bootstrap_value() {
  local value="$1"
  local name="$2"
  [[ -n "${value}" ]] || fail "${name} must not be empty"
  validate_var_not_placeholder "${value}"
}

validate_keycloak_redirect_uri() {
  local uri="$1"
  [[ "${uri}" == https://* ]] || fail "KEYCLOAK_BOOTSTRAP_CLIENT_REDIRECT_URIS entries must start with https://: ${uri}"
  [[ "${uri}" != *'"'* ]] || fail "KEYCLOAK_BOOTSTRAP_CLIENT_REDIRECT_URIS entries must not contain double quotes"
}

build_keycloak_redirect_uris_json() {
  local uris="${KEYCLOAK_BOOTSTRAP_CLIENT_REDIRECT_URIS}"
  local uri
  local redirect_uris_json=""

  IFS=',' read -r -a keycloak_redirect_uris <<< "${uris}"
  ((${#keycloak_redirect_uris[@]} > 0)) || fail "KEYCLOAK_BOOTSTRAP_CLIENT_REDIRECT_URIS must not be empty"

  for uri in "${keycloak_redirect_uris[@]}"; do
    [[ -n "${uri}" ]] || fail "KEYCLOAK_BOOTSTRAP_CLIENT_REDIRECT_URIS contains an empty entry"
    validate_keycloak_redirect_uri "${uri}"
    if [[ -n "${redirect_uris_json}" ]]; then
      redirect_uris_json="${redirect_uris_json}, "
    fi
    redirect_uris_json="${redirect_uris_json}\"$(json_escape "${uri}")\""
  done

  KEYCLOAK_BOOTSTRAP_CLIENT_REDIRECT_URIS_JSON="${redirect_uris_json}"
  export KEYCLOAK_BOOTSTRAP_CLIENT_REDIRECT_URIS_JSON
}

build_keycloak_bootstrap_user_block() {
  local username_escaped password_escaped email_escaped group_escaped

  if [[ -z "${KEYCLOAK_BOOTSTRAP_USERNAME:-}" && -z "${KEYCLOAK_BOOTSTRAP_USER_PASSWORD:-}" ]]; then
    KEYCLOAK_BOOTSTRAP_USERS_BLOCK=""
    export KEYCLOAK_BOOTSTRAP_USERS_BLOCK
    return
  fi

  [[ -n "${KEYCLOAK_BOOTSTRAP_USERNAME:-}" ]] || \
    fail "KEYCLOAK_BOOTSTRAP_USERNAME is required when KEYCLOAK_BOOTSTRAP_USER_PASSWORD is set"
  [[ -n "${KEYCLOAK_BOOTSTRAP_USER_PASSWORD:-}" ]] || \
    fail "KEYCLOAK_BOOTSTRAP_USER_PASSWORD is required when KEYCLOAK_BOOTSTRAP_USERNAME is set"

  validate_keycloak_bootstrap_value "${KEYCLOAK_BOOTSTRAP_USERNAME}" "KEYCLOAK_BOOTSTRAP_USERNAME"
  validate_keycloak_bootstrap_value "${KEYCLOAK_BOOTSTRAP_USER_PASSWORD}" "KEYCLOAK_BOOTSTRAP_USER_PASSWORD"
  validate_keycloak_bootstrap_value "${KEYCLOAK_BOOTSTRAP_USER_EMAIL_DOMAIN:-}" "KEYCLOAK_BOOTSTRAP_USER_EMAIL_DOMAIN"
  validate_var_fqdn "${KEYCLOAK_BOOTSTRAP_USER_EMAIL_DOMAIN}"

  username_escaped="$(json_escape "${KEYCLOAK_BOOTSTRAP_USERNAME}")"
  password_escaped="$(json_escape "${KEYCLOAK_BOOTSTRAP_USER_PASSWORD}")"
  email_escaped="$(json_escape "${KEYCLOAK_BOOTSTRAP_USERNAME}@${KEYCLOAK_BOOTSTRAP_USER_EMAIL_DOMAIN}")"
  group_escaped="$(json_escape "/${KEYCLOAK_BOOTSTRAP_GROUP_NAME}")"

  KEYCLOAK_BOOTSTRAP_USERS_BLOCK=$(cat <<EOF
  ,
  "users": [
    {
      "username": "${username_escaped}",
      "email": "${email_escaped}",
      "enabled": true,
      "emailVerified": true,
      "groups": [
        "${group_escaped}"
      ],
      "credentials": [
        {
          "type": "password",
          "value": "${password_escaped}",
          "temporary": false
        }
      ]
    }
  ]
EOF
)
  export KEYCLOAK_BOOTSTRAP_USERS_BLOCK
}

render_keycloak_realm_import() {
  local import_dir="${WORKDIR}/keycloak/import"

  build_keycloak_redirect_uris_json
  build_keycloak_bootstrap_user_block
  install -d -m 0755 "${import_dir}"
  render_template "${TEMPLATE_DIR}/keycloak-realm.json.tpl" "${import_dir}/provider-box-realm.json"
}

require_ca_ready_for_keycloak() {
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

write_keycloak_full_chain_bundle() {
  local cert_dir="$1"
  local cert_file="${cert_dir}/keycloak.crt"
  local full_chain_file="${cert_dir}/keycloak-full-chain.pem"
  local cert_count

  awk '
    /-----BEGIN CERTIFICATE-----/ { in_cert = 1 }
    in_cert { print }
    /-----END CERTIFICATE-----/ { exit }
  ' "${cert_file}" > "${full_chain_file}" || \
    fail "Failed to extract the Keycloak leaf certificate."

  cat "${CA_DATA_DIR}/certs/intermediate_ca.crt" "${CA_DATA_DIR}/certs/root_ca.crt" >> "${full_chain_file}" || \
    fail "Failed to build the Keycloak full certificate chain bundle."

  cert_count="$(grep -c "BEGIN CERTIFICATE" "${full_chain_file}")"
  [[ "${cert_count}" -eq 3 ]] || \
    fail "Keycloak full certificate chain bundle must contain exactly 3 certificates."

  chmod 0644 "${full_chain_file}"
  chown 1000:1000 "${full_chain_file}"
}

normalize_keycloak_certificate_permissions() {
  local cert_dir="$1"
  local cert_file="${cert_dir}/keycloak.crt"
  local key_file="${cert_dir}/keycloak.key"
  local chain_file="${cert_dir}/keycloak-ca-chain.pem"
  local roots_file="${cert_dir}/keycloak-ca-roots.pem"
  local full_chain_file="${cert_dir}/keycloak-full-chain.pem"

  chmod 0755 "${cert_dir}"
  chown 1000:1000 "${cert_dir}"
  [[ -f "${cert_file}" ]] && chmod 0644 "${cert_file}" && chown 1000:1000 "${cert_file}"
  [[ -f "${chain_file}" ]] && chmod 0644 "${chain_file}" && chown 1000:1000 "${chain_file}"
  [[ -f "${roots_file}" ]] && chmod 0644 "${roots_file}" && chown 1000:1000 "${roots_file}"
  [[ -f "${full_chain_file}" ]] && chmod 0644 "${full_chain_file}" && chown 1000:1000 "${full_chain_file}"
  [[ -f "${key_file}" ]] && chmod 0600 "${key_file}" && chown 1000:1000 "${key_file}"
}

issue_keycloak_certificates() {
  local cert_dir="${KEYCLOAK_DIR}/certs"
  local cert_file="${cert_dir}/keycloak.crt"
  local key_file="${cert_dir}/keycloak.key"
  local cert_dir_in_container="/etc/provider-box/keycloak-certs"
  local password_file_in_container="/home/step/${CA_PASSWORD_FILE#${CA_DATA_DIR}/}"

  install -d -m 0755 "${WORKDIR}/keycloak" "${cert_dir}" "${KEYCLOAK_DIR}/data"
  if [[ "$(stat -c %u "${cert_dir}")" != "1000" ]]; then
    chown 1000:1000 "${cert_dir}"
  fi

  if certificate_matches_dns_identity "${cert_file}" "${key_file}" "${KEYCLOAK_FQDN}"; then
    echo "Reusing existing Keycloak certificate for ${KEYCLOAK_FQDN}."
    write_keycloak_full_chain_bundle "${cert_dir}"
    normalize_keycloak_certificate_permissions "${cert_dir}"
    chown 1000:1000 "${KEYCLOAK_DIR}/data"
    return
  fi

  if [[ -f "${cert_file}" || -f "${key_file}" ]]; then
    echo "Existing Keycloak certificate is not valid for ${KEYCLOAK_FQDN}; issuing replacement."
  else
    echo "Issuing Keycloak certificate for ${KEYCLOAK_FQDN}."
  fi
  rm -f \
    "${cert_file}" \
    "${key_file}" \
    "${cert_dir}/keycloak-ca-chain.pem" \
    "${cert_dir}/keycloak-ca-roots.pem" \
    "${cert_dir}/keycloak-full-chain.pem" \
    "${cert_dir}/keycloak-leaf.crt"

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca certificate "${KEYCLOAK_FQDN}" "${cert_dir_in_container}/keycloak-leaf.crt" "${cert_dir_in_container}/keycloak.key" \
      --san "${KEYCLOAK_FQDN}" \
      --not-after "${SERVICE_CERT_DURATION}" \
      --issuer "${CA_PROVISIONER_NAME}" \
      --provisioner-password-file "${password_file_in_container}" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to issue a Keycloak certificate from step-ca."

  mv "${cert_dir}/keycloak-leaf.crt" "${cert_dir}/keycloak.crt" || \
    fail "Failed to store the Keycloak certificate chain."

  cat "${CA_DATA_DIR}/certs/intermediate_ca.crt" "${CA_DATA_DIR}/certs/root_ca.crt" > "${cert_dir}/keycloak-ca-chain.pem" || \
    fail "Failed to build the Keycloak CA chain bundle."
  write_keycloak_full_chain_bundle "${cert_dir}"

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca roots "${cert_dir_in_container}/keycloak-ca-roots.pem" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to fetch the step-ca root bundle for Keycloak."

  normalize_keycloak_certificate_permissions "${cert_dir}"
  chown 1000:1000 "${KEYCLOAK_DIR}/data"
}

wait_for_keycloak_https() {
  local attempt http_code
  local keycloak_url="https://${KEYCLOAK_FQDN}:${KEYCLOAK_PORT}/"

  echo "Waiting for Keycloak HTTPS endpoint to become ready at ${keycloak_url}."

  for attempt in $(seq 1 45); do
    http_code="$(curl --silent --show-error \
      --output /dev/null \
      --write-out '%{http_code}' \
      --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
      --resolve "${KEYCLOAK_FQDN}:${KEYCLOAK_PORT}:127.0.0.1" \
      "${keycloak_url}" || true)"

    case "${http_code}" in
      200|301|302)
        echo "Keycloak is ready."
        return 0
        ;;
    esac

    sleep 2
  done

  fail "Keycloak failed readiness check at ${keycloak_url}. Last observed HTTP status: ${http_code}. Check logs with: docker compose logs"
}

do_keycloak() {
  require_keycloak_vars
  require_keycloak_ca_vars
  common_pkgs
  docker_pkgs
  require_ca_ready_for_keycloak
  render_keycloak_realm_import
  issue_keycloak_certificates
  render_template "${TEMPLATE_DIR}/docker-compose.keycloak.yml.tpl" "${WORKDIR}/keycloak/docker-compose.yml"
  (
    cd "${WORKDIR}/keycloak"
    docker compose down || true
    docker compose up -d
  )
  ufw allow "${KEYCLOAK_PORT}/tcp" || true
  wait_for_keycloak_https
}

remove_keycloak() {
  local runtime_dir="${WORKDIR}/keycloak"
  local compose_file="${runtime_dir}/docker-compose.yml"

  require_keycloak_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    (
      cd "${runtime_dir}"
      docker compose down || true
    )
  fi

  rm -rf "${runtime_dir}"
  echo "Removed Keycloak containers and runtime files. Persistent data in ${KEYCLOAK_DIR} was preserved."
}
