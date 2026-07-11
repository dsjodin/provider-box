# Provider Box Dashboard

A **read-only** "current state" view of the Provider Box services. It queries
each service's API on request and renders the result. There is no persistent
store, no history, and no writes to any upstream.

Two supported ways to run it:

- **Bootstrap module (recommended):** `sudo bash bootstrap/provider-box.sh
  --dashboard`. Also deployed by `--all` (last). This issues the cert, brings up
  the stack, verifies HTTPS, and publishes `CONTROL_PLANE_FQDN` via DNS. See
  "Bootstrap module" below.
- **Standalone / manual:** `services/control-plane/scripts/run.sh`, unchanged, for
  running the service on its own without the bootstrap flow. See "Running it
  manually" below.

This service absorbs the intent of the former design-stage `services/stepca-api`
(the "list issued certificates" panel); that directory has been removed and its
reusable step-ca reader lives here as `internal/certs`, now reading step-ca's
PostgreSQL backend (the BadgerDB reader was retired when step-ca moved to
postgres).

## What it shows

Five panels. Each degrades independently: if its source is down or not
configured it renders "unavailable" / "not configured" and never blanks the
page or fails the request.

1. **Certificates (step-ca)** - active certs with subject/SANs, provisioner,
   notBefore/notAfter and days-to-expiry, flagged against a warn threshold.
   Read from step-ca's dedicated PostgreSQL backend over `127.0.0.1` with a
   `SELECT`-only role, decoding the opaque cert blobs (see `STEPCA_STORAGE.md`).
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
  - step-ca: the postgres backend is read through a `SELECT`-only role on the
    cert tables; there is no signing path and no write path.
  - Docker socket is mounted `:ro`.
- **Tokens come from files/env, never hardcoded, never logged.** The compose
  file mounts them from `CONTROL_PLANE_SECRETS_DIR` read-only.
- **The dashboard serves HTTPS** with a step-ca-issued cert for its FQDN. If no
  cert is configured it falls back to plaintext HTTP with a logged warning
  (lab only).
- **No auth on the UI itself (v1).** This is acceptable only on a trusted,
  internal lab network. **TODO before any non-lab use: put the dashboard behind
  authentication** (e.g. the Authentik/Keycloak already in this repo, or a
  reverse proxy with auth). Until then, do not expose it beyond the lab.

## Bootstrap module

`bootstrap/dashboard.sh` (flag `--dashboard`) wires this service into
`provider-box.sh`. It does not rewrite the service - cert issuance and startup
reuse `scripts/issue-cert.sh` and `scripts/run.sh`, the same code the
manual path uses. The module follows the standard five-step flow:

1. **Validate** the `CONTROL_PLANE_*` variables and the CA variables (fail fast on
   an empty or malformed value).
2. **Create** `CONTROL_PLANE_CERT_DIR` and `CONTROL_PLANE_SECRETS_DIR` owned by uid 1000
   before anything writes to them.
3. **Issue** the TLS cert from step-ca as a full chain (leaf + intermediate) via
   `scripts/issue-cert.sh`.
4. **Start** the compose stack via `scripts/run.sh` (resolves the host docker
   gid, `--env-file provider-box.env`, `up -d --build`).
5. **Verify** `https://${CONTROL_PLANE_FQDN}:${port}/healthz` returns 200 over the
   step-ca chain (bounded poll, fail fast).

```sh
sudo bash bootstrap/provider-box.sh --ca
sudo bash bootstrap/provider-box.sh --technitium
sudo bash bootstrap/provider-box.sh --netbox
sudo bash bootstrap/provider-box.sh --dns-sync
sudo bash bootstrap/provider-box.sh --dashboard
sudo bash bootstrap/provider-box.sh --dashboard --remove
```

**DNS:** `--dashboard` (via `provider_box_builtin_fqdns`) publishes
`CONTROL_PLANE_FQDN -> HOST_IP`: `dns-sync` synthesizes the record on its next
pass, so `dashboard.<domain>` resolves by name after `--dns-sync`.

The scoped read-only tokens (below) are **optional** for the module - if absent,
the NetBox and Technitium panels render "not configured", so `--dashboard` and
`--all` stay green without them.

`--dashboard --remove` brings the container down and preserves the cert and
secrets directories, matching the other modules.

## Running it manually

The standalone path (`scripts/run.sh`) is unchanged and still supported for
running the service without the bootstrap flow.

Prerequisites: `--ca` deployed (for step-ca's postgres, the read-only role, and
the root cert), and the services you want panels for (`--technitium`,
`--netbox`, `--dns-sync`).

1. **Add the dashboard variables to your config.** Copy the `CONTROL_PLANE_*` block
   from `config/provider-box.env.example` into your `config/provider-box.env`
   and adjust. (`scripts/run.sh` resolves `CONTROL_PLANE_DOCKER_GID` from the host
   docker group automatically, so you can leave the example default.)

