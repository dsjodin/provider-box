services:
  traefik:
    image: {{.TRAEFIK_IMAGE}}
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - {{.WORKDIR}}/traefik/traefik.yml:/etc/traefik/traefik.yml:ro
      - {{.WORKDIR}}/traefik/dynamic:/etc/traefik/dynamic:ro
      - {{.WORKDIR}}/traefik/usersfile:/usersfile:ro
      - {{.TRAEFIK_DIR}}/certs:/certs:ro
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks:
      - proxy

networks:
  proxy:
    external: true
