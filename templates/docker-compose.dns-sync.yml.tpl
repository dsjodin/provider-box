services:
  dns-sync:
    image: ${DNS_SYNC_IMAGE}
    restart: unless-stopped
    user: "1000:1000"
    environment:
      NETBOX_URL: "${DNS_SYNC_NETBOX_URL}"
      NETBOX_TOKEN_FILE: "/run/provider-box/secrets/netbox.token"
      NETBOX_CA_BUNDLE: "/etc/provider-box/certs/root_ca.crt"
      TECHNITIUM_URL: "${DNS_SYNC_TECHNITIUM_URL}"
      TECHNITIUM_TOKEN_FILE: "/run/provider-box/secrets/technitium.token"
      TECHNITIUM_CA_BUNDLE: "/etc/provider-box/certs/root_ca.crt"
      DNS_SYNC_INTERVAL: "${DNS_SYNC_INTERVAL}"
    volumes:
      - ${DNS_SYNC_SECRETS_DIR}:/run/provider-box/secrets:ro
      - ${CA_DATA_DIR}/certs/root_ca.crt:/etc/provider-box/certs/root_ca.crt:ro
