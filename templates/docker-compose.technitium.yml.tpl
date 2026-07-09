services:
  technitium:
    image: ${TECHNITIUM_IMAGE}
    restart: unless-stopped
    environment:
      DNS_SERVER_DOMAIN: "${DNS_FQDN}"
    ports:
      - "53:53/tcp"
      - "53:53/udp"
      - "${TECHNITIUM_HTTP_PORT}:5380/tcp"
      - "${TECHNITIUM_HTTPS_PORT}:53443/tcp"
    volumes:
      - ${TECHNITIUM_DATA_DIR:?TECHNITIUM_DATA_DIR must be set (empty would create a blank bind-mount source)}:/etc/dns
      - ${TECHNITIUM_CERT_DIR:?TECHNITIUM_CERT_DIR must be set (empty would create a blank bind-mount source)}:/etc/provider-box/technitium-certs:ro
