# Traefik Reverse Proxy - Design / Implementation Plan

## Context

Today every labprovider HTTPS service resolves to the same `HOST_IPV4` and is
told apart only by port (`netbox:8444`, `keycloak:8443`, `dashboard:8445`,
`certsrv:8446`, ...). Operators have to remember a port per service. The goal is
to reach each service at its bare FQDN on `:443` and drop the port bookkeeping.

Decision record (from design discussion):
- Introduce **Traefik** as a single ingress on `:80`/`:443`.
- **Traefik terminates TLS** with one **step-ca-issued `*.sddc.lab` wildcard
  leaf** served as the file-provider default certificate. No ACME, no per-service
  front certs.
- **Docker-label provider** for the bridge service stacks (dynamic discovery is
  explicitly accepted for this lab-only platform), plus the **file provider** for
  the host-networked control plane. This mirrors the operator's existing homelab
  Traefik.
- **Lab security posture:** Traefik talks **plain HTTP** to backends over a shared
  `proxy` network - no re-encrypt, no back-leg cert trust. Acceptable because this
  is a single-node, trusted-lab platform.

### Already done (separate, committed on this branch)

The certsrv naming fix is implemented and pushed already:
- `VMSCA_FQDN` (default `certsrv.sddc.lab`) added to the env schema/example.
- Published as an A record to `HOST_IPV4` when `VMSCA_ENABLE=true`
  (`builtinServiceFQDNs`, netbox.go).
- Added as a SAN on the control-plane leaf (`IssueCert` now takes extra SANs;
  `CertMatchesDNSIdentity` requires all SANs), so the certsrv listener validates
  at `https://certsrv.sddc.lab:8446/certsrv`.

Traefik **extends** that: once Traefik fronts the control plane, VCF can target
`https://certsrv.sddc.lab/certsrv` with **no port** - the original goal for
certsrv. The `:8446` direct path keeps working via the SAN already added.

---

## Architecture

```
                         :80  -> redirect -> :443
client ---- FQDN:443 ---> Traefik (terminates *.sddc.lab wildcard)
                             |
          docker-label routers (proxy network, plain HTTP)
             |-- netbox.sddc.lab     -> netbox app     :8080
             |-- auth.sddc.lab       -> keycloak       :8080 (http mode)
             |-- idp.sddc.lab        -> authentik      :9000
             |-- zid.sddc.lab        -> zitadel core   :8080
             |-- vcfdepot.sddc.lab   -> depot nginx    :80 (keeps basic-auth)
             |-- s3.sddc.lab         -> seaweedfs s3   :8333 (path-style)
             |-- sftp.sddc.lab       -> sftpgo web UI  :8080
                             |
          file-provider routers (host network, loopback)
             |-- dashboard.sddc.lab  -> 127.0.0.1:8445
             |-- certsrv.sddc.lab    -> 127.0.0.1:8446

Left on their own ports (NOT behind Traefik):
   step-ca :9000 (bootstrap: certs issued before DNS/Traefik exist)
   Technitium DNS :53, chrony NTP :123, rsyslog :514, SFTP (SSH) :2022
```

---

## Changes

### 1. `install.sh` - create the shared `proxy` network (prerequisite)

Every stack references `networks: [proxy]` as `external: true`, so the network
must exist before any stack starts and cannot be owned by the Traefik module
(ordering). Add to the one-time host-prep block:

```bash
docker network inspect proxy >/dev/null 2>&1 || docker network create proxy
```

### 2. New env vars - `config/labprovider.env.example` + `envfile/schema.go`

```
TRAEFIK_ENABLE="true"
TRAEFIK_IMAGE="docker.io/library/traefik:v3.7.7"
TRAEFIK_FQDN="traefik.sddc.lab"          # dashboard
TRAEFIK_DIR="/opt/labprovider/traefik"   # persistent: wildcard cert, dynamic dir
TRAEFIK_DASHBOARD_USERS="..."            # htpasswd line for the Traefik dashboard
```

