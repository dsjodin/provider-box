services:
  depot:
    image: {{.DEPOT_IMAGE}}
    restart: unless-stopped
    # TLS is terminated by Traefik (which owns :80/:443); depot serves plain HTTP
    # on 80, fronted at https://{{.DEPOT_FQDN}}. The host port is kept on the
    # loopback for deploy-time readiness only.
    ports:
      - "{{.DEPOT_HTTP_PORT}}:80"
    volumes:
      - {{.WORKDIR}}/depot/nginx.conf:/etc/nginx/nginx.conf:ro
      - {{.DEPOT_DATA_DIR}}:/usr/share/nginx/html:ro
      - {{.DEPOT_AUTH_DIR}}:/etc/nginx/auth:ro
    networks:
      - default
      - proxy
    labels:
      - "traefik.enable=true"
      - "traefik.docker.network=proxy"
      - "traefik.http.routers.depot.rule=Host(`{{.DEPOT_FQDN}}`)"
      - "traefik.http.routers.depot.entrypoints=websecure"
      - "traefik.http.routers.depot.tls=true"
      - "traefik.http.services.depot.loadbalancer.server.port=80"

networks:
  proxy:
    external: true
