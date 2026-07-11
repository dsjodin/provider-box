#!/usr/bin/env bash

# The Provider Box dashboard is a standalone, read-only "current state" view of
# the other services (services/control-plane). This module wires the existing
# service into bootstrap; it does not rewrite it. Cert issuance and startup
# reuse the service's own scripts (scripts/issue-cert.sh and
# scripts/run.sh) so the manual/standalone path and the module share one code
# path. The dashboard reads its upstreams read-only and its panels degrade to
# "not configured" when an optional scoped token is absent, so it never fails
# on missing tokens.

require_dashboard_vars() {
  local var
  for var in REPO_ROOT CONTROL_PLANE_FQDN CONTROL_PLANE_ADDR CONTROL_PLANE_IMAGE \
             CONTROL_PLANE_CERT_DIR CONTROL_PLANE_SECRETS_DIR CONTROL_PLANE_CONTAINER_FILTERS \
             CONTROL_PLANE_LOG_TAIL CONTROL_PLANE_UPSTREAM_TIMEOUT CONTROL_PLANE_CERT_WARN_DAYS \
             CA_DATA_DIR CA_POSTGRES_DB CA_POSTGRES_PORT CA_POSTGRES_RO_USER \
             CA_POSTGRES_RO_PASSWORD; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_fqdn "${CONTROL_PLANE_FQDN}"
  validate_var_path "${CONTROL_PLANE_CERT_DIR}"
  validate_var_path "${CONTROL_PLANE_SECRETS_DIR}"
  validate_var_path "${CA_DATA_DIR}"
  validate_var_port "${CA_POSTGRES_PORT}"
  validate_var_not_placeholder "${CA_POSTGRES_RO_PASSWORD}"
  [[ "${CONTROL_PLANE_IMAGE}" == *:* ]] || fail "CONTROL_PLANE_IMAGE must include an explicit image tag"
  [[ "${CONTROL_PLANE_IMAGE}" != *:latest ]] || fail "CONTROL_PLANE_IMAGE must not use the latest tag"

  # CONTROL_PLANE_ADDR is a listen address like ":8445"; the verify step and ufw
  # rule need the bare port.
  CONTROL_PLANE_PORT="${CONTROL_PLANE_ADDR##*:}"
  [[ -n "${CONTROL_PLANE_PORT}" && "${CONTROL_PLANE_PORT}" != "${CONTROL_PLANE_ADDR}" ]] || \
    fail "CONTROL_PLANE_ADDR must include a port (e.g. :8445); got '${CONTROL_PLANE_ADDR}'"
  validate_var_port "${CONTROL_PLANE_PORT}"
  export CONTROL_PLANE_PORT
}

require_dashboard_ca_vars() {
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

require_dashboard_remove_vars() {
  local var
  for var in REPO_ROOT CONTROL_PLANE_CERT_DIR CONTROL_PLANE_SECRETS_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${CONTROL_PLANE_CERT_DIR}"
  validate_var_path "${CONTROL_PLANE_SECRETS_DIR}"
}

# The dashboard issues its cert from step-ca; require the same CA material the
# fullchain build needs (root + intermediate) and confirm the CA answers.
require_ca_ready_for_dashboard() {
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

# Directories must exist with uid-1000 ownership BEFORE the cert is issued (the
# step-cli container writes as uid 1000) and BEFORE compose mounts the secrets
# dir read-only.
bootstrap_dashboard_layout() {
  install -d -m 0755 "${CONTROL_PLANE_CERT_DIR}"
  install -d -m 0700 "${CONTROL_PLANE_SECRETS_DIR}"
  chown 1000:1000 "${CONTROL_PLANE_CERT_DIR}" "${CONTROL_PLANE_SECRETS_DIR}"
}

# Materialize the step-ca read-only postgres password the cert panel reads. The
# value is the RO role password created by --ca; the dashboard only ever gets
# SELECT on the cert tables through it. Written 0600/uid-1000 so the read-only
# secrets bind mount is readable by the container user.
provision_dashboard_stepca_ro_password() {
  local pw_file="${CONTROL_PLANE_SECRETS_DIR}/stepca-ro.pgpassword"
  install -m 0600 /dev/null "${pw_file}"
  printf '%s' "${CA_POSTGRES_RO_PASSWORD}" > "${pw_file}"
  chown 1000:1000 "${pw_file}"
}

# Reuse the service's own issuance script (fullchain leaf + intermediate,
# --add-host ca pin, uid-1000 ownership) rather than duplicating the docker run.
issue_dashboard_certificate() {
  "${REPO_ROOT}/services/control-plane/scripts/issue-cert.sh" "${ENV_FILE}" || \
    fail "Failed to issue the dashboard certificate from step-ca."
}

# Reuse the service's run.sh: it resolves CONTROL_PLANE_DOCKER_GID from the host
# docker group, validates the bind-mount vars, and runs the standalone compose
# (--env-file provider-box.env, up -d --build).
start_dashboard_stack() {
  "${REPO_ROOT}/services/control-plane/scripts/run.sh" "${ENV_FILE}" || \
    fail "Failed to start the dashboard compose stack."
}

verify_dashboard_https() {
  local attempt
  local url="https://${CONTROL_PLANE_FQDN}:${CONTROL_PLANE_PORT}/healthz"

  echo "Verifying dashboard HTTPS at ${url} with the step-ca chain."
  for attempt in $(seq 1 30); do
    if curl --silent --show-error --fail \
      --output /dev/null \
      --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
      --resolve "${CONTROL_PLANE_FQDN}:${CONTROL_PLANE_PORT}:127.0.0.1" \
      "${url}" 2>/dev/null; then
      echo "Dashboard HTTPS is serving the step-ca-issued certificate."
      return 0
    fi
    sleep 2
  done
  fail "Dashboard HTTPS did not become ready at ${url} with the step-ca certificate. Check 'docker compose -f ${REPO_ROOT}/services/control-plane/docker-compose.yml logs'."
}

do_dashboard() {
  require_dashboard_vars
  require_dashboard_ca_vars
  common_pkgs
  docker_pkgs
  require_ca_ready_for_dashboard
  bootstrap_dashboard_layout
  provision_dashboard_stepca_ro_password
  issue_dashboard_certificate
  start_dashboard_stack
  ufw allow "${CONTROL_PLANE_PORT}/tcp" || true
  verify_dashboard_https
  echo "Dashboard is ready: https://${CONTROL_PLANE_FQDN}:${CONTROL_PLANE_PORT}/"
  echo "Read-only tokens are auto-provisioned by --netbox and --technitium (an"
  echo "operator-placed SOPS/age token wins); panels degrade to 'not configured'"
  echo "until the producing module has run:"
  echo "  ${CONTROL_PLANE_SECRETS_DIR}/netbox-readonly.token  (NetBox IPAM panel, from --netbox)"
  echo "  ${CONTROL_PLANE_SECRETS_DIR}/technitium.token        (Technitium DNS panel, from --technitium)"
  echo "${CONTROL_PLANE_FQDN} resolves by name once --dns-sync publishes it via Technitium."
}

remove_dashboard() {
  local compose_file="${REPO_ROOT}/services/control-plane/docker-compose.yml"

  require_dashboard_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    "${REPO_ROOT}/services/control-plane/scripts/run.sh" "${ENV_FILE}" -- down || true
  fi

  echo "Removed the dashboard container. Certificates in ${CONTROL_PLANE_CERT_DIR} and tokens in ${CONTROL_PLANE_SECRETS_DIR} were preserved."
}
