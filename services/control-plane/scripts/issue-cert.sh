#!/usr/bin/env bash
# Issue the control plane's TLS certificate from step-ca, mirroring the technitium
# module's cert-issuance docker run. Writes control-plane.crt as a FULL chain (leaf
# + step-ca intermediate) and control-plane.key into CONTROL_PLANE_CERT_DIR, owned by
# uid 1000 so the compose service can read them. The full chain is enforced
# explicitly below so the served cert validates against the step-ca root.
#
# Usage: services/control-plane/scripts/issue-cert.sh [ENV_FILE]
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
           CA_PASSWORD_FILE SERVICE_CERT_DURATION CONTROL_PLANE_FQDN CONTROL_PLANE_CERT_DIR; do
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

install -d -m 0755 "${CONTROL_PLANE_CERT_DIR}"
# The step-cli container writes as uid 1000; the cert dir must be owned by 1000
# BEFORE the run, or issuance fails "permission denied" on a first run where the
# dir is freshly created as root/dsadmin. Post-run chown alone is too late.
chown 1000:1000 "${CONTROL_PLANE_CERT_DIR}"
rm -f "${CONTROL_PLANE_CERT_DIR}/control-plane.crt" "${CONTROL_PLANE_CERT_DIR}/control-plane.key"

echo "Issuing control-plane certificate for ${CONTROL_PLANE_FQDN}."
docker run --rm --network host \
  --add-host "${CA_FQDN}:127.0.0.1" \
  -v "${CA_DATA_DIR}:/home/step" \
  -v "${CONTROL_PLANE_CERT_DIR}:${cert_dir_in_container}" \
  "${CA_IMAGE}" \
  step ca certificate "${CONTROL_PLANE_FQDN}" \
    "${cert_dir_in_container}/control-plane.crt" \
    "${cert_dir_in_container}/control-plane.key" \
    --san "${CONTROL_PLANE_FQDN}" \
    --not-after "${SERVICE_CERT_DURATION}" \
    --issuer "${CA_PROVISIONER_NAME}" \
    --provisioner-password-file "${password_file_in_container}" \
    --ca-url "https://${CA_FQDN}:${CA_PORT}" \
    --root /home/step/certs/root_ca.crt || \
    fail "Failed to issue a control-plane certificate from step-ca."

# Guarantee a FULL chain (leaf + intermediate). `step ca certificate` bundles
# the intermediate by default, but the served cert must validate against the
# step-ca root on its own, so make it explicit and defensive: if only the leaf
# is present, append the step-ca intermediate. A leaf-only cert bit us during a
# CA rebuild.
cert_count="$(grep -c 'BEGIN CERTIFICATE' "${CONTROL_PLANE_CERT_DIR}/control-plane.crt" || true)"
if [[ "${cert_count}" -lt 2 ]]; then
  intermediate="${CA_DATA_DIR}/certs/intermediate_ca.crt"
  [[ -f "${intermediate}" ]] || \
    fail "control-plane.crt has no intermediate and ${intermediate} is missing; cannot build a full chain. Run --ca first."
  cat "${intermediate}" >> "${CONTROL_PLANE_CERT_DIR}/control-plane.crt"
  echo "Appended the step-ca intermediate to control-plane.crt (leaf + intermediate)."
fi

chown -R 1000:1000 "${CONTROL_PLANE_CERT_DIR}"
chmod 0600 "${CONTROL_PLANE_CERT_DIR}/control-plane.key"
echo "Wrote ${CONTROL_PLANE_CERT_DIR}/control-plane.crt and control-plane.key (owner uid 1000)."
