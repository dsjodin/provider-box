# Changelog

All notable changes to this project will be documented in this file.

---

## 2026-07-09

### Fixes
- Prevent unset variable-sourced bind-mount paths from corrupting data. When a variable used as a Docker bind-mount source is empty at compose time, `${VAR}/certs/root_ca.crt` collapses to `/certs/root_ca.crt` and Docker auto-creates the missing source as a DIRECTORY - which once turned step-ca's `root_ca.crt` into a directory and destroyed a running trust root ("file is a directory", init loop). Two guards now cover every variable-sourced mount:
  - Defense in depth in every compose template and the standalone `services/dashboard/docker-compose.yml`: each mount whose source begins with a variable now uses compose's mandatory-variable syntax `${VAR:?VAR must be set (empty would create a blank bind-mount source)}`. `envsubst` passes the guard through rendering unchanged, so an empty value aborts `docker compose` with a named error instead of mounting a blank path (32 mounts across 10 files).
  - Script-side validation before every compose call. All bootstrap modules already validated their mount-source variables via the `require_*_vars` helpers (audited: `CA_DATA_DIR`, `WORKDIR`, `TECHNITIUM_*_DIR`, `NETBOX_*_DIR`, `DEPOT_*_DIR`, `KEYCLOAK_DIR`, `AUTHENTIK_DIR`, `S3_DATA_DIR`, `SFTP_*_DIR`, `DNS_SYNC_SECRETS_DIR` are all covered). The standalone dashboard launcher `services/dashboard/scripts/run.sh` had none; it now validates `CA_DATA_DIR`, `DASHBOARD_CERT_DIR`, and `DASHBOARD_SECRETS_DIR` (non-empty, absolute) before running `docker compose`.
- Add a step-ca corruption guard: `--ca` now refuses to start if `${CA_DATA_DIR}/certs/root_ca.crt` exists as a directory (or any non-regular file), explaining the likely cause (a prior compose run with an empty `CA_DATA_DIR`) and the fix (remove it and restore/re-initialize), so the corruption fails loudly instead of silently breaking init.
- Behavior note: rendered compose files now retain the `${VAR:?...}` mount guards rather than a pre-substituted absolute path, so running `docker compose` on a rendered file manually requires the same environment sourced (the bootstrap modules already source it before every compose call).

### Features
- Add `services/dashboard`: a standalone, read-only "current state" view of the Provider Box services (single Go binary serving an HTML page + JSON API, std-lib HTTP, embedded template - matches the existing Go services and needs no frontend framework). Five panels, each isolated behind its own short timeout so a dead or unconfigured source renders "unavailable"/"not configured" without blanking the page: Certificates (step-ca), DNS (Technitium), IPAM (NetBox), Services (Docker), and Recent errors (container log tail)
- Absorb the design-stage `services/stepca-api` into the dashboard: its reusable step-ca BadgerDB reader is migrated to `services/dashboard/internal/certs` as the Certificates panel (active certs, subjects/SANs, provisioner, notBefore/notAfter, days-to-expiry against a warn threshold); the phase-2 collector parts (SQLite inventory, reconcile loop, token-authed REST API) were dropped as out of v1 scope. `services/stepca-api/` is removed
- Read paths reuse the shapes the repo already verified: the DNS panel calls the same Technitium endpoints as `dns-sync` (`zones/list`, `zones/records/get`, `settings/get`), the IPAM panel reads NetBox IPAM, and both honor the v1/v2 token formats
- Serve HTTPS with a step-ca-issued cert for `DASHBOARD_FQDN`; fall back to plaintext HTTP with a logged warning when no cert is configured (lab only)

### Security
- Read-only throughout: no upstream write path exists. The dashboard expects a dedicated minimum-read-scope NetBox token (not the dns-sync/bootstrap admin token), a scoped Technitium token, the step-ca DB via a read-only snapshot copy (never opened directly), and the Docker socket mounted `:ro`. All tokens come from files/env, never hardcoded or logged
- v1 ships with no authentication on the UI itself - acceptable only on a trusted internal lab network. Documented as an explicit assumption with a TODO to front it with auth before any non-lab use

### Notes
- The dashboard is run manually and is NOT wired into `bootstrap/provider-box.sh` or `--all`. A `--dashboard` bootstrap module, a history/collector, and UI auth are the documented phase 2. Run it with the standalone `services/dashboard/docker-compose.yml`; add the `DASHBOARD_*` block from `config/provider-box.env.example` to your `config/provider-box.env`

