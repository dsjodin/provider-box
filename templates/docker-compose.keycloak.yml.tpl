services:
  keycloak:
    image: ${KEYCLOAK_IMAGE}
    restart: unless-stopped
    environment:
      KC_BOOTSTRAP_ADMIN_USERNAME: "${KEYCLOAK_ADMIN_USER}"
      KC_BOOTSTRAP_ADMIN_PASSWORD: "${KEYCLOAK_ADMIN_PASSWORD}"
      KC_HEALTH_ENABLED: "true"
      KC_HTTP_MANAGEMENT_HEALTH_ENABLED: "false"
      KC_HTTPS_CERTIFICATE_FILE: /opt/keycloak/certs/keycloak.crt
      KC_HTTPS_CERTIFICATE_KEY_FILE: /opt/keycloak/certs/keycloak.key
    ports:
      - "${KEYCLOAK_PORT}:8443"
    volumes:
      - ${KEYCLOAK_DIR:?KEYCLOAK_DIR must be set (empty would create a blank bind-mount source)}/certs:/opt/keycloak/certs:ro
      - ${KEYCLOAK_DIR:?KEYCLOAK_DIR must be set (empty would create a blank bind-mount source)}/data:/opt/keycloak/data
      - ${WORKDIR:?WORKDIR must be set (empty would create a blank bind-mount source)}/keycloak/import:/opt/keycloak/data/import:ro
    command:
      - start
      - --import-realm
      - --hostname=https://${KEYCLOAK_FQDN}:${KEYCLOAK_PORT}
