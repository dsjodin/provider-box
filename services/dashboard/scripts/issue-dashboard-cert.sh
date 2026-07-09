#!/usr/bin/env bash
# Issue the dashboard's TLS leaf certificate from step-ca, mirroring the
# technitium module's cert-issuance docker run. Writes dashboard.crt (leaf +
# chain, as `step ca certificate` bundles it) and dashboard.key into
# DASHBOARD_CERT_DIR, owned by uid 1000 so the compose service can read them.
#
# Usage: services/dashboard/scripts/issue-dashboard-cert.sh [ENV_FILE]
# ENV_FILE defaults to config/provider-box.env at the repo root.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
ENV_FILE="${1:-${REPO_ROOT}/config/provider-box.env}"

fail() {
  echo "Error: $*" >&2
  exit 1
}

[[ -f "${ENV_FILE}" ]] || fail "Missing env file ${ENV_FILE}"
# shellcheck disable=SC1090
source "${ENV_FILE}"

for var in CA_FQDN CA_PORT CA_IMAGE CA_DATA_DIR CA_PROVISIONER_NAME \
           CA_PASSWORD_FILE SERVICE_CERT_DURATION DASHBOARD_FQDN DASHBOARD_CERT_DIR; do
  [[ -n "${!var:-}" ]] || fail "${var} is not set in ${ENV_FILE}"
done

command -v docker >/dev/null || fail "docker is required"
[[ -f "${CA_DATA_DIR}/certs/root_ca.crt" ]] || \
  fail "Missing step-ca root certificate at ${CA_DATA_DIR}/certs/root_ca.crt. Run --ca first."

# CA_PASSWORD_FILE lives under CA_DATA_DIR; map it into the container's /home/step.
case "${CA_PASSWORD_FILE}" in
  "${CA_DATA_DIR}/"*) ;;
  *) fail "CA_PASSWORD_FILE must live under CA_DATA_DIR (${CA_DATA_DIR})" ;;
esac
password_file_in_container="/home/step/${CA_PASSWORD_FILE#"${CA_DATA_DIR}"/}"
cert_dir_in_container="/certs"

install -d -m 0755 "${DASHBOARD_CERT_DIR}"
# The step-cli container writes as uid 1000; the cert dir must be owned by 1000
# BEFORE the run, or issuance fails "permission denied" on a first run where the
# dir is freshly created as root/dsadmin. Post-run chown alone is too late.
chown 1000:1000 "${DASHBOARD_CERT_DIR}"
rm -f "${DASHBOARD_CERT_DIR}/dashboard.crt" "${DASHBOARD_CERT_DIR}/dashboard.key"

echo "Issuing dashboard certificate for ${DASHBOARD_FQDN}."
docker run --rm --network host \
  --add-host "${CA_FQDN}:127.0.0.1" \
  -v "${CA_DATA_DIR}:/home/step" \
  -v "${DASHBOARD_CERT_DIR}:${cert_dir_in_container}" \
  "${CA_IMAGE}" \
  step ca certificate "${DASHBOARD_FQDN}" \
    "${cert_dir_in_container}/dashboard.crt" \
    "${cert_dir_in_container}/dashboard.key" \
    --san "${DASHBOARD_FQDN}" \
    --not-after "${SERVICE_CERT_DURATION}" \
    --issuer "${CA_PROVISIONER_NAME}" \
    --provisioner-password-file "${password_file_in_container}" \
    --ca-url "https://${CA_FQDN}:${CA_PORT}" \
    --root /home/step/certs/root_ca.crt || \
    fail "Failed to issue a dashboard certificate from step-ca."

chown -R 1000:1000 "${DASHBOARD_CERT_DIR}"
chmod 0600 "${DASHBOARD_CERT_DIR}/dashboard.key"
echo "Wrote ${DASHBOARD_CERT_DIR}/dashboard.crt and dashboard.key (owner uid 1000)."