### Dashboard follow-ups (first-deploy fixes)
- Fix the HTTP fallback: with a `DASHBOARD_TLS_CERT` path set but the cert file missing/unreadable, the server crash-looped on bind instead of falling back. It now validates the cert/key (loads the keypair) before binding, logs a WARNING and serves plaintext HTTP when it cannot, and reports the mode actually chosen in the startup log's `tls` field
- Add `services/dashboard/scripts/issue-dashboard-cert.sh`: issues the dashboard's TLS leaf cert from step-ca (mirrors the technitium cert-issuance docker run - `--add-host ca:127.0.0.1`, admin provisioner + password file, `--not-after SERVICE_CERT_DURATION`, bundled leaf+chain into `DASHBOARD_CERT_DIR` owned by uid 1000), so HTTPS works on a clean deploy. The cert dir is chowned to uid 1000 BEFORE the run (not only after), so the uid-1000 step-cli container can write into a freshly created root-owned `DASHBOARD_CERT_DIR` instead of failing "permission denied" on a first run
- Add `services/dashboard/scripts/run.sh`: one correct launch path that runs the documented compose command (`--env-file ../../config/provider-box.env`) with `DASHBOARD_DOCKER_GID` resolved from the host docker group, avoiding the all-blank-vars / invalid-mount failure of a bare `docker compose up`
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
- Be sure to update your `config/provider-box.env` (new `DNS_BACKEND` and `DNS_FORWARDER` variables).

### Fixes
- Fix `--authentik` failing intermittently on re-run at the brand step with "Failed to determine the default Authentik brand": Authentik rejects even valid API tokens for a few seconds after container start, and `/-/health/ready/` does not imply token-auth readiness. A new authenticated readiness gate polls `GET /api/v3/core/brands/` with the Bearer token (bounded, 401/403 retried within the window) before any brand or blueprint configuration runs, with a timeout message that distinguishes a persistent-data token mismatch from a transient startup delay. Per-attempt curl stderr is also suppressed in the HTTP readiness wait and the certificate verification poll, which previously printed normal "connection reset" and "self-signed certificate" retries as error noise
- Fix built-in Provider Box service FQDNs (netbox, ca, dns, auth, idp, ...) never reaching the Technitium zone: dns-sync now synthesizes their A records from `provider-box.env` on every reconcile, using the same built-in list as the unbound backend (`provider_box_builtin_fqdns`, which now also covers `AUTHENTIK_FQDN` and skips unset services); they are deliberately not seeded into NetBox because the pinned NetBox enforces global IP uniqueness (verified live: duplicate host-IP objects are rejected without anycast roles), and A-only synthesis keeps `PROVIDER_BOX_FQDN` as the sole PTR target; the post-deploy zone verification now checks every built-in FQDN resolves via Technitium
- Fix the long-running dns-sync container failing every reconcile with "connection refused": the `127.0.0.1` extra_hosts pins pointed at the container's own loopback on the default bridge network; the service now runs with `network_mode: host`, matching the module's one-shot docker runs
- Remove the Technitium forwarder step from `--dns-sync`, which silently overwrote the `DNS_FORWARDER` value the technitium module sets and verifies (one owner per setting); the unused `TECHNITIUM_FORWARDER` variable is removed from the example env
- Include the NetBox response body (about 200 characters) in dns-sync client errors on non-2xx responses so failures like 403 are diagnosable from logs
- Auto-provision the dns-sync NetBox token during `--netbox`, mirroring the technitium module's token handling: a stored token at `DNS_SYNC_SECRETS_DIR/netbox.token` is validated live and reused (operator-placed tokens win while valid); otherwise a dedicated token with description "provider-box dns-sync" is provisioned, previous tokens with that description are retired, the composite is validated with one authenticated request, and it is written with the same ownership/permission conventions; skipped with a notice when `DNS_SYNC_SECRETS_DIR` is unset so `--netbox` stays standalone
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
- Add early detection for outdated `provider-box.env` files when required variables are missing from the local configuration

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
- Be sure to update your `config/provider-box.env`.

### Fixes
- Make certificate handling for step-ca-dependent services identity-aware by reusing valid existing certificates and reissuing only when missing, expired, or mismatched

## 2026-04-21

### Features
- Add nginx-based VCF offline depot service
- Add optional SFTPGo backup-user bootstrap with validation, API-based provisioning, and idempotent create-if-missing behavior

### Improvements
- Centralize container image versions in provider-box.env
- Align default Keycloak bootstrap username with admin
- Normalize default persistent service paths under `/opt/provider-box`
- Move default runtime working directory to `/opt/provider-box/runtime`

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
- Initial Provider Box release
- Add bootstrap support for DNS, NTP, syslog, step-ca, Keycloak, NetBox, SeaweedFS, and SFTPGo
- Add an nginx-based VCF offline depot service with HTTP and HTTPS support
- Add step-ca-based certificate handling for containerized HTTPS services
- Add initial Keycloak realm bootstrap support for VCF-style integration

### Improvements
- Improve README structure, service documentation, and architecture overview
- Add Docker-service remove support for containerized services
- Improve CA password handling to avoid a repository-shipped static password file
- Improve NetBox seeding for Provider Box service endpoints
