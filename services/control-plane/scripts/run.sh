#!/usr/bin/env bash
# One correct way to start the standalone dashboard. Runs the documented compose
# command with the shared env file and resolves CONTROL_PLANE_DOCKER_GID from the
# host docker group so the read-only socket mount is usable by uid 1000.
#
# Usage: services/control-plane/scripts/run.sh [ENV_FILE] [-- extra compose args]
# ENV_FILE defaults to config/provider-box.env at the repo root.
# Example: services/control-plane/scripts/run.sh            # up -d --build
#          services/control-plane/scripts/run.sh -- down    # stop it
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVICE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${SERVICE_DIR}/../.." && pwd)"

fail() {
  echo "Error: $*" >&2
  exit 1
}

ENV_FILE="${REPO_ROOT}/config/provider-box.env"
if [[ "${1:-}" != "" && "${1:-}" != "--" ]]; then
  ENV_FILE="$1"
  shift
fi
[[ "${1:-}" == "--" ]] && shift

[[ -f "${ENV_FILE}" ]] || fail "Missing env file ${ENV_FILE}"
command -v docker >/dev/null || fail "docker is required"
docker compose version >/dev/null 2>&1 || fail "docker compose v2 is required"

# Validate the variables the compose file interpolates into bind-mount SOURCES
# before calling docker compose. An empty value makes Docker auto-create the
# missing source as a directory, which can corrupt data (this is how an empty
# CA_DATA_DIR once turned step-ca's root_ca.crt into a directory). Compose's
# ${VAR:?...} guards are the second layer; this fails fast naming the variable.
set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a
for var in CA_DATA_DIR CONTROL_PLANE_CERT_DIR CONTROL_PLANE_SECRETS_DIR; do
  [[ -n "${!var:-}" ]] || fail "${var} is empty or unset in ${ENV_FILE}; refusing to run docker compose - an empty bind-mount source would create a blank host directory and can corrupt data."
  [[ "${!var}" == /* ]] || fail "${var} must be an absolute path (got '${!var}')."
done

# Resolve the host docker gid; a shell export overrides the env-file default so
# the socket mount works without hand-editing config.
if docker_gid="$(getent group docker | cut -d: -f3)" && [[ -n "${docker_gid}" ]]; then
  export CONTROL_PLANE_DOCKER_GID="${docker_gid}"
else
  echo "Warning: no 'docker' group found; leaving CONTROL_PLANE_DOCKER_GID from ${ENV_FILE}." >&2
fi

# Default action is up -d --build; pass anything after -- to override.
if [[ $# -eq 0 ]]; then
  set -- up -d --build
fi

cd "${SERVICE_DIR}"
exec docker compose --env-file "${ENV_FILE}" "$@"
