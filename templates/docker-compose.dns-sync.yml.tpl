services:
  dns-sync:
    image: ${DNS_SYNC_IMAGE}
    restart: unless-stopped
    user: "1000:1000"
    # host networking so the 127.0.0.1 pins reach the host-published NetBox
    # and Technitium ports, matching the module's one-shot docker runs; on
    # the default bridge, 127.0.0.1 is the container's own loopback.
    network_mode: host
    extra_hosts:
      - "${DNS_SYNC_NETBOX_HOST}:127.0.0.1"
      - "${DNS_SYNC_TECHNITIUM_HOST}:127.0.0.1"
    environment:
      NETBOX_URL: "${DNS_SYNC_NETBOX_URL}"
      NETBOX_TOKEN_FILE: "/run/provider-box/secrets/netbox.token"
      NETBOX_CA_BUNDLE: "/etc/provider-box/certs/root_ca.crt"
      TECHNITIUM_URL: "${DNS_SYNC_TECHNITIUM_URL}"
      TECHNITIUM_TOKEN_FILE: "/run/provider-box/secrets/technitium.token"
      TECHNITIUM_CA_BUNDLE: "/etc/provider-box/certs/root_ca.crt"
      DNS_SYNC_INTERVAL: "${DNS_SYNC_INTERVAL}"
      DNS_SYNC_BUILTIN_RECORDS: "${DNS_SYNC_BUILTIN_RECORDS}"
      # Non-admin Technitium user the read-only dashboard authenticates as.
      # dns-sync grants it View on each newly created zone so continuously-synced
      # zones appear in the dashboard without a --technitium re-run. Best-effort;
      # empty disables the grant.
      DNS_SYNC_TECHNITIUM_DASHBOARD_USER: "${DNS_SYNC_TECHNITIUM_DASHBOARD_USER}"
    volumes:
      - ${DNS_SYNC_SECRETS_DIR:?DNS_SYNC_SECRETS_DIR must be set (empty would create a blank bind-mount source)}:/run/provider-box/secrets:ro
      - ${CA_DATA_DIR:?CA_DATA_DIR must be set (empty would create a blank bind-mount source)}/certs/root_ca.crt:/etc/provider-box/certs/root_ca.crt:ro