Schema rows follow the existing `[]string{"traefik"}` service-tag pattern;
`TRAEFIK_FQDN` uses `checkFQDN`, `TRAEFIK_IMAGE` non-empty, etc. Add `TRAEFIK_FQDN`
to `builtinServiceFQDNs` so it resolves like every other name.

### 3. New deploy module - `internal/deploy/traefik.go`

Mirror the `Depot` module shape (`Name/Deps/Deploy/Remove`):

- `Name() "traefik"`, `Deps() []string{"ca"}` (needs the CA to mint the wildcard).
- Deploy:
  1. `EnsureDir(TRAEFIK_DIR, TRAEFIK_DIR/dynamic, TRAEFIK_DIR/certs)`.
  2. **Wildcard leaf:** `IssueCert(ctx, rc, "*."+baseDomain, TRAEFIK_DIR+"/certs", "wildcard")`
     where `baseDomain` is derived from `SEARCH_DOMAIN` (e.g. `sddc.lab`).
     Reuses the existing single helper; no new cert code.
  3. Render the static config `traefik.yml` and the dynamic files
     (`dynamic/tls.yml`, `dynamic/control-plane.yml`) from templates.
  4. `Render("docker-compose.traefik.yml.tpl", ...)`, `Compose("traefik").Down/Up`.
  5. Readiness: `WaitHTTPSPinned` on `https://dashboard.sddc.lab/` (through
     Traefik, validated against the step-ca root) or Traefik `--ping`.
- Remove: `Compose("traefik").Down` + `RemoveAll(Workdir)`; **preserve**
  `TRAEFIK_DIR/certs` (wildcard) per the removal-preserves-data rule.

Register it in `cmd/control-plane/main.go` alongside the others
(`engine.Register(deploy.Traefik{})`), after the CA.

### 4. New templates (`internal/deploy/templates/`) + golden tests

- `docker-compose.traefik.yml.tpl` - image pinned, `ports: 80:80/443:443`,
  docker provider (`exposedbydefault=false`, `network=proxy`), file provider
  (`/etc/traefik/dynamic`, watch), web->websecure redirect, dashboard router with
  basic-auth middleware, `proxy` external network, mount
  `TRAEFIK_DIR/certs` and `TRAEFIK_DIR/dynamic` read-only.
- `traefik-dynamic-tls.yml.tpl` - the default wildcard cert:
  ```yaml
  tls:
    stores:
      default:
        defaultCertificate:
          certFile: /certs/wildcard.crt
          keyFile: /certs/wildcard.key
  ```
- `traefik-dynamic-controlplane.yml.tpl` - file-provider routers/services for the
  two host-networked endpoints, pointing at `http://127.0.0.1:8445` (dashboard)
  and `http://127.0.0.1:8446` (certsrv), `tls: {}` (serves the default wildcard).

Add a `*.golden` for each under `testdata/`, following `render_test.go`.

### 5. Per-stack edits (the bridge services)

For each of netbox, keycloak, authentik, zitadel, depot, s3, sftpgo, in the
compose template:

- Join the shared network:
  ```yaml
  networks:
    proxy:
      external: true
  ```
  and attach the routable container to it.
- Add labels on the routable container (wildcard means no per-router cert):
  ```yaml
  labels:
    - traefik.enable=true
    - traefik.docker.network=proxy
    - traefik.http.routers.<svc>.rule=Host(`<FQDN>`)
    - traefik.http.routers.<svc>.entrypoints=websecure
    - traefik.http.routers.<svc>.tls=true
    - traefik.http.services.<svc>.loadbalancer.server.port=<internal-http-port>
  ```
