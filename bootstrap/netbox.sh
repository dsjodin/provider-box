#!/usr/bin/env bash

require_netbox_ca_vars() {
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

validate_netbox_secret_key() {
  [[ "${NETBOX_SECRET_KEY}" != CHANGE_ME* ]] || \
    fail "Replace NETBOX_SECRET_KEY before continuing"
  (( ${#NETBOX_SECRET_KEY} >= 50 )) || \
    fail "NETBOX_SECRET_KEY must be at least 50 characters long"
}

validate_netbox_image() {
  [[ "${NETBOX_IMAGE}" == *:* ]] || fail "NETBOX_IMAGE must include an explicit image tag"
  [[ "${NETBOX_IMAGE}" != *:latest ]] || fail "NETBOX_IMAGE must not use the latest tag"
  [[ "${NETBOX_POSTGRES_IMAGE}" == *:* ]] || fail "NETBOX_POSTGRES_IMAGE must include an explicit image tag"
  [[ "${NETBOX_POSTGRES_IMAGE}" != *:latest ]] || fail "NETBOX_POSTGRES_IMAGE must not use the latest tag"
  [[ "${NETBOX_REDIS_IMAGE}" == *:* ]] || fail "NETBOX_REDIS_IMAGE must include an explicit image tag"
  [[ "${NETBOX_REDIS_IMAGE}" != *:latest ]] || fail "NETBOX_REDIS_IMAGE must not use the latest tag"
  [[ "${NETBOX_NGINX_IMAGE}" == *:* ]] || fail "NETBOX_NGINX_IMAGE must include an explicit image tag"
  [[ "${NETBOX_NGINX_IMAGE}" != *:latest ]] || fail "NETBOX_NGINX_IMAGE must not use the latest tag"
}

validate_netbox_email() {
  [[ "${NETBOX_SUPERUSER_EMAIL}" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || \
    fail "Invalid NETBOX_SUPERUSER_EMAIL value: ${NETBOX_SUPERUSER_EMAIL}"
}

build_netbox_dns_seed_block() {
  local line fqdn address_value
  NETBOX_DNS_RECORDS=""

  if [[ ! -f "${RECORDS_FILE}" ]]; then
    export NETBOX_DNS_RECORDS
    return
  fi

  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    [[ "$line" = \#* ]] && continue
    parse_dns_record_line "$line"
    fqdn="${DNS_RECORD_FQDN}"
    address_value="${DNS_RECORD_TARGET}"
    NETBOX_DNS_RECORDS="${NETBOX_DNS_RECORDS}${fqdn}|${address_value}|unbound.records
"
  done < "${RECORDS_FILE}"

  export NETBOX_DNS_RECORDS
}

build_netbox_service_seed_block() {
  NETBOX_PROVIDER_SERVICES="dns-tcp|${DNS_FQDN}|tcp|53
dns-udp|${DNS_FQDN}|udp|53
ntp|${DNS_FQDN}|udp|123
syslog-tcp|${SYSLOG_FQDN}|tcp|${SYSLOG_PORT}
syslog-udp|${SYSLOG_FQDN}|udp|${SYSLOG_PORT}
step-ca|${CA_FQDN}|tcp|${CA_PORT}
depot-http|${DEPOT_FQDN}|tcp|${DEPOT_HTTP_PORT}
depot-https|${DEPOT_FQDN}|tcp|${DEPOT_HTTPS_PORT}
keycloak|${KEYCLOAK_FQDN}|tcp|${KEYCLOAK_PORT:-8443}
netbox|${NETBOX_FQDN}|tcp|${NETBOX_PORT}
s3|${S3_FQDN}|tcp|${S3_PORT}
sftp|${SFTP_FQDN}|tcp|${SFTP_PORT}
sftp-admin|${SFTP_FQDN}|tcp|${SFTP_ADMIN_PORT}
"

  export NETBOX_PROVIDER_SERVICES
}

build_netbox_provider_box_host_description() {
  NETBOX_PROVIDER_BOX_HOST_DESCRIPTION="Provider Box services: ${DNS_FQDN}, ${CA_FQDN}, ${DEPOT_FQDN}, ${KEYCLOAK_FQDN}, ${NETBOX_FQDN}, ${S3_FQDN}, ${SFTP_FQDN}, ${SYSLOG_FQDN}"
  export NETBOX_PROVIDER_BOX_HOST_DESCRIPTION
}

require_netbox_vars() {
  local var
  for var in PROVIDER_BOX_FQDN DNS_FQDN CA_FQDN CA_PORT DEPOT_FQDN DEPOT_HTTP_PORT DEPOT_HTTPS_PORT KEYCLOAK_FQDN KEYCLOAK_PORT NETBOX_FQDN NETBOX_PORT S3_FQDN S3_PORT SFTP_FQDN SFTP_PORT SFTP_ADMIN_PORT SYSLOG_FQDN SYSLOG_PORT NETBOX_DIR NETBOX_MEDIA_DIR NETBOX_POSTGRES_DATA_DIR NETBOX_REDIS_DATA_DIR NETBOX_IMAGE NETBOX_POSTGRES_IMAGE NETBOX_REDIS_IMAGE NETBOX_NGINX_IMAGE NETBOX_POSTGRES_DB NETBOX_POSTGRES_USER NETBOX_POSTGRES_PASSWORD NETBOX_REDIS_PASSWORD NETBOX_SECRET_KEY NETBOX_ALLOWED_HOSTS NETBOX_CSRF_TRUSTED_ORIGINS NETBOX_SUPERUSER_NAME NETBOX_SUPERUSER_EMAIL NETBOX_SUPERUSER_PASSWORD; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_fqdn "${PROVIDER_BOX_FQDN}"
  validate_var_fqdn "${DNS_FQDN}"
  validate_var_fqdn "${CA_FQDN}"
  validate_var_port "${CA_PORT}"
  validate_var_fqdn "${DEPOT_FQDN}"
  validate_var_port "${DEPOT_HTTP_PORT}"
  validate_var_port "${DEPOT_HTTPS_PORT}"
  validate_var_fqdn "${KEYCLOAK_FQDN}"
  validate_var_port "${KEYCLOAK_PORT}"
  validate_var_fqdn "${NETBOX_FQDN}"
  validate_var_port "${NETBOX_PORT}"
  validate_var_fqdn "${S3_FQDN}"
  validate_var_port "${S3_PORT}"
  validate_var_fqdn "${SFTP_FQDN}"
  validate_var_port "${SFTP_PORT}"
  validate_var_port "${SFTP_ADMIN_PORT}"
  validate_var_fqdn "${SYSLOG_FQDN}"
  validate_var_port "${SYSLOG_PORT}"
  validate_var_path "${NETBOX_DIR}"
  validate_var_path "${NETBOX_MEDIA_DIR}"
  validate_var_path "${NETBOX_POSTGRES_DATA_DIR}"
  validate_var_path "${NETBOX_REDIS_DATA_DIR}"
  validate_var_not_placeholder "${NETBOX_POSTGRES_PASSWORD}"
  validate_var_not_placeholder "${NETBOX_REDIS_PASSWORD}"
  validate_var_not_placeholder "${NETBOX_SUPERUSER_PASSWORD}"
  validate_netbox_secret_key
  validate_netbox_image
  validate_netbox_email
  [[ "${NETBOX_ALLOWED_HOSTS}" == *"${NETBOX_FQDN}"* ]] || \
    fail "NETBOX_ALLOWED_HOSTS must include ${NETBOX_FQDN}"
}

require_netbox_remove_vars() {
  local var
  for var in NETBOX_DIR NETBOX_MEDIA_DIR NETBOX_POSTGRES_DATA_DIR NETBOX_REDIS_DATA_DIR; do
    [[ -n "${!var:-}" ]] || fail "Missing required variable: $var"
  done

  validate_var_path "${NETBOX_DIR}"
  validate_var_path "${NETBOX_MEDIA_DIR}"
  validate_var_path "${NETBOX_POSTGRES_DATA_DIR}"
  validate_var_path "${NETBOX_REDIS_DATA_DIR}"
}

require_ca_ready_for_netbox() {
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

normalize_netbox_certificate_permissions() {
  local cert_dir="$1"
  local cert_file="${cert_dir}/netbox.crt"
  local key_file="${cert_dir}/netbox.key"
  local chain_file="${cert_dir}/netbox-ca-chain.pem"
  local roots_file="${cert_dir}/netbox-ca-roots.pem"

  chmod 0755 "${cert_dir}"
  chown 1000:1000 "${cert_dir}"
  [[ -f "${cert_file}" ]] && chmod 0644 "${cert_file}" && chown 1000:1000 "${cert_file}"
  [[ -f "${chain_file}" ]] && chmod 0644 "${chain_file}" && chown 1000:1000 "${chain_file}"
  [[ -f "${roots_file}" ]] && chmod 0644 "${roots_file}" && chown 1000:1000 "${roots_file}"
  [[ -f "${key_file}" ]] && chmod 0600 "${key_file}" && chown 1000:1000 "${key_file}"
}

issue_netbox_certificates() {
  local cert_dir="${NETBOX_DIR}/certs"
  local cert_file="${cert_dir}/netbox.crt"
  local key_file="${cert_dir}/netbox.key"
  local cert_dir_in_container="/etc/provider-box/netbox-certs"
  local password_file_in_container="/home/step/${CA_PASSWORD_FILE#${CA_DATA_DIR}/}"

  install -d -m 0755 "${cert_dir}"
  if [[ "$(stat -c %u "${cert_dir}")" != "1000" ]]; then
    chown 1000:1000 "${cert_dir}"
  fi

  if certificate_matches_dns_identity "${cert_file}" "${key_file}" "${NETBOX_FQDN}"; then
    echo "Reusing existing NetBox certificate for ${NETBOX_FQDN}."
    normalize_netbox_certificate_permissions "${cert_dir}"
    return
  fi

  if [[ -f "${cert_file}" || -f "${key_file}" ]]; then
    echo "Existing NetBox certificate is not valid for ${NETBOX_FQDN}; issuing replacement."
  else
    echo "Issuing NetBox certificate for ${NETBOX_FQDN}."
  fi
  rm -f \
    "${cert_file}" \
    "${key_file}" \
    "${cert_dir}/netbox-ca-chain.pem" \
    "${cert_dir}/netbox-ca-roots.pem" \
    "${cert_dir}/netbox-leaf.crt"

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca certificate "${NETBOX_FQDN}" "${cert_dir_in_container}/netbox-leaf.crt" "${cert_dir_in_container}/netbox.key" \
      --san "${NETBOX_FQDN}" \
      --not-after "${SERVICE_CERT_DURATION}" \
      --issuer "${CA_PROVISIONER_NAME}" \
      --provisioner-password-file "${password_file_in_container}" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to issue a NetBox certificate from step-ca."

  mv "${cert_dir}/netbox-leaf.crt" "${cert_dir}/netbox.crt" || \
    fail "Failed to store the NetBox certificate chain."

  cat "${CA_DATA_DIR}/certs/intermediate_ca.crt" "${CA_DATA_DIR}/certs/root_ca.crt" > "${cert_dir}/netbox-ca-chain.pem" || \
    fail "Failed to build the NetBox CA chain bundle."

  docker run --rm --network host \
    --add-host "${CA_FQDN}:127.0.0.1" \
    -v "${CA_DATA_DIR}:/home/step" \
    -v "${cert_dir}:${cert_dir_in_container}" \
    "${CA_IMAGE}" \
    step ca roots "${cert_dir_in_container}/netbox-ca-roots.pem" \
      --ca-url "https://${CA_FQDN}:${CA_PORT}" \
      --root /home/step/certs/root_ca.crt || \
      fail "Failed to fetch the step-ca root bundle for NetBox."

  normalize_netbox_certificate_permissions "${cert_dir}"
}

prepare_netbox_directories() {
  install -d -m 0755 "${NETBOX_DIR}" "${NETBOX_DIR}/certs" "${NETBOX_MEDIA_DIR}" "${NETBOX_REDIS_DATA_DIR}"
  install -d -m 0700 "${NETBOX_POSTGRES_DATA_DIR}"
  chown -R 70:70 "${NETBOX_POSTGRES_DATA_DIR}"
  chmod 0700 "${NETBOX_POSTGRES_DATA_DIR}"
}

render_netbox_stack() {
  build_netbox_dns_seed_block
  build_netbox_service_seed_block
  build_netbox_provider_box_host_description
  render_template "${TEMPLATE_DIR}/docker-compose.netbox.yml.tpl" "${NETBOX_DIR}/docker-compose.yml"
  DOLLAR='$'
  export DOLLAR
  render_template "${TEMPLATE_DIR}/netbox-nginx.conf.tpl" "${NETBOX_DIR}/nginx.conf"
}

verify_netbox_stack() {
  local attempt service missing_services running_services

  for attempt in $(seq 1 60); do
    running_services="$(docker compose ps --services --status running || true)"
    missing_services=""

    for service in postgres redis netbox netbox-https; do
      if ! grep -qx "${service}" <<< "${running_services}"; then
        missing_services="${missing_services}${service} "
      fi
    done

    [[ -z "${missing_services}" ]] && return 0
    sleep 2
  done

  fail "NetBox services did not all reach running state in time. Missing: ${missing_services% }. Check 'docker compose ps' and 'docker compose logs'."
}

wait_for_netbox_https() {
  local attempt http_code
  local netbox_url="https://${NETBOX_FQDN}:${NETBOX_PORT}/"

  echo "Waiting for NetBox to become ready at ${netbox_url}. First start may take several minutes."

  for attempt in $(seq 1 120); do
    http_code="$(curl --silent --show-error \
      --output /dev/null \
      --write-out '%{http_code}' \
      --cacert "${CA_DATA_DIR}/certs/root_ca.crt" \
      --resolve "${NETBOX_FQDN}:${NETBOX_PORT}:127.0.0.1" \
      "${netbox_url}" || true)"

    if [[ -z "${http_code}" ]]; then
      http_code="000"
    fi

    case "${http_code}" in
      200|301|302)
        return 0
        ;;
    esac

    sleep 5
  done

  fail "NetBox did not become ready at ${netbox_url} before timeout. Last observed HTTP status: ${http_code}. Check 'docker compose ps' and 'docker compose logs'."
}

netbox_api_request() {
  local method="$1"
  local endpoint="$2"
  local data="${3:-}"
  local auth_header="${4:-}"
  local response_file http_code response_body
  local curl_args=(
    --silent
    --show-error
    --cacert "${CA_DATA_DIR}/certs/root_ca.crt"
    --resolve "${NETBOX_FQDN}:${NETBOX_PORT}:127.0.0.1"
    -H "Accept: application/json"
    -H "Content-Type: application/json"
    -X "${method}"
    --write-out "%{http_code}"
  )

  response_file="$(mktemp)"
  curl_args+=(--output "${response_file}")

  if [[ -n "${auth_header}" ]]; then
    curl_args+=(-H "${auth_header}")
  fi

  if [[ -n "${data}" ]]; then
    curl_args+=(--data "${data}")
  fi

  http_code="$(curl "${curl_args[@]}" "https://${NETBOX_FQDN}:${NETBOX_PORT}${endpoint}" || true)"
  response_body="$(cat "${response_file}")"
  rm -f "${response_file}"

  case "${http_code}" in
    2*|3*)
      printf '%s' "${response_body}"
      ;;
    *)
      echo "NetBox API request failed: ${method} ${endpoint}" >&2
      if [[ -n "${data}" ]]; then
        echo "Payload: ${data}" >&2
      fi
      if [[ -n "${response_body}" ]]; then
        echo "Response: ${response_body}" >&2
      fi
      fail "NetBox API request returned HTTP ${http_code:-000}"
      ;;
  esac
}

json_first_id() {
  local first_id
  first_id="$(tr -d '\n' | grep -o '"id":[0-9]*' | head -n1 | cut -d: -f2 || true)"
  printf '%s' "${first_id}"
}

json_string_field() {
  local field="$1"
  sed -n "s/.*\"${field}\":\"\\([^\"]*\\)\".*/\\1/p" | head -n1
}

netbox_api_token() {
  local response token
  response="$(netbox_api_request POST /api/users/tokens/provision/ "{\"username\":\"${NETBOX_SUPERUSER_NAME}\",\"password\":\"${NETBOX_SUPERUSER_PASSWORD}\"}")" || \
    fail "Failed to provision a NetBox API token for ${NETBOX_SUPERUSER_NAME}."
  token="$(printf '%s' "${response}" | json_string_field key)"
  [[ -n "${token}" ]] || fail "Failed to extract the NetBox API token from the provision response."
  printf '%s' "${token}"
}

netbox_get_object_id() {
  local endpoint="$1"
  local query="$2"
  netbox_api_request GET "${endpoint}?${query}&brief=1" "" "Authorization: Token ${NETBOX_API_TOKEN}" | json_first_id
}

netbox_create_object() {
  local endpoint="$1"
  local payload="$2"
  netbox_api_request POST "${endpoint}" "${payload}" "Authorization: Token ${NETBOX_API_TOKEN}" | json_first_id
}

netbox_patch_object() {
  local endpoint="$1"
  local object_id="$2"
  local payload="$3"
  netbox_api_request PATCH "${endpoint}${object_id}/" "${payload}" "Authorization: Token ${NETBOX_API_TOKEN}" >/dev/null
}

ensure_netbox_site() {
  local site_id
  site_id="$(netbox_get_object_id /api/dcim/sites/ "name=Provider%20Box")"
  if [[ -z "${site_id}" ]]; then
    site_id="$(netbox_create_object /api/dcim/sites/ '{"name":"Provider Box","slug":"provider-box","status":"active"}')"
  fi
  [[ -n "${site_id}" ]] || fail "Failed to create or find the Provider Box site in NetBox."
  NETBOX_SITE_ID="${site_id}"
}

ensure_netbox_manufacturer() {
  local manufacturer_id
  manufacturer_id="$(netbox_get_object_id /api/dcim/manufacturers/ "name=Provider%20Box")"
  if [[ -z "${manufacturer_id}" ]]; then
    manufacturer_id="$(netbox_create_object /api/dcim/manufacturers/ '{"name":"Provider Box","slug":"provider-box"}')"
  fi
  [[ -n "${manufacturer_id}" ]] || fail "Failed to create or find the Provider Box manufacturer in NetBox."
  NETBOX_MANUFACTURER_ID="${manufacturer_id}"
}

ensure_netbox_device_type() {
  local device_type_id payload
  device_type_id="$(netbox_get_object_id /api/dcim/device-types/ "model=Provider%20Box")"
  if [[ -z "${device_type_id}" ]]; then
    payload="$(printf '{"manufacturer":%s,"model":"Provider Box","slug":"provider-box"}' "${NETBOX_MANUFACTURER_ID}")"
    device_type_id="$(netbox_create_object /api/dcim/device-types/ "${payload}")"
  fi
  [[ -n "${device_type_id}" ]] || fail "Failed to create or find the Provider Box device type in NetBox."
  NETBOX_DEVICE_TYPE_ID="${device_type_id}"
}

ensure_netbox_device_role() {
  local role_id
  role_id="$(netbox_get_object_id /api/dcim/device-roles/ "name=Provider%20Services")"
  if [[ -z "${role_id}" ]]; then
    role_id="$(netbox_create_object /api/dcim/device-roles/ '{"name":"Provider Services","slug":"provider-services","color":"607d8b"}')"
  fi
  [[ -n "${role_id}" ]] || fail "Failed to create or find the Provider Services device role in NetBox."
  NETBOX_DEVICE_ROLE_ID="${role_id}"
}

ensure_netbox_device() {
  local device_id create_payload update_payload
  device_id="$(netbox_get_object_id /api/dcim/devices/ "name=provider-box")"
  create_payload="$(printf '{"name":"provider-box","site":%s,"device_type":%s,"role":%s,"status":"active"}' "${NETBOX_SITE_ID}" "${NETBOX_DEVICE_TYPE_ID}" "${NETBOX_DEVICE_ROLE_ID}")"
  update_payload="$(printf '{"site":%s,"device_type":%s,"role":%s,"status":"active"}' "${NETBOX_SITE_ID}" "${NETBOX_DEVICE_TYPE_ID}" "${NETBOX_DEVICE_ROLE_ID}")"

  if [[ -z "${device_id}" ]]; then
    device_id="$(netbox_create_object /api/dcim/devices/ "${create_payload}")"
  else
    netbox_patch_object /api/dcim/devices/ "${device_id}" "${update_payload}"
  fi

  [[ -n "${device_id}" ]] || fail "Failed to create or find the Provider Box device in NetBox."
  NETBOX_DEVICE_ID="${device_id}"
}

ensure_netbox_service() {
  local name="$1"
  local fqdn="$2"
  local protocol="$3"
  local port="$4"
  local service_id payload

  service_id="$(netbox_get_object_id /api/ipam/services/ "device_id=${NETBOX_DEVICE_ID}&name=${name}")"
  payload="$(printf '{"parent_object_type":"dcim.device","parent_object_id":%s,"name":"%s","protocol":"%s","ports":[%s],"description":"%s"}' "${NETBOX_DEVICE_ID}" "${name}" "${protocol}" "${port}" "${fqdn}")"

  if [[ -z "${service_id}" ]]; then
    service_id="$(netbox_create_object /api/ipam/services/ "${payload}")"
  else
    netbox_patch_object /api/ipam/services/ "${service_id}" "${payload}"
  fi
}

ensure_netbox_prefix() {
  local prefix="$1"
  local source="$2"
  local prefix_id payload

  prefix_id="$(netbox_get_object_id /api/ipam/prefixes/ "prefix=${prefix}")"
  payload="$(printf '{"prefix":"%s","status":"active","description":"Imported from %s"}' "${prefix}" "${source}")"

  if [[ -z "${prefix_id}" ]]; then
    netbox_create_object /api/ipam/prefixes/ "${payload}" >/dev/null
  else
    netbox_patch_object /api/ipam/prefixes/ "${prefix_id}" "${payload}"
  fi
}

ensure_netbox_ip_address() {
  local fqdn="$1"
  local ip_value="$2"
  local source="$3"
  local host_ip address prefix
  local ip_id payload

  host_ip="$(extract_ipv4_from_value "${ip_value}")"
  if value_has_cidr "${ip_value}"; then
    prefix="$(cidr_to_network "${ip_value}")"
    ensure_netbox_prefix "${prefix}" "${source}"
    address="${ip_value}"
  else
    address="${host_ip}/32"
  fi

  ip_id="$(netbox_get_object_id /api/ipam/ip-addresses/ "address=${address}")"
  payload="$(printf '{"address":"%s","dns_name":"%s","status":"active","description":"Imported from %s"}' "${address}" "${fqdn}" "${source}")"

  if [[ -z "${ip_id}" ]]; then
    netbox_create_object /api/ipam/ip-addresses/ "${payload}" >/dev/null
  else
    # Keep a single IP object per unique address value. Multiple FQDNs may
    # resolve to the same address/mask, so re-runs intentionally leave the
    # existing object unchanged rather than overwriting its canonical dns_name.
    return 0
  fi
}

ensure_provider_box_host_ip_address() {
  local address prefix
  local ip_id payload

  if value_has_cidr "${HOST_IP}"; then
    prefix="$(cidr_to_network "${HOST_IP}")"
    ensure_netbox_prefix "${prefix}" "provider-box"
    address="${HOST_IP}"
  else
    address="${HOST_IPV4}/32"
  fi

  ip_id="$(netbox_get_object_id /api/ipam/ip-addresses/ "address=${address}")"
  payload="$(printf '{"address":"%s","dns_name":"%s","status":"active","description":"%s"}' "${address}" "${PROVIDER_BOX_FQDN}" "${NETBOX_PROVIDER_BOX_HOST_DESCRIPTION}")"

  if [[ -z "${ip_id}" ]]; then
    netbox_create_object /api/ipam/ip-addresses/ "${payload}" >/dev/null
  else
    netbox_patch_object /api/ipam/ip-addresses/ "${ip_id}" "${payload}"
  fi
}

seed_netbox_via_api() {
  local line name fqdn protocol port record_fqdn record_ip record_source
  NETBOX_API_TOKEN="$(netbox_api_token)"

  ensure_netbox_site
  ensure_netbox_manufacturer
  ensure_netbox_device_type
  ensure_netbox_device_role
  ensure_netbox_device
  ensure_provider_box_host_ip_address

  while IFS='|' read -r name fqdn protocol port; do
    [[ -n "${name}" ]] || continue
    ensure_netbox_service "${name}" "${fqdn}" "${protocol}" "${port}"
  done <<< "${NETBOX_PROVIDER_SERVICES}"

  while IFS='|' read -r record_fqdn record_ip record_source; do
    [[ -n "${record_fqdn}" ]] || continue
    ensure_netbox_ip_address "${record_fqdn}" "${record_ip}" "${record_source}"
  done <<< "${NETBOX_DNS_RECORDS}"
}

print_netbox_summary() {
  cat <<SUMMARY
NetBox deployed
URL: https://${NETBOX_FQDN}:${NETBOX_PORT}
NetBox media: ${NETBOX_MEDIA_DIR}
PostgreSQL data: ${NETBOX_POSTGRES_DATA_DIR}
Redis data: ${NETBOX_REDIS_DATA_DIR}
Initial superuser: ${NETBOX_SUPERUSER_NAME} (${NETBOX_SUPERUSER_EMAIL})
SUMMARY
}

do_netbox() {
  require_netbox_vars
  require_netbox_ca_vars
  if [[ -f "${RECORDS_FILE}" ]]; then
    validate_records_file
  else
    echo "No custom DNS records file found at ${RECORDS_FILE}; skipping import. Copy config/unbound.records.example to config/unbound.records to add external/custom records."
  fi
  common_pkgs
  docker_pkgs
  require_ca_ready_for_netbox
  prepare_netbox_directories
  issue_netbox_certificates
  render_netbox_stack
  (
    cd "${NETBOX_DIR}"
    docker compose down || true
    docker compose up -d
    verify_netbox_stack
  )
  wait_for_netbox_https
  seed_netbox_via_api
  ufw allow "${NETBOX_PORT}/tcp" || true
  print_netbox_summary
}

remove_netbox() {
  local runtime_dir="${NETBOX_DIR}"
  local compose_file="${runtime_dir}/docker-compose.yml"

  require_netbox_remove_vars

  if [[ -f "${compose_file}" ]]; then
    require_command docker
    (
      cd "${runtime_dir}"
      docker compose down || true
    )
  fi

  rm -f "${NETBOX_DIR}/docker-compose.yml" "${NETBOX_DIR}/nginx.conf"
  rm -rf "${NETBOX_DIR}/certs"
  echo "Removed NetBox containers and runtime files. Persistent data in ${NETBOX_MEDIA_DIR}, ${NETBOX_POSTGRES_DATA_DIR}, and ${NETBOX_REDIS_DATA_DIR} was preserved."
}