2. **Issue the dashboard's TLS cert** from step-ca. This mirrors the technitium
   module's cert-issuance docker run and writes `dashboard.crt` (leaf + chain)
   and `dashboard.key` into `CONTROL_PLANE_CERT_DIR`, owned by uid 1000:

   ```sh
   services/control-plane/scripts/issue-cert.sh
   ```

   (Pass a path as the first argument to use a non-default env file.) HTTPS is
   the default. If the cert is missing or unreadable at start, the server logs a
   WARNING and falls back to plaintext HTTP rather than crash-looping - fine for
   a lab, but issue the cert for real use.

3. **Provide the scoped read-only tokens** in `CONTROL_PLANE_SECRETS_DIR`
   (mode 0600, owner uid 1000). See "Creating the read-only tokens" below.
   - `technitium.token` - a scoped Technitium API token.
   - `netbox-readonly.token` - a dedicated NetBox read-only token.

4. **Start it:**

   ```sh
   services/control-plane/scripts/run.sh
   ```

   This runs the documented compose command
   (`docker compose --env-file ../../config/provider-box.env up -d --build`) with
   `CONTROL_PLANE_DOCKER_GID` resolved from the host docker group. Then browse
   `https://${CONTROL_PLANE_FQDN}:8445/` (the `CONTROL_PLANE_ADDR` port). To stop it:
   `services/control-plane/scripts/run.sh -- down`.

Any panel whose upstream URL/token/path is unset simply renders "not
configured", so you can run with a subset of sources.

### Creating the read-only tokens

Both panels are optional; without a token they render "not configured". Create
**dedicated, read-only** credentials - never reuse the dns-sync or bootstrap
admin tokens.

**NetBox (`netbox-readonly.token`).** In the NetBox UI as an admin:

1. Create a group (e.g. `dashboard-readonly`) and add an object permission that
   grants only the **view** action on `IPAM > Prefix` and `IPAM > IP address`
   (no add/change/delete). Assign the group that permission.
2. Create a service user (e.g. `dashboard`), add it to that group, and leave it
   without staff/superuser flags.
3. Under that user, create an API token with **Write enabled unchecked**
   (read-only). On NetBox 4.6 the token is the composite `nbt_<key>.<token>` -
   copy the full value.
4. Write it to the secret file:

   ```sh
   install -d -m 0700 "${CONTROL_PLANE_SECRETS_DIR}"
   printf '%s' 'nbt_...' > "${CONTROL_PLANE_SECRETS_DIR}/netbox-readonly.token"
   chmod 0600 "${CONTROL_PLANE_SECRETS_DIR}/netbox-readonly.token"
   chown 1000:1000 "${CONTROL_PLANE_SECRETS_DIR}/netbox-readonly.token"
   ```

The dashboard only ever GETs `ipam/prefixes` and `ipam/ip-addresses`, so a
view-only token on those two models is sufficient.

**Technitium (`technitium.token`).** Technitium's API has no per-scope tokens,
so create a **non-admin** user and use its token (the dashboard only calls
`zones/list`, `zones/records/get`, and `settings/get`):

1. In the Technitium console, add a user (e.g. `dashboard`) that is **not** in
   the `administrators` group.
2. Create a permanent API token for it (console, or
   `/api/user/createToken?user=<u>&pass=<p>&tokenName=dashboard`).
3. Write it to `${CONTROL_PLANE_SECRETS_DIR}/technitium.token` with the same
   `0600` / uid-1000 ownership as above.

## Configuration

All settings are environment variables (see the `CONTROL_PLANE_*` block in
`config/provider-box.env.example` for the documented set). The binary also
reads them directly, so it can run outside Docker for development:

```sh
CONTROL_PLANE_ADDR=:8445 \
CONTROL_PLANE_STEPCA_DSN='postgresql://dashboard_ro@127.0.0.1:5432/stepca?sslmode=disable' \
CONTROL_PLANE_STEPCA_PG_PASSWORD=... \
CONTROL_PLANE_TECHNITIUM_URL=https://dns.sddc.lab:53443 \
CONTROL_PLANE_TECHNITIUM_TOKEN=... \
go run ./cmd/dashboard
```

Without `CONTROL_PLANE_TLS_CERT`/`CONTROL_PLANE_TLS_KEY` it serves plaintext HTTP with a
warning - fine for local development, not for the lab network.

## Phase 2 (explicitly out of scope for v1)

- **History / collector.** v1 fetches on page load only; there is no background
  polling, time series, or store.
- **UI authentication.** Front the dashboard with the repo's IdP or a
  reverse-proxy auth layer before any non-lab exposure.

(Bootstrap integration - the `--dashboard` module and `--all` inclusion - has
landed; see "Bootstrap module" above.)

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

Each upstream client has a table-driven parsing test over recorded sample
payloads; the server has tests for per-panel isolation (source up, source down,
not configured).
