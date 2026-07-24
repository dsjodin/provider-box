tls:
  stores:
    default:
      defaultCertificate:
        certFile: /certs/wildcard.crt
        keyFile: /certs/wildcard.key

http:
  serversTransports:
    insecure:
      insecureSkipVerify: true

  middlewares:
    dashboard-auth:
      basicAuth:
        usersFile: /usersfile

  routers:
    dashboard:
      rule: "Host(`{{.TRAEFIK_FQDN}}`)"
      entryPoints:
        - websecure
      service: api@internal
      middlewares:
        - dashboard-auth
      tls: {}
    control-plane:
      rule: "Host(`{{.CONTROL_PLANE_FQDN}}`)"
      entryPoints:
        - websecure
      service: control-plane
      tls: {}
{{- if eq .VMSCA_ENABLE "true"}}
    certsrv:
      rule: "Host(`{{.VMSCA_FQDN}}`)"
      entryPoints:
        - websecure
      service: certsrv
      tls: {}
{{- end}}

  services:
    control-plane:
      loadBalancer:
        serversTransport: insecure
        servers:
          - url: "https://{{.HOST_IPV4}}:{{.CONTROL_PLANE_PORT}}"
{{- if eq .VMSCA_ENABLE "true"}}
    certsrv:
      loadBalancer:
        serversTransport: insecure
        servers:
          - url: "https://{{.HOST_IPV4}}:{{.VMSCA_PORT}}"
{{- end}}