- Point the router at the **plaintext** app port; drop TLS from the backend:
  - **netbox / zitadel:** delete the nginx sidecar service and its
    `*-nginx.conf.tpl`; label the app on `:8080` (they already speak plain HTTP
    internally).
  - **keycloak:** switch to HTTP mode (`KC_HTTP_ENABLED=true`,
    `KC_PROXY_HEADERS=xforwarded`), drop the cert mounts, label on `:8080`.
  - **authentik:** label the server on `:9000` (already proxy-aware).
  - **depot:** keep its nginx (retains path-scoped basic-auth) but serve plain
    HTTP on `:80`; remove the host `:443` publish (Traefik owns 443).
  - **s3 (seaweedfs):** label on `:8333`; path-style bucket addressing only
    (see Tradeoffs).
  - **sftpgo:** label only the **web admin UI** (`:8080`); the SFTP/SSH service
    stays on host `:2022`.
- Host `ports:` publishes for the HTTP services may be dropped once verified;
  keeping them during transition is harmless. L4 ports stay.

### 6. Deletions (cert-model simplification)

- Remove per-service `IssueCert` calls for the services now fronted by the
  wildcard (netbox, keycloak, authentik, zitadel, depot, s3, sftp) and their cert
  dirs/mounts.
- Delete `netbox-nginx.conf.tpl`, `zitadel-nginx.conf.tpl` and their golden files
  (depot keeps its nginx).
- Net: ~8 per-service leafs + 2 nginx terminators collapse into one wildcard leaf.

### 7. Left unchanged

- **step-ca** (`:9000`) - never fronted; certs are issued with `CA_FQDN` pinned to
  `127.0.0.1` before DNS/Traefik exist.
- **Technitium DNS** `:53`, **chrony** `:123`, **rsyslog** `:514`, **SFTP/SSH**
  `:2022` - non-HTTP, stay on their ports.
- **control-plane / certsrv** keep their own listeners and TLS leaf; Traefik just
  adds a no-port front door via the file provider. The `VMSCA_FQDN` SAN work
  remains valid for direct `:8446`.

---

## Deploy ordering

- `proxy` network: created by `install.sh` (exists before any stack).
- Traefik module depends on `ca` (wildcard issuance). Service stacks carry labels
  regardless of whether Traefik is up (labels are inert without it), so no new
  hard dependency on Traefik is needed - order stays CA-first, consumers after.

---

## Verification

1. `cd services/control-plane && go build ./... && go test ./...` (golden tests
   for the new/changed templates must pass; update goldens intentionally).
2. `docker network inspect proxy` exists after `install.sh`.
3. Deploy CA, then Traefik; confirm the wildcard leaf:
   `openssl x509 -in $TRAEFIK_DIR/certs/wildcard.crt -noout -text | grep -A1 "Subject Alternative Name"` shows `DNS:*.sddc.lab`.
4. Bring up a service (e.g. netbox); from a lab client:
   `curl --cacert root_ca.crt https://netbox.sddc.lab/` returns 200 with **no port**.
5. `curl --cacert root_ca.crt https://certsrv.sddc.lab/certsrv/ -u user:pass`
   returns 200 (no port); point VCF's Microsoft CA at
   `https://certsrv.sddc.lab/certsrv`.
6. Confirm L4 services still answer on their ports (`dig @HOST_IPV4`,
   `sftp -P 2022 ...`).

---

## Tradeoffs (accepted lab posture)

- **Plaintext backends** on the `proxy` network (no re-encrypt). Single trusted
  host; acceptable.
- **Wildcard is one label deep:** `*.sddc.lab` does not cover
  `bucket.s3.sddc.lab`. S3 must use **path-style** (`s3.sddc.lab/bucket`) or the
  client disables TLS verification. Confirm how S3 consumers address buckets.
- **Single wildcard key** shared across all names, and **one expiry** for all UIs
  (re-issue + file-provider hot-reload is the whole renewal story).
- **New always-on ingress** (SPOF) in front of every web UI; dynamic discovery is
  accepted here despite the general Non-Goal.
- **depot** loses its host `:443` publish to Traefik.
