services:
  postgresql:
    image: ${AUTHENTIK_POSTGRES_IMAGE}
    restart: unless-stopped
    environment:
      POSTGRES_DB: "${AUTHENTIK_PG_DB}"
      POSTGRES_USER: "${AUTHENTIK_PG_USER}"
      POSTGRES_PASSWORD: "${AUTHENTIK_PG_PASSWORD}"
    volumes:
      - ${AUTHENTIK_DIR:?AUTHENTIK_DIR must be set (empty would create a blank bind-mount source)}/postgres:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${AUTHENTIK_PG_USER} -d ${AUTHENTIK_PG_DB}"]
      interval: 15s
      timeout: 5s
      retries: 10

  server:
    image: ${AUTHENTIK_IMAGE}
    restart: unless-stopped
    command: server
    shm_size: 512mb
    depends_on:
      postgresql:
        condition: service_healthy
    environment:
      AUTHENTIK_SECRET_KEY: "${AUTHENTIK_SECRET_KEY}"
      AUTHENTIK_POSTGRESQL__HOST: postgresql
      AUTHENTIK_POSTGRESQL__NAME: "${AUTHENTIK_PG_DB}"
      AUTHENTIK_POSTGRESQL__USER: "${AUTHENTIK_PG_USER}"
      AUTHENTIK_POSTGRESQL__PASSWORD: "${AUTHENTIK_PG_PASSWORD}"
      AUTHENTIK_ERROR_REPORTING__ENABLED: "false"
      AUTHENTIK_DISABLE_UPDATE_CHECK: "true"
      AUTHENTIK_BOOTSTRAP_PASSWORD: "${AUTHENTIK_ADMIN_PASSWORD}"
      AUTHENTIK_BOOTSTRAP_TOKEN: "${AUTHENTIK_API_TOKEN}"
    ports:
      - "${AUTHENTIK_PORT}:9443"
    volumes:
      - ${AUTHENTIK_DIR:?AUTHENTIK_DIR must be set (empty would create a blank bind-mount source)}/certs:/certs:ro
      - ${AUTHENTIK_DIR:?AUTHENTIK_DIR must be set (empty would create a blank bind-mount source)}/data:/data
      - ${WORKDIR:?WORKDIR must be set (empty would create a blank bind-mount source)}/authentik/blueprints:/blueprints/custom:ro

  worker:
    image: ${AUTHENTIK_IMAGE}
    restart: unless-stopped
    command: worker
    shm_size: 512mb
    depends_on:
      postgresql:
        condition: service_healthy
    environment:
      AUTHENTIK_SECRET_KEY: "${AUTHENTIK_SECRET_KEY}"
      AUTHENTIK_POSTGRESQL__HOST: postgresql
      AUTHENTIK_POSTGRESQL__NAME: "${AUTHENTIK_PG_DB}"
      AUTHENTIK_POSTGRESQL__USER: "${AUTHENTIK_PG_USER}"
      AUTHENTIK_POSTGRESQL__PASSWORD: "${AUTHENTIK_PG_PASSWORD}"
      AUTHENTIK_ERROR_REPORTING__ENABLED: "false"
      AUTHENTIK_DISABLE_UPDATE_CHECK: "true"
      AUTHENTIK_BOOTSTRAP_PASSWORD: "${AUTHENTIK_ADMIN_PASSWORD}"
      AUTHENTIK_BOOTSTRAP_TOKEN: "${AUTHENTIK_API_TOKEN}"
    volumes:
      - ${AUTHENTIK_DIR:?AUTHENTIK_DIR must be set (empty would create a blank bind-mount source)}/certs:/certs:ro
      - ${AUTHENTIK_DIR:?AUTHENTIK_DIR must be set (empty would create a blank bind-mount source)}/data:/data
      - ${WORKDIR:?WORKDIR must be set (empty would create a blank bind-mount source)}/authentik/blueprints:/blueprints/custom:ro
