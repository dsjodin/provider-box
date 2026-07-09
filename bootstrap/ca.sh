#!/usr/bin/env bash

require_ca_vars() {
  local var
  for var in WORKDIR CA_FQDN CA_PORT CA_DATA_DIR CA_NAME CA_PROVISIONER_NAME SERVICE_CERT_DURATION CA_ENABLE_ACME CA_IMAGE; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_fqdn "${CA_FQDN}"
  validate_var_port "${CA_PORT}"
  validate_var_path "${CA_DATA_DIR}"
  validate_service_cert_duration "${SERVICE_CERT_DURATION}"
  [[ "${CA_IMAGE}" == *:* ]] || fail "CA_IMAGE must include an explicit image tag"
  [[ "${CA_IMAGE}" != *:latest ]] || fail "CA_IMAGE must not use the latest tag"
  resolve_ca_password_file
  validate_var_path "${CA_PASSWORD_FILE}"
  [[ "${CA_ENABLE_ACME}" == "true" || "${CA_ENABLE_ACME}" == "false" ]] || \
    fail "CA_ENABLE_ACME must be either true or false"
  [[ "${CA_PASSWORD_FILE}" == "${CA_DATA_DIR}"/* ]] || \
    fail "CA_PASSWORD_FILE must be located under CA_DATA_DIR so it is mounted into the container"
  if [[ -n "${CA_PASSWORD:-}" ]]; then
    validate_ca_password_value "${CA_PASSWORD}"
  fi
}

require_ca_remove_vars() {
  local var
  for var in WORKDIR CA_DATA_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${WORKDIR}"
  validate_var_path "${CA_DATA_DIR}"
}

# Guard against a prior `docker compose` run with an empty CA_DATA_DIR (an
# unset variable) having created ${CA_DATA_DIR}/certs/root_ca.crt as a
# DIRECTORY: Docker auto-creates a missing bind-mount source as a directory, so
# a blank path turned this trust-root file into a directory, broke step-ca init
# ("file is a directory") and destroyed the running CA. Refuse to proceed on a
# corrupted root so the operator notices instead of looping on a broken init.
require_ca_root_not_corrupted() {
  local root="${CA_DATA_DIR}/certs/root_ca.crt"
  if [[ -d "${root}" ]]; then
    fail "step-ca root certificate ${root} is a DIRECTORY, not a file. This is almost always a prior 'docker compose' run with an empty CA_DATA_DIR (an unset variable) auto-creating the missing bind-mount source as a directory - it can destroy the CA. Remove it (rm -rf '${root}') and restore or re-initialize the CA before re-running --ca."
  fi
  if [[ -e "${root}" && ! -f "${root}" ]]; then
    fail "step-ca root certificate ${root} exists but is not a regular file (likely a bad bind mount from an empty CA_DATA_DIR). Investigate and restore a valid root_ca.crt before re-running --ca."
  fi
}

normalize_ca_password_files() {
  local file

  for file in \
    "${CA_PASSWORD_FILE}" \
    "${CA_DATA_DIR}/secrets/password"
  do
    [[ -f "${file}" ]] || continue
    chown 1000:1000 "${file}"
    chmod 0600 "${file}"
  done
}

# step ca init runs asynchronously at first container start; key generation
# takes seconds. Wait for the configuration to appear, then for the CA to
# answer on its health endpoint, before doing anything that depends on it.
wait_for_ca_init() {
  local ca_config="${CA_DATA_DIR}/config/ca.json"
  local attempt

  for attempt in $(seq 1 30); do
    [[ -f "${ca_config}" ]] && break
    sleep 2
  done
  [[ -f "${ca_config}" ]] || \
    fail "step-ca did not initialize. Check: docker logs step-ca-step-ca-1"

  for attempt in $(seq 1 30); do
    if curl --silent --show-error --fail \
      --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
      --resolve "${CA_FQDN}:${CA_PORT}:127.0.0.1" \
      "https://${CA_FQDN}:${CA_PORT}/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  fail "step-ca did not initialize. Check: docker logs step-ca-step-ca-1"
}

configure_ca_service_cert_duration() {
  local ca_config="${CA_DATA_DIR}/config/ca.json"

  echo "Configuring step-ca service certificate duration: ${SERVICE_CERT_DURATION}"
  docker run --rm \
    --user 1000:1000 \
    -v "${CA_DATA_DIR}:/home/step" \
    "${CA_IMAGE}" \
    step ca provisioner update "${CA_PROVISIONER_NAME}" \
      --x509-default-dur="${SERVICE_CERT_DURATION}" \
      --x509-max-dur="${SERVICE_CERT_DURATION}" \
      --ca-config /home/step/config/ca.json || \
    fail "Failed to configure step-ca provisioner certificate duration."
  chown 1000:1000 "${ca_config}"
  chmod 0600 "${ca_config}"
}

do_ca() {
  local password_dir password_value

  require_ca_vars
  require_ca_root_not_corrupted
  common_pkgs
  docker_pkgs
  password_dir="$(dirname "${CA_PASSWORD_FILE}")"
  install -d -m 0755 "${WORKDIR}/step-ca" "${CA_DATA_DIR}"
  install -d -m 0700 "${password_dir}"

  if [[ -f "${CA_PASSWORD_FILE}" ]]; then
    chmod 600 "${CA_PASSWORD_FILE}"
    echo "Using existing CA password file: ${CA_PASSWORD_FILE}"
  else
    if [[ -n "${CA_PASSWORD:-}" ]]; then
      password_value="${CA_PASSWORD}"
      echo "Materializing CA_PASSWORD to managed file: ${CA_PASSWORD_FILE}"
    else
      echo "CA password input not provided. Generating one..."
      require_command openssl
      password_value="$(openssl rand -base64 32)"
      echo "Generated CA password at: ${CA_PASSWORD_FILE}"
    fi

    install -m 0600 /dev/null "${CA_PASSWORD_FILE}"
    printf '%s\n' "${password_value}" > "${CA_PASSWORD_FILE}"
    chmod 600 "${CA_PASSWORD_FILE}"
  fi
  # The step-ca image runs as uid 1000; root-owned data or secrets dirs make
  # the entrypoint unable to read the password file and init never runs.
  chown -R 1000:1000 "${CA_DATA_DIR}"
  chmod 0700 "${password_dir}"
  normalize_ca_password_files

  CA_PASSWORD_FILE_IN_CONTAINER="/home/step/${CA_PASSWORD_FILE#${CA_DATA_DIR}/}"
  if [[ "${CA_ENABLE_ACME}" == "true" ]]; then
    CA_ACME_ENV_BLOCK='      DOCKER_STEPCA_INIT_ACME: "true"'
  else
    CA_ACME_ENV_BLOCK=""
  fi
  export CA_PASSWORD_FILE_IN_CONTAINER CA_ACME_ENV_BLOCK

  render_template "${TEMPLATE_DIR}/docker-compose.step-ca.yml.tpl" "${WORKDIR}/step-ca/docker-compose.yml"

  (
    cd "${WORKDIR}/step-ca"
    docker compose down || true
    docker compose up -d
  )
  normalize_ca_password_files
  wait_for_ca_init
  configure_ca_service_cert_duration
  (
    cd "${WORKDIR}/step-ca"
    docker compose restart step-ca
  )
  ufw allow "${CA_PORT}/tcp" || true
}

remove_ca() {
  local runtime_dir="${WORKDIR}/step-ca"
  local compose_file="${runtime_dir}/docker-compose.yml"

  require_ca_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    (
      cd "${runtime_dir}"
      docker compose down || true
    )
  fi

  rm -rf "${runtime_dir}"
  echo "Removed step-ca containers and runtime files. Persistent data in ${CA_DATA_DIR} was preserved."
}
