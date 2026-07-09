services:
  sftpgo:
    image: ${SFTPGO_IMAGE}
    restart: unless-stopped
    environment:
      SFTPGO_DATA_PROVIDER__CREATE_DEFAULT_ADMIN: "true"
      SFTPGO_DEFAULT_ADMIN_USERNAME: "${SFTP_ADMIN_USER}"
      SFTPGO_DEFAULT_ADMIN_PASSWORD: "${SFTP_ADMIN_PASSWORD}"
      SFTPGO_HTTPD__BINDINGS__0__PORT: "8080"
      SFTPGO_HTTPD__BINDINGS__0__ENABLE_HTTPS: "1"
      SFTPGO_HTTPD__BINDINGS__0__CERTIFICATE_FILE: /var/lib/sftpgo/certs/sftpgo.crt
      SFTPGO_HTTPD__BINDINGS__0__CERTIFICATE_KEY_FILE: /var/lib/sftpgo/certs/sftpgo.key
    ports:
      - "${SFTP_PORT}:2022"
      - "${SFTP_ADMIN_PORT}:8080"
    volumes:
      - ${SFTP_DATA_DIR:?SFTP_DATA_DIR must be set (empty would create a blank bind-mount source)}:/srv/sftpgo
      - ${SFTP_HOME_DIR:?SFTP_HOME_DIR must be set (empty would create a blank bind-mount source)}:/var/lib/sftpgo
      - ${SFTP_CERT_DIR:?SFTP_CERT_DIR must be set (empty would create a blank bind-mount source)}:/var/lib/sftpgo/certs:ro
