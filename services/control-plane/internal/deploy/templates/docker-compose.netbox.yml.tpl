services:
  postgres:
    image: {{.NETBOX_POSTGRES_IMAGE}}
    restart: unless-stopped
    environment:
      POSTGRES_DB: "{{.NETBOX_POSTGRES_DB}}"
      POSTGRES_USER: "{{.NETBOX_POSTGRES_USER}}"
      POSTGRES_PASSWORD: "{{.NETBOX_POSTGRES_PASSWORD}}"
    volumes:
      - {{.NETBOX_POSTGRES_DATA_DIR}}:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U {{.NETBOX_POSTGRES_USER}} -d {{.NETBOX_POSTGRES_DB}}"]
      interval: 15s
      timeout: 5s
      retries: 10

  redis:
    image: {{.NETBOX_REDIS_IMAGE}}
    restart: unless-stopped
    command:
      - redis-server
      - --appendonly
      - yes
      - --requirepass
      - {{.NETBOX_REDIS_PASSWORD}}
    volumes:
      - {{.NETBOX_REDIS_DATA_DIR}}:/data

  netbox:
    image: {{.NETBOX_IMAGE}}
    restart: unless-stopped
    depends_on:
      - postgres
      - redis
    environment:
      ALLOWED_HOSTS: "{{.NETBOX_ALLOWED_HOSTS}}"
      CSRF_TRUSTED_ORIGINS: "{{.NETBOX_CSRF_TRUSTED_ORIGINS}}"
      DB_HOST: postgres
      DB_NAME: "{{.NETBOX_POSTGRES_DB}}"
      DB_USER: "{{.NETBOX_POSTGRES_USER}}"
      DB_PASSWORD: "{{.NETBOX_POSTGRES_PASSWORD}}"
      REDIS_HOST: redis
      REDIS_PORT: "6379"
      REDIS_PASSWORD: "{{.NETBOX_REDIS_PASSWORD}}"
      REDIS_DATABASE: "0"
      REDIS_CACHE_HOST: redis
      REDIS_CACHE_PORT: "6379"
      REDIS_CACHE_PASSWORD: "{{.NETBOX_REDIS_PASSWORD}}"
      REDIS_CACHE_DATABASE: "1"
      SECRET_KEY: "{{.NETBOX_SECRET_KEY}}"
      API_TOKEN_PEPPER_1: "{{.NETBOX_API_TOKEN_PEPPER_1}}"
      SUPERUSER_NAME: "{{.NETBOX_SUPERUSER_NAME}}"
      SUPERUSER_EMAIL: "{{.NETBOX_SUPERUSER_EMAIL}}"
      SUPERUSER_PASSWORD: "{{.NETBOX_SUPERUSER_PASSWORD}}"
      SKIP_SUPERUSER: "false"
    volumes:
      - {{.NETBOX_MEDIA_DIR}}:/opt/netbox/netbox/media

  netbox-https:
    image: {{.NETBOX_NGINX_IMAGE}}
    restart: unless-stopped
    depends_on:
      - netbox
    ports:
      - "{{.NETBOX_PORT}}:8443"
    volumes:
      - {{.NETBOX_DIR}}/nginx.conf:/etc/nginx/conf.d/default.conf:ro
      - {{.NETBOX_DIR}}/certs:/etc/provider-box/certs:ro
