# Changelog

All notable changes to this project will be documented in this file.

---

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
- Fix SFTPGo startup failure caused by SQLite “readonly database” errors by ensuring rw bind-mounted persistent directories are recursively owned by the container runtime user (UID:GID 1000:1000)
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
