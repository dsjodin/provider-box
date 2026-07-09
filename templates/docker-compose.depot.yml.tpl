services:
  depot:
    image: ${DEPOT_IMAGE}
    restart: unless-stopped
    ports:
      - "${DEPOT_HTTP_PORT}:80"
      - "${DEPOT_HTTPS_PORT}:443"
    volumes:
      - ${WORKDIR:?WORKDIR must be set (empty would create a blank bind-mount source)}/depot/nginx.conf:/etc/nginx/nginx.conf:ro
      - ${DEPOT_DATA_DIR:?DEPOT_DATA_DIR must be set (empty would create a blank bind-mount source)}:/usr/share/nginx/html:ro
      - ${DEPOT_CERT_DIR:?DEPOT_CERT_DIR must be set (empty would create a blank bind-mount source)}:/etc/provider-box/certs:ro
      - ${DEPOT_AUTH_DIR:?DEPOT_AUTH_DIR must be set (empty would create a blank bind-mount source)}:/etc/nginx/auth:ro
