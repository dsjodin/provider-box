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
- Be sure to update your `config/provider-box.env` (new `DNS_BACKEND` variable).

### Fixes
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
