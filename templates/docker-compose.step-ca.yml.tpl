services:
  stepca-postgres:
    image: ${CA_POSTGRES_IMAGE}
    restart: unless-stopped
    environment:
      POSTGRES_DB: "${CA_POSTGRES_DB}"
      POSTGRES_USER: "${CA_POSTGRES_USER}"
      POSTGRES_PASSWORD: "${CA_POSTGRES_PASSWORD}"
    # Loopback-only publish so the host-networked dashboard can read the cert
    # tables over 127.0.0.1 with its SELECT-only role. Never expose off-host.
    ports:
      - "127.0.0.1:${CA_POSTGRES_PORT}:5432"
    volumes:
      - ${CA_POSTGRES_DATA_DIR:?CA_POSTGRES_DATA_DIR must be set (empty would create a blank bind-mount source)}:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${CA_POSTGRES_USER} -d ${CA_POSTGRES_DB}"]
      interval: 15s
      timeout: 5s
      retries: 10

  step-ca:
    image: ${CA_IMAGE}
    restart: unless-stopped
    depends_on:
      stepca-postgres:
        condition: service_healthy
    # `step ca health` resolves CA_FQDN through Docker's embedded resolver
    # (127.0.0.11), which cannot answer for the lab zone, so pin it to the
    # local listener. This verifies against the cert SAN (CA_FQDN) instead of
    # localhost (which would fail SAN verification). Matches the --add-host
    # 127.0.0.1 idiom the service modules use for the same reason.
    extra_hosts:
      - "${CA_FQDN}:127.0.0.1"
    environment:
      DOCKER_STEPCA_INIT_NAME: "${CA_NAME}"
      DOCKER_STEPCA_INIT_DNS_NAMES: "${CA_FQDN}"
      DOCKER_STEPCA_INIT_PROVISIONER_NAME: "${CA_PROVISIONER_NAME}"
      DOCKER_STEPCA_INIT_PASSWORD_FILE: "${CA_PASSWORD_FILE_IN_CONTAINER}"
      # pgx reads the postgres password from this file (libpq .pgpass format),
      # keeping it out of the dataSource DSN in ca.json.
      PGPASSFILE: "${CA_PGPASSFILE_IN_CONTAINER}"
${CA_ACME_ENV_BLOCK}
    ports:
      - "${CA_PORT}:9000"
    volumes:
      - ${CA_DATA_DIR:?CA_DATA_DIR must be set (empty would create a blank bind-mount source)}:/home/step
    healthcheck:
      test: ["CMD-SHELL", "step ca health --ca-url https://${CA_FQDN}:9000 --root /home/step/certs/root_ca.crt | grep '^ok'"]
      interval: 15s
      timeout: 5s
      retries: 10
