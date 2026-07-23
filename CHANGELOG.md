# Changelog

All notable changes to this project will be documented in this file.

---

## 2026-07-23 (Microsoft-CA web-enrollment emulator for VCF)

### Features
- New `internal/msca` package: a Microsoft ADCS "Certificate Authority Web Enrollment" (`certsrv`) emulator in front of step-ca, so VCF / SDDC Manager can automate certificate replacement using its Microsoft CA integration (step-ca offers no such interface natively). It serves the endpoints an ADCS web-enrollment client drives - `certfnsh.asp` (CSR submit), `certnew.cer` (issued leaf and CA cert), `certnew.p7b` (CA chain as certs-only PKCS#7), `certcarc.asp` (renewal count), and the `/certsrv/` credential probe - all behind HTTP Basic Auth. Each accepted CSR is signed synchronously through the existing `deploy.SignCSR` (same `admin` provisioner and full-chain guarantee as the dashboard's `/api/csr/sign`), so emulator-issued certs are identical to dashboard- and `IssueCert`-issued ones. The CA chain is emitted as a hand-built degenerate PKCS#7 (`internal/msca/pkcs7.go`, `encoding/asn1`, no new dependency; output verified with `openssl pkcs7 -print_certs`).
- The control plane starts the emulator as a second listener on `VMSCA_PORT` (default 8446; must not collide with other service ports - NETBOX_PORT is 8444) when `VMSCA_ENABLE=true`, reusing the control plane's own step-ca TLS leaf (so VCF reaches it at the control plane FQDN). The signer and CA-chain closures reload the managed config per request, so a CA deployed or reconfigured after startup is picked up without a restart. New config `VMSCA_ENABLE`, `VMSCA_PORT`, `VMSCA_USERNAME`, `VMSCA_PASSWORD`, `VMSCA_TEMPLATE` (validated under the `msca` pseudo-service). Point SDDC Manager's Certificate Authority at `https://<control-plane FQDN>:<VMSCA_PORT>/certsrv` with CA Type "Microsoft". See `vcf-msca-emulation_design.md` for the full contract, risks, and validation.

---

## 2026-07-22 (Zitadel uses the v4 Login V2 UI)

### Features
- Zitadel multi-tenant seeding via `ZITADEL_TENANTS`. When set to a comma-separated list of org names, the deployer creates each as an isolated organization - its own vcf-sso project, OIDC client, project role, and lab user - via the Management API scoped with the `x-zitadel-orgid` header, leaving the default org for instance administration only. Each tenant's generated client id/secret, issuer, and org login scope (`urn:zitadel:iam:org:id:<orgId>`) are written to `zitadel-oidc-<name>.txt`. Empty keeps the prior single-default-org behavior. Org creation is idempotent (lookup-before-create, since Zitadel org names are not unique). Note: the generated org domains are logical identifiers (login names / org discovery), not DNS records - all orgs share the one instance URL.

### Changes
- Log the Zitadel Console admin login name and URL at the end of a deploy. Zitadel appends the generated org domain to the configured admin username (e.g. `provider-admin` becomes `provider-admin@zitadel.<fqdn>`), which was otherwise only discoverable by querying the API; the deployer now fetches and prints it (best-effort).

### Fixes
- Make the Zitadel missing-PAT error actionable: it now explains that the PAT is written only during a first-instance init on an empty database and gives the exact recovery (stop the stack, remove the `postgres` and `machinekey` dirs under `ZITADEL_DIR`, redeploy), instead of just "check the server logs".
- Persist the Zitadel machine-user PATs under `ZITADEL_DIR` instead of the runtime dir. Zitadel writes the admin and login-client PATs only during first-instance init; they lived under `WORKDIR/zitadel/machinekey`, which `Remove` wipes while it preserves the database under `ZITADEL_DIR`. A Remove-then-deploy (or any runtime cleanup) left the DB present but the PATs gone, and init would not rewrite them - failing with `Zitadel did not write the machine-user PAT`. The PATs now live under `ZITADEL_DIR/machinekey`, sharing the database's persistence lifecycle.
- Preserve the port when proxying to Zitadel. The nginx terminator forwarded `Host $host`, which strips the port, so Zitadel built the OIDC issuer as `https://<fqdn>` (no port) and the Console failed to load `/.well-known/openid-configuration` (hit port 443, `status 0`). Forward `$http_host` (and `X-Forwarded-Host`) instead, and include the port in the login container's `CUSTOM_REQUEST_HEADERS` Host override, so public URLs carry `<fqdn>:<port>`.
- Set `CUSTOM_REQUEST_HEADERS=Host:<ZITADEL_FQDN>` on the Zitadel login container. It reaches the core over the internal service name (`http://zitadel:8080`), so Zitadel saw `Host: zitadel` and could not match the virtual instance (keyed on the external domain), failing every login-container call with `Instance not found` / `Errors.Instance.NotFound`. Overriding the Host header makes the instance lookup resolve.
- Harden the Zitadel nginx terminator for the Console: `http2 on`, `proxy_http_version 1.1` (required for the Angular Console's gRPC-web calls and chunked responses), a cleared `Connection` header, `X-Forwarded-Host`, and larger proxy buffers. The previous minimal config (copied from the netbox terminator) defaulted to HTTP/1.0 upstream, which broke the Console's browser API calls. `proxy_pass` (not `grpc_pass`) is kept on `/` so the REST/JSON endpoints the control plane provisions against keep working.
- Gate Zitadel provisioning on an authenticated Management API call (`GET /management/v1/orgs/me`) instead of only `/debug/ready`. `/debug/ready` is served by the HTTP layer, but the Management API proxies through Zitadel's internal grpc-gateway to its own gRPC backend over loopback, which can refuse the connection for a few seconds after readiness reports up - surfacing as `create Zitadel project: HTTP 503 ... dial tcp [::1]:8080: connect: connection refused` (gRPC code 14). The gate mirrors the Authentik deployer's two-phase readiness and raises a clear error (pointing at IPv6 loopback) if it persists.

### Changes
- Switched the Zitadel deployer from the bundled legacy login to Zitadel v4's decoupled Login V2. The stack is now four containers: PostgreSQL 17, the core server (plain HTTP, `--tlsMode external`), the `zitadel-login` container, and an nginx TLS terminator that serves the step-ca certificate and routes `/ui/v2/login` to the login container and everything else to the core. This keeps multi-tenant flows on the actively developed login (Login V1 is deprecated).
  - Core enables `ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_REQUIRED=true` and, on fresh install, auto-creates the `login-client` service account, writing its PAT to `WORKDIR/zitadel/machinekey/login-client.pat` (via `ZITADEL_FIRSTINSTANCE_LOGINCLIENTPATPATH`); the login container reads it through `ZITADEL_SERVICE_USER_TOKEN_FILE`.
  - New config: `ZITADEL_LOGIN_IMAGE` (`ghcr.io/zitadel/zitadel-login`, track `ZITADEL_IMAGE`) and `ZITADEL_NGINX_IMAGE`. New template `zitadel-nginx.conf.tpl` (mirrors the depot/netbox terminator pattern).

---

## 2026-07-22 (control plane deploys Zitadel as a third IdP)

### Features
- Added Zitadel as an identity-provider option in the control-plane deploy engine, alongside Keycloak and Authentik. Registering `deploy.Zitadel{}` surfaces it automatically in the `/deploy` UI and in "Select all" (dependency order now: chrony, rsyslog, ca, technitium, depot, keycloak, authentik, zitadel, netbox, s3, sftp, dns-sync). Scope is the Go control plane only; the legacy `bootstrap/*.sh` path is unchanged.
  - **zitadel**: Zitadel v4 core container plus a PostgreSQL 17 backend (v4 dropped CockroachDB), serving the step-ca-issued certificate directly (no self-signed bootstrap window). `ZITADEL_MASTERKEY` must be exactly 32 characters. v4 defaults new instances to the decoupled Login V2 container; the deploy keeps the bundled legacy login (`ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_REQUIRED=false`) so a single container serves the interactive sign-in flow. Zitadel's FirstInstance init mints a machine service account whose PAT is written to `WORKDIR/zitadel/machinekey/pat.txt`; the deployer reads it and drives the Management API to create the bootstrap project, an OIDC application with the configured redirect URIs, a project role, and a lab user granted that role (idempotent on re-runs). Because Zitadel generates the OIDC client id/secret on creation, the real issuer/client id/secret are written to `${ZITADEL_DIR}/certs/<ZITADEL_FQDN>/zitadel-oidc-client.txt` for the VCF SSO configuration.
- New files: `services/control-plane/internal/deploy/zitadel.go`, `.../templates/docker-compose.zitadel.yml.tpl`, and the render-parity golden. Wired into the schema validation table, the dashboard container filter, and the NetBox service/FQDN seeding; `config/labprovider.env.example` gains the `ZITADEL_*` block.

---

## 2026-07-11 (v2 documentation: control plane is the primary path)

### Fixes
- Fix the Technitium admin-password rotation in the Go deployer (`services/control-plane/internal/deploy/technitium_api.go`, `AdminToken`), which aborted a fresh deploy with `rotate the Technitium admin password: /api/user/changePassword: status error: Parameter 'newPass' missing.`. The `changePassword` call omitted `newPass` and passed the new password as `pass`; it now sends the current (first-boot) password as `pass=admin` and the configured value as `newPass`, matching Technitium's API. Added `AdminToken` unit tests covering the first-boot rotation (asserts `newPass`/`pass`) and the idempotent re-run (no `changePassword` when the configured password already works).

### Changes
- README leads with the v2 flow: `sudo bash install.sh`, then the web UI (`/config` wizard, `/deploy` with live progress, `/` dashboard). A required-open-ports table replaces the bootstrap's best-effort `ufw allow` calls. The bash bootstrap docs remain below under an explicit "Transitional" heading; the bash path is scheduled for removal once the control-plane path has proven parity end-to-end on a fresh host.
- AGENTS.md and PROJECT_CONTEXT.md are rewritten to the v2 model (closing IMPROVEMENTS #13/#14): fully containerized services, Go deployers under `services/control-plane/internal/deploy/`, the schema-table validation model, the deployer structure contract, and the updated filesystem/DNS/TLS rules.
- IMPROVEMENTS #1, #2 (fixed in the Go deployers) and #4 (fixed in install.sh) are marked resolved.

Remaining before the bash deletion (the final v2 step): end-to-end verification on a fresh Debian/Ubuntu host - install.sh, wizard, deploy all, the README checks - then delete `bootstrap/` and the legacy `templates/` directory.

---

## 2026-07-11 (control plane deploys depot, keycloak, authentik, sftpgo)

### Features
- The deploy engine now covers every service (Phase 5 of the v2 plan): Go ports of `bootstrap/depot.sh`, `bootstrap/keycloak.sh`, `bootstrap/authentik.sh`, and `bootstrap/sftp.sh`.
  - **depot**: PROD directory layout, step-ca cert, HTTP+HTTPS health waits; the managed `htpasswd` is generated with a native APR1-MD5 implementation - the apache2-utils host package is no longer needed.
  - **keycloak**: the bootstrap realm import (realm, group, VCF OIDC client, optional lab user) is built with encoding/json instead of the json_escape/heredoc templating; `keycloak-full-chain.pem` (leaf+intermediate+root) is still produced for the VCF SSO chain upload.
  - **authentik**: blueprint render, fullchain.pem/privkey.pem for certificate discovery (reuse check runs against the discovery names), HTTP + token-auth readiness gates, blueprint re-apply once the keypair is discovered, brand web-certificate PATCH, and the final verify that the served cert chains to step-ca.
  - **sftp**: HTTPS admin UI cert, data/home ownership, optional backup user via the SFTPGo API (created once, never modified after).
- With this, "deploy all" from the control plane UI covers the entire service catalog in dependency order: chrony, rsyslog, ca, technitium, depot, keycloak, authentik, netbox, s3, sftp, dns-sync.

---

## 2026-07-11 (control plane deploys the DNS chain: technitium, netbox, dns-sync)

### Features
- The deploy engine now covers the full DNS chain (Phase 4 of the v2 plan): Go ports of `bootstrap/technitium.sh`, `bootstrap/netbox.sh`, and `bootstrap/dns-sync.sh` with the same flows and data-preservation semantics. Highlights per deployer:
  - **technitium**: pre-pull-before-down (DNS never goes down against an uncached image), port-53 test-bind preflight, PKCS#12 bundle built natively with go-pkcs12 (replaces openssl), forwarder + web-TLS configuration over the settings API, dns-sync token minting, dashboard user/grants/token, and host resolv.conf point/restore via the mounted `/host/etc`. NEW: the first-boot `admin`/`admin` credentials are rotated to `TECHNITIUM_ADMIN_PASSWORD` (new required variable) on first deploy and used on re-runs - closes IMPROVEMENTS #1 (default credentials window, broken re-runs after a manual password change).
  - **netbox**: pepper resolution/persistence, API seeding with typed JSON payloads (site/manufacturer/device-type/role/device, canonical host IP, built-in service entries, dns.seed import), v2 Bearer token provisioning with legacy fallback, dns-sync + dashboard read-only token provisioning with description-tagged retirement. NEW: the per-run superuser seeding token is retired at the end of the deploy - closes IMPROVEMENTS #2 (leaked live superuser token per run).
  - **dns-sync**: image built from source baked into the control-plane image (no repo checkout needed on the host), dns.seed import via dns-seed, pinned readiness gates against NetBox and Technitium, then real-DNS verification that `LABPROVIDER_FQDN` and every built-in service FQDN resolve via Technitium.
- The config wizard now also manages `dns.seed` (edit/validate/save, saved next to the managed config); the netbox and dns-sync deployers read the same managed copy.
- Deploying "all" from the UI now includes dns-sync automatically in the right order - the old "run --dns-sync after --all" caveat is gone on the control-plane path.

---

## 2026-07-11 (control plane deploys step-ca)

### Features
- The deploy engine now deploys step-ca with its dedicated PostgreSQL backend (Phase 3 of the v2 plan): a full Go port of `bootstrap/ca.sh` in `services/control-plane/internal/deploy/ca.go`. Password-file materialization (existing file / `CA_PASSWORD` / generated), the `.pgpass` writer with `:`/`\` escaping, uid-70 postgres dir prep, the badger-to-postgres `ca.json` rewrite (encoding/json replaces jq), the fresh-root-vs-nonempty-store guard, the corrupted-root_ca.crt guard, CRL enablement, provisioner duration configuration, and the read-only role provisioning (now over pgx to the loopback-published port instead of psql-in-container) all behave as in the bash module.
- Shared step-ca helpers for every certificate consumer (`internal/deploy/stepca.go`): `IssueCert` (step CLI container, full-chain guarantee, cert-identity reuse), `WaitHTTPSPinned` (Go equivalent of `curl --resolve fqdn:port:127.0.0.1 --cacert root_ca.crt`), and a native x509 port of `certificate_matches_dns_identity`.
- After deploying the CA, the engine issues the control plane's own leaf certificate; `install.sh` points `CONTROL_PLANE_TLS_CERT`/`_KEY` at it, so a container restart upgrades the UI from HTTP to HTTPS.

---

## 2026-07-11 (control plane deploy engine, config wizard, install.sh)

### Features
- The control plane now carries a deploy engine (Phase 2 of the v2 plan): a static service registry with explicit dependencies, executed sequentially in dependency order, with per-run progress streamed over SSE. New routes: `/config` (wizard), `/deploy` (service selection + live log), `GET/PUT /api/config`, `POST /api/config/validate`, `GET /api/services`, `POST /api/deploy`, `GET /api/deploys/{id}/events`. The engine is enabled when the image carries the example config; the legacy `--dashboard` compose deployment stays a read-only dashboard.
- Config wizard: edit or paste `labprovider.env` in the browser, download it, validate (all findings at once, per-variable), and save. The managed copy lives at `/opt/labprovider/control-plane/labprovider.env`; deploys always re-read and re-validate it. Saving is blocked only when variables defined in the example are missing.
- First three deployers, ported/new in Go (`services/control-plane/internal/deploy`): `s3` (port of `bootstrap/s3.sh`), `chrony`, and `rsyslog` - the latter two are now CONTAINERIZED (host networking; chrony gets only `cap_add: SYS_TIME`; rsyslog config is `rsyslogd -N1`-validated before start). Their images are built locally by the engine from embedded Dockerfiles (`CHRONY_IMAGE`, `RSYSLOG_IMAGE`; alpine + chrony/rsyslog), no registry needed. The bash `--ntp`/`--rsyslog` host-native path is unchanged until cutover.
- `install.sh` at the repo root: the only shell in the v2 model. Installs Docker if absent (Debian AND Ubuntu repos - fixes IMPROVEMENTS #4), does the one-time host prep (disables the systemd-resolved stub listener with a marked drop-in, disables systemd-timesyncd, creates `/opt/labprovider`), builds the control-plane image from the checkout, and runs it root + host-network with the docker socket, `/opt/labprovider`, and `/host/etc` mounted. Prints the UI URL. No auth on the UI - trusted lab networks only.
- Templates are rendered with Go text/template (`{{.VAR}}`) instead of envsubst, embedded in the binary. A missing variable now fails the render instead of silently producing an empty string. Golden-file parity tests pin the converted templates against the envsubst output of the originals.

### Changes
- `SYSLOG_LOG_DIR` default moves to `/opt/labprovider/syslog/logs` (was `/var/log/labprovider`) so the containerized rsyslog and control plane share the `/opt/labprovider` mount. New `CHRONY_DIR` (drift data) and `CHRONY_IMAGE`/`RSYSLOG_IMAGE` variables.
- The control-plane image is now alpine-based (was scratch) and bundles the pinned docker CLI + compose/buildx plugins; the build context is the repo root so the image carries `labprovider.env.example` for the wizard.

---

## 2026-07-10 (rename services/dashboard to services/control-plane)

### Changes
- `services/dashboard` is renamed to `services/control-plane` (git mv; Phase 1 of the labprovider v2 plan). The service is unchanged functionally - it is still the read-only "current state" dashboard - but it is the foundation the v2 deploy engine and web UI build on.
- Go module path is now `github.com/dsjodin/labprovider/services/control-plane`; the binary and image are `control-plane` (`CONTROL_PLANE_IMAGE="labprovider/control-plane:0.1.0"`).
- Every `DASHBOARD_*` variable in `labprovider.env` is renamed to `CONTROL_PLANE_*` (same meanings and defaults; default cert/secrets paths move to `/opt/labprovider/control-plane/...`). `DNS_SYNC_TECHNITIUM_DASHBOARD_USER` and the read-only `dashboard` service accounts in Technitium/NetBox keep their names.
- `scripts/issue-dashboard-cert.sh` is renamed to `scripts/issue-cert.sh`; the issued leaf is now `control-plane.crt`/`control-plane.key` (a redeploy reissues it).
- The `--dashboard` bootstrap flag is unchanged and now deploys the renamed service.

---

## 2026-07-10 (remove unbound; Technitium is the only DNS backend)

### Removed
- The host-based Unbound DNS backend is removed entirely: `bootstrap/dns.sh`, `templates/unbound.conf.tpl`, `config/unbound.records.example`, the `--unbound` flag, and the `DNS_BACKEND` backend switch are gone. Technitium (with NetBox and dns-sync) is the only DNS path; `--all` always deploys Technitium right after `--ca`.
- `UNBOUND_FORWARDER` is removed from the env model. `DNS_FORWARDER` is now required (no fallback). `DNS_BACKEND` is removed; `--technitium` and `--dns-sync` no longer check it.

### Changes
- External/custom DNS records now live only in `config/dns.seed` (same `<fqdn> <ip[/cidr]>` format as the removed `config/unbound.records`). `--netbox` imports `config/dns.seed` when the file exists; record-source metadata in NetBox descriptions says `dns.seed`.
- Dead code dropped from the dispatcher with the backend switch: `unbound_pkgs`, `configure_resolv_conf` (the unconditional resolver takeover with no restore path), `build_dns_record_block`, `build_labprovider_dns_block`, `require_records_file`, `validate_dns_backend`, and `require_dns_backend`.

This is Phase 0 of the labprovider v2 plan (fully containerized services plus a Go control plane replacing the bash bootstrap).

---

## 2026-07-10 (dashboard token auto-provisioning)

### Features
- Auto-provision the dashboard's read-only upstream tokens at deploy time, mirroring the existing `provision_dns_sync_netbox_token` pattern (validity-probe idempotency, producer-side, skip-if-not-configured). No manual token placement is needed for the NetBox and Technitium panels to light up on a fresh `--netbox`/`--technitium`/`--dashboard` cycle.
- `--netbox` (`bootstrap/netbox.sh`) now also creates the dashboard's minimum-read NetBox path: a `dashboard-readonly` group with a view-only object permission scoped to IPAM `Prefix` and `IP address` (the only two models the panel reads), a non-privileged service user (`dashboard`, no staff/superuser) in that group, and a READ-ONLY token (`write_enabled=false`, composite `nbt_<key>.<token>`). The token is written to `${DASHBOARD_SECRETS_DIR}/netbox-readonly.token` (`0600`, uid `1000`). Idempotent by an IPAM-read validity probe. Skipped with a `NOTICE` when `DASHBOARD_SECRETS_DIR` is unset so `--netbox` stays standalone-deployable.
- `--technitium` (`bootstrap/technitium.sh`) now creates a non-admin `dashboard` user, grants it read access to the panel's data (`Settings: View` for `settings/get`, plus per-zone `View` on every existing zone), then mints a permanent API token (`admin/sessions/createToken`) carrying that user's limited privileges. Written to `${DASHBOARD_SECRETS_DIR}/technitium.token`, same ownership/idempotency/skip rules (probe: `settings/get`).
- Operator-placed tokens still win: when a token file already exists and validates, it is reused and never overwritten, so a manual/SOPS/age override is respected.
- `dns-sync` grants the dashboard Technitium user `View` on each zone it newly creates, so continuously-synced zones appear in the dashboard without a `--technitium` re-run (the bootstrap grant covers pre-existing zones; this covers ones created later). The grant uses the same `zones/permissions/set` shape as `provision_technitium_dashboard_token` (only `userPermissions`, group access untouched) and fires only on fresh creation, not when the zone already exists. It is best-effort and non-fatal: an empty `DNS_SYNC_TECHNITIUM_DASHBOARD_USER` skips it entirely, a missing dashboard user is silently ignored by Technitium, and any grant error is logged and swallowed so zone creation and reconcile never break. Configured via the new `DNS_SYNC_TECHNITIUM_DASHBOARD_USER` (default `dashboard`, plumbed through the compose template).

### Fixes
- Grant the Technitium dashboard user explicit per-zone `View` permission so its `zones/list` returns the lab zones. The earlier assumption that the built-in `Everyone` group's section-level `Zones: View` makes zones visible to a non-admin was wrong (verified live: the dashboard token got `status: ok` but an empty zone list). Technitium filters `zones/list` by BOTH the `Zones` section permission AND each zone's own per-zone permission, and a zone is created with per-zone access for its creator plus the `Administrators`/`DNS Administrators` groups only - never `Everyone` (confirmed in the pinned image source, `WebServiceZonesApi.CreateZoneAsync`). `provision_technitium_dashboard_token` now enumerates existing (non-internal) zones and grants the dashboard user `View` on each via `zones/permissions/set`, sending only `userPermissions` (the API syncs the user and group tables independently, so the zone's `Administrators`/`DNS Administrators` group access is left untouched). The read grants are applied on every run before the token reuse check, so re-running `--technitium` picks up zones created since (e.g. by `dns-sync`) and a reused token still gets access.
- Generate the dashboard service-user password to satisfy NetBox 4.6's default password validators. An all-lowercase-hex password (`openssl rand -hex`) failed `utilities.password_validation.AlphanumericPasswordValidator` with `400 "Password must have at least one uppercase letter."`, aborting `--netbox`. The password is now `openssl rand -base64` (>= 12 chars for `MinimumLengthValidator`) with a fixed `Aa1!` suffix guaranteeing the required digit/uppercase/lowercase classes; the Technitium dashboard-user password uses the same generator for consistency (Technitium enforces no complexity policy). The password is never stored - the dashboard authenticates with its token, not this password.

### Changes
- Refactor the NetBox token housekeeping (retire previous labprovider tokens by description) into `netbox_retire_tokens_by_description`, now shared by the dns-sync and dashboard token provisioners.
- `--dashboard` now reports the tokens as auto-provisioned by the producing modules (with the operator-override note) instead of describing them as manual-only; the same clarification is applied to `config/labprovider.env.example`. No dashboard service code change was needed: `DASHBOARD_NETBOX_URL`/`DASHBOARD_TECHNITIUM_URL` and the token-file paths were already wired, and the panels already degrade to "not configured" when a token is missing or invalid.

---

## 2026-07-10

### Fixes
- Fix the `step-ca` container reporting `unhealthy` while the CA is fine. `templates/docker-compose.step-ca.yml.tpl` had no healthcheck, so Compose ran the image's default `step ca health` with no args, which resolves `CA_FQDN` through Docker's embedded resolver (`127.0.0.11`) - unable to answer for the lab zone - and fails. The template now pins `CA_FQDN` to the local listener via `extra_hosts` (`${CA_FQDN}:127.0.0.1`, the same `--add-host` idiom the service modules use) and sets an explicit healthcheck: `step ca health --ca-url https://${CA_FQDN}:9000 --root /home/step/certs/root_ca.crt | grep '^ok'`. This resolves locally and verifies against the matching cert SAN (`CA_FQDN`), where `localhost` would fail SAN verification. No cert/SAN change. Verified end-to-end: the container transitions to `healthy` (healthcheck exits 0, output `ok`).

---

## 2026-07-09 (step-ca PostgreSQL backend)

### Features
- Move step-ca from BadgerDB to a DEDICATED PostgreSQL backend (`stepca-postgres`), never shared with NetBox/Authentik (module independence, CA isolation). `templates/docker-compose.step-ca.yml.tpl` gains a `stepca-postgres` service (healthcheck-gated; step-ca `depends_on` it healthy) published on `127.0.0.1:${CA_POSTGRES_PORT}` only. New `CA_POSTGRES_*` variables mirror `NETBOX_POSTGRES_*`; `CA_POSTGRES_DATA_DIR` is a sibling of `CA_DATA_DIR` (never nested - the `chown -R 1000:1000 CA_DATA_DIR` step would corrupt postgres data). Every new bind-mount uses the `${VAR:?...}` blank-mount guard.
- Supply the postgres password to step-ca via a mounted `.pgpass` file (`PGPASSFILE`), so it never appears in `ca.json`'s `dataSource` DSN - matching the file-based `CA_PASSWORD_FILE` convention. step-ca's pgx driver reads it.
- `--ca` rewrites `ca.json` after the container self-initializes: `db` -> `postgresql`, and `crl.enabled` on (with `generateOnRevoke`). The remote admin API is NOT enabled and there is no write/revoke path. The unused badger `db/` dir is moved aside (`db.pre-postgres.<timestamp>`, retained, not deleted), not removed.
- `--ca` provisions a read-only postgres role (`CA_POSTGRES_RO_USER`) with `SELECT` on the cert tables only (`x509_certs`, `x509_certs_data`, `revoked_x509_certs`), which the dashboard reads through.
- Guards: `--ca` refuses to run against an existing badger-backed CA (Phase 2 rebuilds on postgres, it does not migrate in place), and refuses to bind a freshly initialized root onto a postgres store that already holds cert rows.
- Repoint the dashboard Certificates panel at step-ca's postgres via `github.com/jackc/pgx/v5`, reusing the existing DER/JSON blob decode. `--dashboard` materializes the RO password into `DASHBOARD_SECRETS_DIR`. Because the table IS the bucket on postgres, `nkey` is the plain decimal serial and the reader `SELECT`s per table instead of prefix-scanning badger's binary keys.

### Removals
- Remove BadgerDB from the dashboard entirely: the `github.com/dgraph-io/badger/v3` dependency and pin, the snapshot-copy code, the `db/` bind mount, `DASHBOARD_STEPCA_DB`/`DASHBOARD_STEPCA_SNAPSHOT_DIR`, and `badger_fixture_test.go`. Replaced by `DASHBOARD_STEPCA_DSN` + `DASHBOARD_STEPCA_PG_PASSWORD_FILE`. Resolves IMPROVEMENTS.md #16 (the badger major-version coupling no longer exists).

### Security
- The CA postgres port is loopback-only (`127.0.0.1`), for the host-networked dashboard; never exposed off-host. The dashboard holds only a `SELECT`-scoped role, so its blast radius stays "can read cert metadata".
- Evaluated `damhau/stepca-web` as the admin console and REJECTED it (recorded in IMPROVEMENTS.md #17): its access footprint (DB creds + remote admin API + cert-issuing JWK key + `ca.json` write + systemd control) makes a compromise a full CA compromise, it is single-maintainer/low-adoption in the trust path, and it assumes a systemd host. Chose the existing read-only dashboard panel instead.

### Notes
- Needs on-host confirmation (a sandbox cannot cover): the reissue chain against the new root; that the pinned image's pgx honors `PGPASSFILE` at runtime; PG TLS posture (`sslmode=disable` is loopback-only today); and the CRL endpoint path. Docker was unavailable in the build sandbox, so the live postgres reader test is skip-gated behind `STEPCA_TEST_PG_DSN`; the decode path is covered by unit tests.

---

## 2026-07-09

### Features
- Promote the dashboard to a first-class bootstrap module: `bootstrap/dashboard.sh` with the `--dashboard` flag (and `--dashboard --remove`), following the standard five-step flow (validate `DASHBOARD_*`/CA vars, create the cert and secrets dirs owned by uid 1000, issue the cert, start the stack, verify HTTPS). Cert issuance and startup reuse the service's own `scripts/issue-dashboard-cert.sh` and `scripts/run.sh` rather than duplicating them; the standalone `run.sh` path is unchanged.
- Publish `DASHBOARD_FQDN` through `labprovider_builtin_fqdns`, so both DNS backends resolve `dashboard.<domain>` to the host IP (unbound renders it directly; technitium via `dns-sync` on its next pass). This makes the dashboard reachable by name.
- Issue the dashboard cert as an explicit full chain (leaf + step-ca intermediate) so the served certificate validates against the step-ca root on its own; `issue-dashboard-cert.sh` now appends the intermediate if `step ca certificate` returned a leaf-only file.
- Include `--dashboard` in `--all` (last, after the services it reads) and in `--all --remove` (first). Its scoped upstream tokens are optional - the NetBox/Technitium panels degrade to "not configured" - so `--all` stays coherent when they are unset. Add a `--dashboard` row to the module reference and update the env example, dashboard README (both the module and the still-supported standalone path), and IMPROVEMENTS.md #7.

### Fixes
- Prevent unset variable-sourced bind-mount paths from corrupting data. When a variable used as a Docker bind-mount source is empty at compose time, `${VAR}/certs/root_ca.crt` collapses to `/certs/root_ca.crt` and Docker auto-creates the missing source as a DIRECTORY - which once turned step-ca's `root_ca.crt` into a directory and destroyed a running trust root ("file is a directory", init loop). Two guards now cover every variable-sourced mount:
  - Defense in depth in every compose template and the standalone `services/dashboard/docker-compose.yml`: each mount whose source begins with a variable now uses compose's mandatory-variable syntax `${VAR:?VAR must be set (empty would create a blank bind-mount source)}`. `envsubst` passes the guard through rendering unchanged, so an empty value aborts `docker compose` with a named error instead of mounting a blank path (32 mounts across 10 files).
  - Script-side validation before every compose call. All bootstrap modules already validated their mount-source variables via the `require_*_vars` helpers (audited: `CA_DATA_DIR`, `WORKDIR`, `TECHNITIUM_*_DIR`, `NETBOX_*_DIR`, `DEPOT_*_DIR`, `KEYCLOAK_DIR`, `AUTHENTIK_DIR`, `S3_DATA_DIR`, `SFTP_*_DIR`, `DNS_SYNC_SECRETS_DIR` are all covered). The standalone dashboard launcher `services/dashboard/scripts/run.sh` had none; it now validates `CA_DATA_DIR`, `DASHBOARD_CERT_DIR`, and `DASHBOARD_SECRETS_DIR` (non-empty, absolute) before running `docker compose`.
- Add a step-ca corruption guard: `--ca` now refuses to start if `${CA_DATA_DIR}/certs/root_ca.crt` exists as a directory (or any non-regular file), explaining the likely cause (a prior compose run with an empty `CA_DATA_DIR`) and the fix (remove it and restore/re-initialize), so the corruption fails loudly instead of silently breaking init.
- Behavior note: rendered compose files now retain the `${VAR:?...}` mount guards rather than a pre-substituted absolute path, so running `docker compose` on a rendered file manually requires the same environment sourced (the bootstrap modules already source it before every compose call).

### Features
- Add `services/dashboard`: a standalone, read-only "current state" view of the labprovider services (single Go binary serving an HTML page + JSON API, std-lib HTTP, embedded template - matches the existing Go services and needs no frontend framework). Five panels, each isolated behind its own short timeout so a dead or unconfigured source renders "unavailable"/"not configured" without blanking the page: Certificates (step-ca), DNS (Technitium), IPAM (NetBox), Services (Docker), and Recent errors (container log tail)
- Absorb the design-stage `services/stepca-api` into the dashboard: its reusable step-ca BadgerDB reader is migrated to `services/dashboard/internal/certs` as the Certificates panel (active certs, subjects/SANs, provisioner, notBefore/notAfter, days-to-expiry against a warn threshold); the phase-2 collector parts (SQLite inventory, reconcile loop, token-authed REST API) were dropped as out of v1 scope. `services/stepca-api/` is removed
- Read paths reuse the shapes the repo already verified: the DNS panel calls the same Technitium endpoints as `dns-sync` (`zones/list`, `zones/records/get`, `settings/get`), the IPAM panel reads NetBox IPAM, and both honor the v1/v2 token formats
- Serve HTTPS with a step-ca-issued cert for `DASHBOARD_FQDN`; fall back to plaintext HTTP with a logged warning when no cert is configured (lab only)

### Security
- Read-only throughout: no upstream write path exists. The dashboard expects a dedicated minimum-read-scope NetBox token (not the dns-sync/bootstrap admin token), a scoped Technitium token, the step-ca DB via a read-only snapshot copy (never opened directly), and the Docker socket mounted `:ro`. All tokens come from files/env, never hardcoded or logged
- v1 ships with no authentication on the UI itself - acceptable only on a trusted internal lab network. Documented as an explicit assumption with a TODO to front it with auth before any non-lab use

### Notes
- The dashboard is run manually and is NOT wired into `bootstrap/labprovider.sh` or `--all`. A `--dashboard` bootstrap module, a history/collector, and UI auth are the documented phase 2. Run it with the standalone `services/dashboard/docker-compose.yml`; add the `DASHBOARD_*` block from `config/labprovider.env.example` to your `config/labprovider.env`

### Dashboard follow-ups (first-deploy fixes)
- Fix the HTTP fallback: with a `DASHBOARD_TLS_CERT` path set but the cert file missing/unreadable, the server crash-looped on bind instead of falling back. It now validates the cert/key (loads the keypair) before binding, logs a WARNING and serves plaintext HTTP when it cannot, and reports the mode actually chosen in the startup log's `tls` field
- Add `services/dashboard/scripts/issue-dashboard-cert.sh`: issues the dashboard's TLS leaf cert from step-ca (mirrors the technitium cert-issuance docker run - `--add-host ca:127.0.0.1`, admin provisioner + password file, `--not-after SERVICE_CERT_DURATION`, bundled leaf+chain into `DASHBOARD_CERT_DIR` owned by uid 1000), so HTTPS works on a clean deploy. The cert dir is chowned to uid 1000 BEFORE the run (not only after), so the uid-1000 step-cli container can write into a freshly created root-owned `DASHBOARD_CERT_DIR` instead of failing "permission denied" on a first run
- Add `services/dashboard/scripts/run.sh`: one correct launch path that runs the documented compose command (`--env-file ../../config/labprovider.env`) with `DASHBOARD_DOCKER_GID` resolved from the host docker group, avoiding the all-blank-vars / invalid-mount failure of a bare `docker compose up`
- Document creating the dedicated read-only NetBox and Technitium tokens (view-only IPAM object permission + a read-only API token for NetBox; a non-admin user's token for Technitium) so the DNS/IPAM panels can be enabled without reusing an admin token
- Downgrade the Certificates panel's BadgerDB dependency from `badger/v4` to `badger/v3` (v3.2103.5) to match step-ca 0.30.2's on-disk format: smallstep CLI 0.30.2 writes a manifest v7 database (badger/v3); a v4 engine (manifest v8) opening it refuses or migrates it, so on a real lab DB the read would fail and blank the panel. The read still opens only a read-only snapshot copy (`WithReadOnly(true)`), so nothing can migrate the live DB regardless. Adds a badger-v3 fixture test that writes a v7-format DB in step-ca's key encoding and reads it back, and an IMPROVEMENTS.md note on the step-ca/badger major-version coupling

## 2026-07-08

### Fixes
- Fix the Technitium redeploy/upgrade path taking DNS down against an un-cached image: when Technitium is the host resolver (`resolv.conf` -> `127.0.0.1`), stopping the container before the new image was cached made `docker pull` fail because `registry-1.docker.io` could no longer resolve. `--technitium` now pre-pulls the pinned image before `compose down`, and aborts without stopping the running server if the pull fails. Applies to every re-run, not just version bumps

### Improvements
- Upgrade Technitium DNS from `13.4.2` to `15.3.0` (the release reviewed for this upgrade, on 2026-07-08 - baseline for the next drift check). The upgrade was assessed against the upstream changelog and verified live: the web service TLS settings API, `createToken`, forwarder settings, and zone/record CRUD are unchanged or backward-compatible (the query-string token form still works on 15.x alongside the new Bearer header), ports and the container uid are unchanged, and a 13.x data directory migrates in place on first 15.x start (zones, records, and API tokens preserved). No bootstrap, compose-template, or dns-sync code changes were required
- Document the upgrade/rollback procedure in a new README "Upgrading Services" section, including the forward-only warning that a 15.x data directory must not be run under 13.x afterward
- Record the 15.x API deltas in `services/dns-sync/TECHNITIUM_API.md`: built-in `internal` zones no longer appear in `zones/list` (the `internal` field is absent, the client filter is retained for mixed-version safety), and deleting a non-existent zone/record now returns an error instead of succeeding - which makes the dns-sync invariant "only delete records that List reported" load-bearing for correctness on 15.x
### Documentation
- Documentation reconciliation pass: audit all documentation against the code on main and fix drift, with no behavior changes
- Document both DNS backends end to end: backend module chains (unbound + `unbound.records` rendering vs technitium + dns-sync + NetBox seeding), the one-backend-per-host rule, Technitium bootstrap details (HTTPS console on the step-ca PKCS#12 bundle, persisted pfx password, auto-provisioned API token, systemd-resolved stub-listener handling and what `--remove` restores), and dns-sync (host networking, built-in service records synthesized from the `*_FQDN` variables, token auto-provisioning with SOPS/age override)
- Correct the quickstart: add `--technitium`/`--dns-sync` with explicit ordering rules (`--ca` before certificate consumers, `--technitium` and `--netbox` before `--dns-sync`), mark the `unbound.records`/`dns.seed` copies optional, and state what `--all` does and does not deploy (the usage note wrongly claimed `--technitium` is never part of `--all`)
- Document NetBox 4.6 token handling: the persisted API token pepper and the consequence of changing it, the v2 composite `nbt_<key>.<token>` Bearer format, and the auto-provisioned dns-sync token
- Document the idempotent Docker install behavior (preserve existing Docker, Compose v2 capability check, Docker CE fallback and its Debian assumption), replacing the stale `docker-compose` package claim
- Document that certificate issuance is DNS-independent via 127.0.0.1 pinning under the single-node assumption
- Add a module reference table (flag, purpose, dependencies, data dirs, ports, secrets, `--remove` semantics), a secrets inventory (path, owner/mode, loss/regeneration consequences), and a troubleshooting section seeded from real failure modes
- Fix `config/unbound.records.example`: the five `pod-240-supervisorN` entries lacked a domain suffix and failed FQDN validation when the example was copied verbatim
- Correct the `DNS_SYNC_SECRETS_DIR` comment and the missing-token fail message: both dns-sync tokens are auto-provisioned (`--netbox`/`--technitium`); manual SOPS/age placement is the override, not the default procedure
- Update the repository layout (technitium/dns-sync modules and templates, `dns.seed.example`, `keycloak-realm.json.tpl`, `services/`, `docs/`) and the runtime model and removal sections for the container-based DNS chain
- Add `IMPROVEMENTS.md` with verified non-documentation findings from the audit

## 2026-07-06

### Features
- Add Authentik identity provider service module for VCF 9 identity federation with OIDC authentication and outbound SCIM 2.0 provisioning
- Seed an opinionated Authentik bootstrap blueprint with one group, one lab user, one OIDC provider, and one VCF application
- Set the Authentik brand web certificate to the discovered step-ca keypair after startup
- Add explicit DNS backend selection via `DNS_BACKEND` (`unbound` or `technitium`, default `unbound`): `--all` deploys only the selected backend, direct backend flags fail fast on a mismatch so the second DNS server cannot land on a converted host, and `--dns-sync` requires the technitium backend
- Make `config/unbound.records` optional for NetBox: when absent, the import is skipped with a notice instead of failing (the file remains required by the unbound backend, which renders it)
- Add `DNS_FORWARDER` as the shared upstream forwarder for both DNS backends (falls back to `UNBOUND_FORWARDER` when unset); the technitium backend now configures its upstream forwarder over the settings API on bootstrap and verifies external resolution before pointing the host at itself, so Technitium can act as the sole nameserver for the host and lab
- Be sure to update your `config/labprovider.env` (new `DNS_BACKEND` and `DNS_FORWARDER` variables).

### Fixes
- Fix `--authentik` failing intermittently on re-run at the brand step with "Failed to determine the default Authentik brand": Authentik rejects even valid API tokens for a few seconds after container start, and `/-/health/ready/` does not imply token-auth readiness. A new authenticated readiness gate polls `GET /api/v3/core/brands/` with the Bearer token (bounded, 401/403 retried within the window) before any brand or blueprint configuration runs, with a timeout message that distinguishes a persistent-data token mismatch from a transient startup delay. Per-attempt curl stderr is also suppressed in the HTTP readiness wait and the certificate verification poll, which previously printed normal "connection reset" and "self-signed certificate" retries as error noise
- Fix built-in labprovider service FQDNs (netbox, ca, dns, auth, idp, ...) never reaching the Technitium zone: dns-sync now synthesizes their A records from `labprovider.env` on every reconcile, using the same built-in list as the unbound backend (`labprovider_builtin_fqdns`, which now also covers `AUTHENTIK_FQDN` and skips unset services); they are deliberately not seeded into NetBox because the pinned NetBox enforces global IP uniqueness (verified live: duplicate host-IP objects are rejected without anycast roles), and A-only synthesis keeps `LABPROVIDER_FQDN` as the sole PTR target; the post-deploy zone verification now checks every built-in FQDN resolves via Technitium
- Fix the long-running dns-sync container failing every reconcile with "connection refused": the `127.0.0.1` extra_hosts pins pointed at the container's own loopback on the default bridge network; the service now runs with `network_mode: host`, matching the module's one-shot docker runs
- Remove the Technitium forwarder step from `--dns-sync`, which silently overwrote the `DNS_FORWARDER` value the technitium module sets and verifies (one owner per setting); the unused `TECHNITIUM_FORWARDER` variable is removed from the example env
- Include the NetBox response body (about 200 characters) in dns-sync client errors on non-2xx responses so failures like 403 are diagnosable from logs
- Auto-provision the dns-sync NetBox token during `--netbox`, mirroring the technitium module's token handling: a stored token at `DNS_SYNC_SECRETS_DIR/netbox.token` is validated live and reused (operator-placed tokens win while valid); otherwise a dedicated token with description "labprovider dns-sync" is provisioned, previous tokens with that description are retired, the composite is validated with one authenticated request, and it is written with the same ownership/permission conventions; skipped with a notice when `DNS_SYNC_SECRETS_DIR` is unset so `--netbox` stays standalone
- Fix Technitium HTTPS never being enabled, which broke the `--dns-sync` HTTPS gate: `--technitium` now converts the step-ca certificate to PKCS#12 (with a generated, persisted bundle password), enables the web service TLS listener via the settings API (`webServiceEnableTls`/`webServiceTlsPort`/`webServiceTlsCertificatePath`/`webServiceTlsCertificatePassword`, verified against 13.4.2), and confirms `https://<DNS_FQDN>:<TECHNITIUM_HTTPS_PORT>` serves the step-ca chain before finishing
- Replace the "capture the API token from the console" manual step: `--technitium` now creates a non-expiring API token via `/api/user/createToken` and stores it at `DNS_SYNC_SECRETS_DIR/technitium.token` (0600, container uid) where `--dns-sync` expects it, reusing a stored token as long as Technitium still accepts it
- Fix `--dns-sync` failing at its reachability gates with "Could not resolve host" when the lab zone is not resolvable yet - which is always the case, since dns-sync itself populates the zone: the NetBox and Technitium gates now pin the lab FQDNs to `127.0.0.1` with `curl --resolve`, the one-shot dns-seed containers and the long-running dns-sync container get the same pinning via `--add-host`/`extra_hosts` (Go resolves through `/etc/hosts`, TLS verification stays full against the lab root), and a new post-sync check confirms over real DNS that the zone is actually served after the first reconcile
- Fix NetBox API authentication against NetBox 4.6 v2 tokens, which reject the legacy `Token` header with 403 "Invalid v1 token": the seeding helpers now build `Authorization: Bearer nbt_<key>.<token>` from both fields of the provision response (falling back to the legacy header when no `token` field exists), validate the header with an authenticated request immediately after provisioning, and dns-sync/dns-seed accept the stored composite `nbt_<key>.<token>` and send it as a Bearer credential
- Fix NetBox 4.6 API token provisioning failing with HTTP 500 "API_TOKEN_PEPPERS is not defined" by generating and injecting the required `API_TOKEN_PEPPER_1` (>= 50 chars); the pepper is persisted under `NETBOX_DIR/secrets` and reused on every redeploy so existing API tokens (including the dns-sync token) stay valid, and can be seeded explicitly with the optional `NETBOX_API_TOKEN_PEPPER` variable
- Fix certificate issuance failing when the lab DNS zone does not resolve yet (for example while bootstrapping the DNS service itself) by pinning `CA_FQDN` to `127.0.0.1` via `--add-host` in the step-ca certificate issuance containers across all six certificate-consuming service modules, making issuance DNS-independent per the single-node design where step-ca always runs on the same host
- Fix Docker Compose installation on Ubuntu, where apt resolves the transitional `docker-compose` package name to `docker-compose-v2` and the dpkg-based post-install check on the literal name failed; install `docker-compose-v2` explicitly (Ubuntu 22.04+) and verify compose by capability (`docker compose version`) instead of by package name
- Fix step-ca crash-looping on first start caused by root-owned `CA_DATA_DIR` and secrets directory that the container user (uid 1000) could not read; the ca module now owns the CA data directory recursively as 1000:1000 while keeping restrictive modes (secrets 0700, password file 0600), matching the ownership handling of the other step-ca-dependent modules
- Replace the misleading "step-ca configuration was not created" error with a bounded initialization wait that polls for `config/ca.json` and then for the CA health endpoint, failing with an actionable message pointing at the container logs
- Fix `--technitium` failing to bind port 53 on stock Ubuntu where the systemd-resolved stub listener holds `127.0.0.53:53`: the module now preflights port 53 before starting the container, disables the stub listener via a `resolved.conf.d` drop-in while repointing the `stub-resolv.conf` symlink so host resolution survives the transition, fails fast (without stopping anything automatically) when a different service holds the port, points `/etc/resolv.conf` at Technitium only after the DNS listener verifies, and restores the stock systemd-resolved configuration on `--technitium --remove`

## 2026-04-27

### Improvements
- Add explicit 365-day service certificate lifetime support for step-ca-issued service certificates

### Fixes
- Fix short-lived step-ca service certificates by configuring and requesting an explicit 365-day certificate duration

## 2026-04-25

### Improvements
- Make shared bootstrap validation service-scoped so unrelated DNS/NTP settings do not block individual services
- Add early detection for outdated `labprovider.env` files when required variables are missing from the local configuration

### Fixes
- Fix CIDR validation to reject invalid IPv4 prefix lengths and make DNS record parsing handle whitespace consistently
- Normalize certificate artifact ownership and permissions when reusing existing step-ca-issued service certificates
- Make Keycloak full-chain bundle generation explicit for VCF SSO certificate-chain upload
- Normalize step-ca password file ownership and permissions for both initialization and runtime password files

## 2026-04-24

### Fixes
- Add Keycloak full certificate chain bundle for VCF SSO certificate-chain upload

## 2026-04-23

### Improvements
- Align Keycloak bootstrap realm defaults with VCF 9, including client settings, redirect URI, and bootstrap user email support.
- Make the Keycloak bootstrap user part of the default VCF-oriented realm bootstrap
- Be sure to update your `config/labprovider.env`.

### Fixes
- Make certificate handling for step-ca-dependent services identity-aware by reusing valid existing certificates and reissuing only when missing, expired, or mismatched

## 2026-04-21

### Features
- Add nginx-based VCF offline depot service
- Add optional SFTPGo backup-user bootstrap with validation, API-based provisioning, and idempotent create-if-missing behavior

### Improvements
- Centralize container image versions in labprovider.env
- Align default Keycloak bootstrap username with admin
- Normalize default persistent service paths under `/opt/labprovider`
- Move default runtime working directory to `/opt/labprovider/runtime`

### Fixes
- Fix depot certificate issuance failure caused by incorrect directory permissions for step-ca
- Preserve nginx runtime variables correctly in depot config rendering
- Fix depot basic auth by making the managed htpasswd file readable by nginx
- Harden certificate directory preparation for step-ca-dependent services
- Add post-start readiness checks for HTTPS services to fail fast when containers do not become reachable
- Fix Keycloak readiness checks by probing the user-facing HTTPS endpoint instead of `/health`
- Fix SFTPGo startup failure caused by SQLite "readonly database" errors by ensuring rw bind-mounted persistent directories are recursively owned by the container runtime user (UID:GID 1000:1000)
- Fix NetBox PostgreSQL startup failure caused by incorrect ownership on the bind-mounted data directory

---

## 2026-04-20

**Release: v0.1.0**

### Features
- Initial labprovider release
- Add bootstrap support for DNS, NTP, syslog, step-ca, Keycloak, NetBox, SeaweedFS, and SFTPGo
- Add an nginx-based VCF offline depot service with HTTP and HTTPS support
- Add step-ca-based certificate handling for containerized HTTPS services
- Add initial Keycloak realm bootstrap support for VCF-style integration

### Improvements
- Improve README structure, service documentation, and architecture overview
- Add Docker-service remove support for containerized services
- Improve CA password handling to avoid a repository-shipped static password file
- Improve NetBox seeding for labprovider service endpoints
