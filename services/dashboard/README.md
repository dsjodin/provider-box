# Provider Box Dashboard

A standalone, **read-only** "current state" view of the Provider Box services.
It queries each service's API on request and renders the result. There is no
persistent store, no history, and no writes to any upstream. It is **not** wired
into `bootstrap/provider-box.sh` or `--all`; run it manually (phase 2 will add a
`--dashboard` module).

This service absorbs the intent of the former design-stage `services/stepca-api`
(the "list issued certificates" panel); that directory has been removed and its
reusable step-ca BadgerDB reader migrated here as `internal/certs`.

## What it shows

Five panels. Each degrades independently: if its source is down or not
configured it renders "unavailable" / "not configured" and never blanks the
page or fails the request.

1. **Certificates (step-ca)** - active certs with subject/SANs, provisioner,
   notBefore/notAfter and days-to-expiry, flagged against a warn threshold.
   Read directly from step-ca's BadgerDB via a per-read snapshot copy (the live
   DB is never opened directly - see `STEPCA_STORAGE.md`).
2. **DNS (Technitium)** - zone list, managed record count per zone, the
   forwarder in use, and whether the TLS console/API (`:53443`) is reachable.
   Uses the same API shapes as `dns-sync` and the technitium module.
3. **IPAM (NetBox)** - prefix and IP-address counts and the `dns_name`
   inventory. Read with a dedicated, minimum-read-scope token.
4. **Services (Docker)** - container name, state, health, uptime and image tag
   for the Provider Box stacks, read from the Docker socket (mounted read-only).
5. **Recent errors (logs)** - the last error-level lines per service from a
   bounded log tail. `dns-sync` emits slog JSON, so `level>=error` is parsed;
   non-JSON lines fall back to a token match.

`GET /` renders the HTML page; `GET /api/state` returns the same data as JSON;
`GET /healthz` is a liveness probe.

## Security posture

- **Read-only throughout.** No upstream write path exists in the code.
  - NetBox: use a **dedicated** token with the minimum read scope
    (`ipam.view_prefix`, `ipam.view_ipaddress`; a fine-grained read-only token
    on NetBox versions that support it). Do **not** reuse the `dns-sync` or
    bootstrap admin token.
  - Technitium: a scoped API token (the API has no per-scope tokens, so create a
    non-admin user's token where possible; it is only ever used for
    `zones/list`, `zones/records/get`, and `settings/get`).
  - step-ca: the DB is read via a read-only snapshot; there is no signing path.
  - Docker socket is mounted `:ro`.
- **Tokens come from files/env, never hardcoded, never logged.** The compose
  file mounts them from `DASHBOARD_SECRETS_DIR` read-only.
- **The dashboard serves HTTPS** with a step-ca-issued cert for its FQDN. If no
  cert is configured it falls back to plaintext HTTP with a logged warning
  (lab only).
- **No auth on the UI itself (v1).** This is acceptable only on a trusted,
  internal lab network. **TODO before any non-lab use: put the dashboard behind
  authentication** (e.g. the Authentik/Keycloak already in this repo, or a
  reverse proxy with auth). Until then, do not expose it beyond the lab.

## Running it manually

Prerequisites: `--ca` deployed (for the step-ca DB and root cert), and the
services you want panels for (`--technitium`, `--netbox`, `--dns-sync`).

1. **Add the dashboard variables to your config.** Copy the `DASHBOARD_*` block
   from `config/provider-box.env.example` into your `config/provider-box.env`
   and adjust. Set `DASHBOARD_DOCKER_GID` to the host docker gid
   (`getent group docker | cut -d: -f3`).

2. **Issue the dashboard's TLS cert** from step-ca (same pattern as the
   technitium module), writing `dashboard.crt` / `dashboard.key` into
   `DASHBOARD_CERT_DIR`:

   ```sh
   source config/provider-box.env
   install -d -m 0755 "${DASHBOARD_CERT_DIR}"
   docker run --rm --network host \
     --add-host "${CA_FQDN}:127.0.0.1" \
     -v "${CA_DATA_DIR}:/home/step" \
     -v "${DASHBOARD_CERT_DIR}:/certs" \
     "${CA_IMAGE}" \
     step ca certificate "${DASHBOARD_FQDN}" /certs/dashboard.crt /certs/dashboard.key \
       --san "${DASHBOARD_FQDN}" \
       --not-after "${SERVICE_CERT_DURATION}" \
       --issuer "${CA_PROVISIONER_NAME}" \
       --provisioner-password-file "/home/step/${CA_PASSWORD_FILE#${CA_DATA_DIR}/}" \
       --ca-url "https://${CA_FQDN}:${CA_PORT}" \
       --root /home/step/certs/root_ca.crt
   chown -R 1000:1000 "${DASHBOARD_CERT_DIR}"
   ```

3. **Provide the scoped read-only tokens** in `DASHBOARD_SECRETS_DIR`
   (mode 0600, owner uid 1000):
   - `technitium.token` - a scoped Technitium API token.
   - `netbox-readonly.token` - a dedicated NetBox read-only token (create it in
     NetBox under a non-admin user with view-only IPAM permissions).

4. **Start it:**

   ```sh
   cd services/dashboard
   docker compose --env-file ../../config/provider-box.env up -d --build
   ```

   Then browse `https://${DASHBOARD_FQDN}:8445/` (the `DASHBOARD_ADDR` port).

Any panel whose upstream URL/token/path is unset simply renders "not
configured", so you can run with a subset of sources.

## Configuration

All settings are environment variables (see the `DASHBOARD_*` block in
`config/provider-box.env.example` for the documented set). The binary also
reads them directly, so it can run outside Docker for development:

```sh
DASHBOARD_ADDR=:8445 \
DASHBOARD_STEPCA_DB=/opt/provider-box/step-ca/db \
DASHBOARD_TECHNITIUM_URL=https://dns.sddc.lab:53443 \
DASHBOARD_TECHNITIUM_TOKEN=... \
go run ./cmd/dashboard
```

Without `DASHBOARD_TLS_CERT`/`DASHBOARD_TLS_KEY` it serves plaintext HTTP with a
warning - fine for local development, not for the lab network.

## Phase 2 (explicitly out of scope for v1)

- **History / collector.** v1 fetches on page load only; there is no background
  polling, time series, or store.
- **Bootstrap integration.** A `--dashboard` module for `provider-box.sh`
  (cert issuance, token provisioning, `--remove`), and inclusion in `--all`.
- **UI authentication.** Front the dashboard with the repo's IdP or a
  reverse-proxy auth layer before any non-lab exposure.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

Each upstream client has a table-driven parsing test over recorded sample
payloads; the server has tests for per-panel isolation (source up, source down,
not configured).
