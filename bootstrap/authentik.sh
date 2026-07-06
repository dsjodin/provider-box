#!/usr/bin/env bash

require_authentik_ca_vars() {
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

validate_authentik_secret_key() {
  local value="${AUTHENTIK_SECRET_KEY}"

  [[ "${value}" != CHANGE_ME* ]] || \
    fail "Replace placeholder AUTHENTIK_SECRET_KEY before continuing"
  [[ "${#value}" -ge 50 ]] || \
    fail "AUTHENTIK_SECRET_KEY must be at least 50 characters long"
}

validate_authentik_bootstrap_value() {
  local value="$1"
  local name="$2"
  [[ -n "${value}" ]] || fail "${name} must not be empty"
  validate_var_not_placeholder "${value}"
  [[ "${value}" != *'"'* ]] || fail "${name} must not contain double quotes"
  [[ "${value}" != *"\\"* ]] || fail "${name} must not contain backslashes"
}

require_authentik_vars() {
  local var
  for var in WORKDIR AUTHENTIK_DIR AUTHENTIK_FQDN AUTHENTIK_PORT AUTHENTIK_IMAGE AUTHENTIK_POSTGRES_IMAGE AUTHENTIK_ADMIN_PASSWORD AUTHENTIK_API_TOKEN AUTHENTIK_SECRET_KEY AUTHENTIK_PG_DB AUTHENTIK_PG_USER AUTHENTIK_PG_PASSWORD AUTHENTIK_BOOTSTRAP_CLIENT_ID AUTHENTIK_BOOTSTRAP_CLIENT_SECRET AUTHENTIK_BOOTSTRAP_GROUP_NAME AUTHENTIK_BOOTSTRAP_USERNAME AUTHENTIK_BOOTSTRAP_USER_PASSWORD AUTHENTIK_BOOTSTRAP_USER_EMAIL_DOMAIN AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${AUTHENTIK_DIR}"
  validate_var_fqdn "${AUTHENTIK_FQDN}"
  validate_var_port "${AUTHENTIK_PORT}"
  [[ "${AUTHENTIK_IMAGE}" == *:* ]] || fail "AUTHENTIK_IMAGE must include an explicit image tag"
  [[ "${AUTHENTIK_IMAGE}" != *:latest ]] || fail "AUTHENTIK_IMAGE must not use the latest tag"
  [[ "${AUTHENTIK_POSTGRES_IMAGE}" == *:* ]] || fail "AUTHENTIK_POSTGRES_IMAGE must include an explicit image tag"
  [[ "${AUTHENTIK_POSTGRES_IMAGE}" != *:latest ]] || fail "AUTHENTIK_POSTGRES_IMAGE must not use the latest tag"
  validate_authentik_bootstrap_value "${AUTHENTIK_ADMIN_PASSWORD}" "AUTHENTIK_ADMIN_PASSWORD"
  validate_authentik_bootstrap_value "${AUTHENTIK_API_TOKEN}" "AUTHENTIK_API_TOKEN"
  validate_authentik_secret_key
  validate_authentik_bootstrap_value "${AUTHENTIK_PG_PASSWORD}" "AUTHENTIK_PG_PASSWORD"
  validate_authentik_bootstrap_value "${AUTHENTIK_BOOTSTRAP_CLIENT_ID}" "AUTHENTIK_BOOTSTRAP_CLIENT_ID"
  validate_authentik_bootstrap_value "${AUTHENTIK_BOOTSTRAP_CLIENT_SECRET}" "AUTHENTIK_BOOTSTRAP_CLIENT_SECRET"
  validate_authentik_bootstrap_value "${AUTHENTIK_BOOTSTRAP_GROUP_NAME}" "AUTHENTIK_BOOTSTRAP_GROUP_NAME"
  validate_authentik_bootstrap_value "${AUTHENTIK_BOOTSTRAP_USERNAME}" "AUTHENTIK_BOOTSTRAP_USERNAME"
  validate_authentik_bootstrap_value "${AUTHENTIK_BOOTSTRAP_USER_PASSWORD}" "AUTHENTIK_BOOTSTRAP_USER_PASSWORD"
  validate_var_fqdn "${AUTHENTIK_BOOTSTRAP_USER_EMAIL_DOMAIN}"
}

require_authentik_remove_vars() {
  local var
  for var in WORKDIR AUTHENTIK_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${AUTHENTIK_DIR}"
}

validate_authentik_redirect_uri() {
  local uri="$1"
  [[ "${uri}" == https://* ]] || fail "AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS entries must start with https://: ${uri}"
  [[ "${uri}" != *'"'* ]] || fail "AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS entries must not contain double quotes"
}

build_authentik_redirect_uris_block() {
  local uris="${AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS}"
  local uri
  local block=""

  IFS=',' read -r -a authentik_redirect_uris <<< "${uris}"
  ((${#authentik_redirect_uris[@]} > 0)) || fail "AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS must not be empty"

  for uri in "${authentik_redirect_uris[@]}"; do
    [[ -n "${uri}" ]] || fail "AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS contains an empty entry"
    validate_authentik_redirect_uri "${uri}"
    block="${block}        - matching_mode: strict
          url: \"${uri}\"
"
  done

  AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS_BLOCK="${block%$'\n'}"
  export AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS_BLOCK
}

render_authentik_blueprint() {
  local blueprint_dir="${WORKDIR}/authentik/blueprints"

  build_authentik_redirect_uris_block
  install -d -m 0755 "${blueprint_dir}"
  render_template "${TEMPLATE_DIR}/authentik-blueprint.yaml.tpl" "${blueprint_dir}/provider-box-vcf.yaml"
  chmod 0644 "${blueprint_dir}/provider-box-vcf.yaml"
}

require_ca_ready_for_authentik() {
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

normalize_authentik_certificate_permissions() {
  local cert_dir="$1"
  local fullchain_file="${cert_dir}/fullchain.pem"
  local key_file="${cert_dir}/privkey.pem"

  chmod 0755 "${AUTHENTIK_DIR}/certs" "${cert_dir}"
  chown 1000:1000 "${AUTHENTIK_DIR}/certs" "${cert_dir}"
  [[ -f "${fullchain_file}" ]] && chmod 0644 "${fullchain_file}" && chown 1000:1000 "${fullchain_file}"
  [[ -f "${key_file}" ]] && chmod 0600 "${key_file}" && chown 1000:1000 "${key_file}"
}

prepare_authentik_directories() {
  install -d -m 0755 \
    "${WORKDIR}/authentik" \
    "${AUTHENTIK_DIR}" \
    "${AUTHENTIK_DIR}/certs" \
    "${AUTHENTIK_DIR}/certs/${AUTHENTIK_FQDN}" \
    "${AUTHENTIK_DIR}/data"
  install -d -m 0700 "${AUTHENTIK_DIR}/postgres"
  chown -R 1000:1000 "${AUTHENTIK_DIR}/data"
  chown -R 70:70 "${AUTHENTIK_DIR}/postgres"
  chmod 0700 "${AUTHENTIK_DIR}/postgres"
}

issue_authentik_certificates() {
  local cert_dir="${AUTHENTIK_DIR}/certs/${AUTHENTIK_FQDN}"
  local fullchain_file="${cert_dir}/fullchain.pem"
  local key_file="${cert_dir}/privkey.pem"
  local cert_dir_in_container="/etc/provider-box/authentik-certs"
  local password_file_in_container="/home/step/${CA_PASSWORD_FILE#${CA_DATA_DIR}/}"
  local cert_count

  if [[ "$(stat -c %u "${cert_dir}")" != "1000" ]]; then
    chown 1000:1000 "${cert_dir}"
  fi

  if certificate_matches_dns_identity "${fullchain_file}" "${key_file}" "${AUTHENTIK_FQDN}"; then
    echo "Reusing existing Authentik certificate for ${AUTHENTIK_FQDN}."
    normalize_authentik_certificate_permissions "${cert_dir}"
    return
  fi

  if [[ -f "${fullchain_file}" || -f "${key_file}" ]]; then
    echo "Existing Authentik certificate is not valid for ${AUTHENTIK_FQDN}; issuing replacement."
  else
    echo "Issuing Authentik certificate for ${AUTHENTIK_FQDN}."
  fi
  rm -f "${fullchain_file}" "${key_file}"

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca certificate "${AUTHENTIK_FQDN}" "${cert_dir_in_container}/fullchain.pem" "${cert_dir_in_container}/privkey.pem" \
      --san "${AUTHENTIK_FQDN}" \
      --not-after "${SERVICE_CERT_DURATION}" \
      --issuer "${CA_PROVISIONER_NAME}" \
      --provisioner-password-file "${password_file_in_container}" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to issue an Authentik certificate from step-ca."

  cert_count="$(grep -c "BEGIN CERTIFICATE" "${fullchain_file}")"
  [[ "${cert_count}" -eq 2 ]] || \
    fail "Authentik fullchain.pem must contain exactly 2 certificates (leaf and intermediate)."

  normalize_authentik_certificate_permissions "${cert_dir}"
}

# Until the brand web certificate is set, Authentik serves its own
# self-signed certificate on the HTTPS listener. Bootstrap-phase requests
# therefore skip CA verification; verify_authentik_certificate confirms the
# step-ca certificate afterwards.
wait_for_authentik_https() {
  local attempt http_code
  local ready_url="https://${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}/-/health/ready/"

  echo "Waiting for Authentik HTTPS endpoint to become ready at ${ready_url}."

  for attempt in $(seq 1 90); do
    http_code="$(curl --silent --show-error \
      --output /dev/null \
      --write-out '%{http_code}' \
      --insecure \
      --resolve "${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}:127.0.0.1" \
      "${ready_url}" || true)"

    case "${http_code}" in
      200|204)
        echo "Authentik is ready."
        return 0
        ;;
    esac

    sleep 2
  done

  fail "Authentik failed readiness check at ${ready_url}. Last observed HTTP status: ${http_code}. Check logs with: docker compose logs"
}

authentik_json_string_field() {
  local field="$1"
  sed -n "s/.*\"${field}\":\"\\([^\"]*\\)\".*/\\1/p" | head -n1
}

authentik_api_get() {
  local path="$1"
  curl --silent --show-error --fail \
    --insecure \
    --resolve "${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}:127.0.0.1" \
    -H "Authorization: Bearer ${AUTHENTIK_API_TOKEN}" \
    "https://${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}${path}"
}

# Certificate discovery and the initial blueprint apply both run during worker
# startup without a guaranteed order. Re-applying the blueprint after the
# keypair exists makes the provider signing_key !Find lookup deterministic.
reapply_authentik_blueprint() {
  local blueprint_pk http_code

  blueprint_pk="$(authentik_api_get "/api/v3/managed/blueprints/?search=provider-box-vcf-bootstrap" | \
    authentik_json_string_field pk)"
  [[ -n "${blueprint_pk}" ]] || \
    fail "Authentik did not discover the provider-box-vcf-bootstrap blueprint. Check worker logs with: docker compose logs worker"

  http_code="$(curl --silent --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --insecure \
    --resolve "${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}:127.0.0.1" \
    -X POST \
    -H "Authorization: Bearer ${AUTHENTIK_API_TOKEN}" \
    "https://${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}/api/v3/managed/blueprints/${blueprint_pk}/apply/" || true)"

  [[ "${http_code}" == "200" ]] || \
    fail "Failed to re-apply the Authentik bootstrap blueprint. HTTP status: ${http_code}"
}

configure_authentik_brand_certificate() {
  local attempt keypair_pk brand_uuid http_code

  echo "Configuring the Authentik brand certificate for ${AUTHENTIK_FQDN}."

  for attempt in $(seq 1 45); do
    keypair_pk="$(authentik_api_get "/api/v3/crypto/certificatekeypairs/?name=${AUTHENTIK_FQDN}" | \
      authentik_json_string_field pk || true)"
    [[ -n "${keypair_pk}" ]] && break
    sleep 2
  done
  [[ -n "${keypair_pk}" ]] || \
    fail "Authentik did not discover the certificate keypair ${AUTHENTIK_FQDN} under /certs. Check worker logs with: docker compose logs worker"

  reapply_authentik_blueprint

  brand_uuid="$(authentik_api_get "/api/v3/core/brands/" | \
    authentik_json_string_field brand_uuid)"
  [[ -n "${brand_uuid}" ]] || fail "Failed to determine the default Authentik brand."

  http_code="$(curl --silent --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --insecure \
    --resolve "${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}:127.0.0.1" \
    -X PATCH \
    -H "Authorization: Bearer ${AUTHENTIK_API_TOKEN}" \
    -H "Content-Type: application/json" \
    --data "{\"web_certificate\": \"${keypair_pk}\"}" \
    "https://${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}/api/v3/core/brands/${brand_uuid}/" || true)"

  [[ "${http_code}" == "200" ]] || \
    fail "Failed to set the Authentik brand web certificate. HTTP status: ${http_code}"

  echo "Authentik brand web certificate set to the step-ca keypair ${AUTHENTIK_FQDN}."
}

verify_authentik_certificate() {
  local attempt
  local ready_url="https://${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}/-/health/ready/"

  # The embedded router refreshes brand TLS material on a periodic interval;
  # picking up the new web certificate can take a couple of minutes.
  echo "Verifying that Authentik serves the step-ca-issued certificate (may take a few minutes)."

  for attempt in $(seq 1 100); do
    if curl --silent --show-error --fail \
      --output /dev/null \
      --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
      --resolve "${AUTHENTIK_FQDN}:${AUTHENTIK_PORT}:127.0.0.1" \
      "${ready_url}"; then
      echo "Authentik is serving the step-ca-issued certificate for ${AUTHENTIK_FQDN}."
      return 0
    fi
    sleep 3
  done

  fail "Authentik is not serving the step-ca-issued certificate at ${ready_url}. Check the brand web certificate in the Authentik admin UI."
}

do_authentik() {
  require_authentik_vars
  require_authentik_ca_vars
  common_pkgs
  docker_pkgs
  require_ca_ready_for_authentik
  prepare_authentik_directories
  render_authentik_blueprint
  issue_authentik_certificates
  render_template "${TEMPLATE_DIR}/docker-compose.authentik.yml.tpl" "${WORKDIR}/authentik/docker-compose.yml"
  (
    cd "${WORKDIR}/authentik"
    docker compose down || true
    docker compose up -d
  )
  ufw allow "${AUTHENTIK_PORT}/tcp" || true
  wait_for_authentik_https
  configure_authentik_brand_certificate
  verify_authentik_certificate
}

remove_authentik() {
  local runtime_dir="${WORKDIR}/authentik"
  local compose_file="${runtime_dir}/docker-compose.yml"

  require_authentik_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    (
      cd "${runtime_dir}"
      docker compose down || true
    )
  fi

  rm -rf "${runtime_dir}"
  echo "Removed Authentik containers and runtime files. Persistent data in ${AUTHENTIK_DIR} was preserved."
}
