services:
  db:
    image: {{.ZITADEL_POSTGRES_IMAGE}}
    restart: unless-stopped
    environment:
      POSTGRES_DB: "{{.ZITADEL_PG_DB}}"
      POSTGRES_USER: "{{.ZITADEL_PG_USER}}"
      POSTGRES_PASSWORD: "{{.ZITADEL_PG_PASSWORD}}"
    volumes:
      - {{.ZITADEL_DIR}}/postgres:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U {{.ZITADEL_PG_USER}} -d {{.ZITADEL_PG_DB}}"]
      interval: 15s
      timeout: 5s
      retries: 10

  zitadel:
    image: {{.ZITADEL_IMAGE}}
    restart: unless-stopped
    # TLS is terminated by the proxy below; the core serves plain HTTP on 8080
    # and trusts the proxy's X-Forwarded-Proto (ExternalSecure=true).
    command: start-from-init --masterkeyFromEnv --tlsMode external
    depends_on:
      db:
        condition: service_healthy
    environment:
      ZITADEL_MASTERKEY: "{{.ZITADEL_MASTERKEY}}"
      ZITADEL_EXTERNALDOMAIN: "{{.ZITADEL_FQDN}}"
      ZITADEL_EXTERNALPORT: "{{.ZITADEL_PORT}}"
      ZITADEL_EXTERNALSECURE: "true"
      # v4: require the decoupled Login V2 UI (the `login` service below).
      ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_REQUIRED: "true"
      ZITADEL_DATABASE_POSTGRES_HOST: db
      ZITADEL_DATABASE_POSTGRES_PORT: "5432"
      ZITADEL_DATABASE_POSTGRES_DATABASE: "{{.ZITADEL_PG_DB}}"
      ZITADEL_DATABASE_POSTGRES_USER_USERNAME: "{{.ZITADEL_PG_USER}}"
      ZITADEL_DATABASE_POSTGRES_USER_PASSWORD: "{{.ZITADEL_PG_PASSWORD}}"
      ZITADEL_DATABASE_POSTGRES_USER_SSL_MODE: disable
      ZITADEL_DATABASE_POSTGRES_ADMIN_USERNAME: "{{.ZITADEL_PG_USER}}"
      ZITADEL_DATABASE_POSTGRES_ADMIN_PASSWORD: "{{.ZITADEL_PG_PASSWORD}}"
      ZITADEL_DATABASE_POSTGRES_ADMIN_SSL_MODE: disable
      ZITADEL_FIRSTINSTANCE_ORG_HUMAN_USERNAME: "{{.ZITADEL_ADMIN_USERNAME}}"
      ZITADEL_FIRSTINSTANCE_ORG_HUMAN_PASSWORD: "{{.ZITADEL_ADMIN_PASSWORD}}"
      ZITADEL_FIRSTINSTANCE_ORG_HUMAN_PASSWORDCHANGEREQUIRED: "false"
      # Admin service account whose PAT the control plane reads to provision the
      # bootstrap project/app/user post-deploy.
      ZITADEL_FIRSTINSTANCE_ORG_MACHINE_MACHINE_USERNAME: labprovider-admin-sa
      ZITADEL_FIRSTINSTANCE_ORG_MACHINE_MACHINE_NAME: labprovider-admin-sa
      ZITADEL_FIRSTINSTANCE_ORG_MACHINE_PAT_EXPIRATIONDATE: "2099-01-01T00:00:00Z"
      ZITADEL_FIRSTINSTANCE_PATPATH: /machinekey/pat.txt
      # Login-client service account the Login V2 container authenticates with.
      # On a fresh install the setup job creates it and writes its PAT here.
      ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_MACHINE_USERNAME: login-client
      ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_MACHINE_NAME: Login Client
      ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_PAT_EXPIRATIONDATE: "2099-01-01T00:00:00Z"
      ZITADEL_FIRSTINSTANCE_LOGINCLIENTPATPATH: /machinekey/login-client.pat
    volumes:
      - {{.ZITADEL_DIR}}/machinekey:/machinekey

  login:
    image: {{.ZITADEL_LOGIN_IMAGE}}
    restart: unless-stopped
    depends_on:
      - zitadel
    environment:
      ZITADEL_API_URL: http://zitadel:8080
      ZITADEL_SERVICE_USER_TOKEN_FILE: /machinekey/login-client.pat
      NEXT_PUBLIC_BASE_PATH: /ui/v2/login
      # The login container reaches the core over the internal service name, so
      # Zitadel would see Host: zitadel and fail to match the virtual instance
      # (which is keyed on the external domain). Override the Host header so the
      # instance lookup resolves and public URLs carry the external host:port.
      CUSTOM_REQUEST_HEADERS: "Host:{{.ZITADEL_FQDN}}:{{.ZITADEL_PORT}}"
    volumes:
      - {{.ZITADEL_DIR}}/machinekey:/machinekey:ro

  proxy:
    image: {{.ZITADEL_NGINX_IMAGE}}
    restart: unless-stopped
    depends_on:
      - zitadel
      - login
    ports:
      - "{{.ZITADEL_PORT}}:8443"
    volumes:
      - {{.WORKDIR}}/zitadel/nginx.conf:/etc/nginx/conf.d/default.conf:ro
      - {{.ZITADEL_DIR}}/certs/{{.ZITADEL_FQDN}}:/etc/labprovider/certs:ro
