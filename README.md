# labprovider

labprovider is a lightweight, single-node platform for standing up shared infrastructure services on a single dedicated host. It provides a self-contained infrastructure services layer for lab environments.

It is designed for lab and proof-of-concept environments, especially VMware Cloud Foundation (VCF). Services (all containerized via Docker Compose):

- DNS via Technitium (NetBox-driven)
- dns-sync for reconciling NetBox IPAM records into Technitium
- Chrony for NTP
- rsyslog for centralized syslog collection
- step-ca for a lightweight private certificate authority (dedicated PostgreSQL backend)
- VCF offline depot served by nginx
- Keycloak for identity
- Authentik for identity federation with OIDC and outbound SCIM 2.0 provisioning
- Zitadel for identity with OIDC (Management API-provisioned bootstrap client)
- NetBox for IPAM, DCIM, and infrastructure source-of-truth
- SeaweedFS for S3-compatible object storage
- SFTPGo for SFTP file transfer
- The labprovider control plane: a web UI with a configuration wizard, service selection + deployment with live progress, and a read-only dashboard of everything above

## labprovider v2: the control plane

The control plane is the primary way to run labprovider. One script installs it; everything else happens in the browser:

```bash
git clone https://github.com/dsjodin/labprovider.git
cd labprovider
sudo bash install.sh
```

`install.sh` installs Docker if absent (Debian and Ubuntu), does the one-time host preparation (disables the systemd-resolved stub listener so Technitium can own port 53, disables systemd-timesyncd because chrony runs containerized), builds the control-plane image from the checkout, and starts it. It prints the UI URL when done (port 8445 by default).

Then, in the UI:

1. **`/config`** - edit or paste `labprovider.env` (or download it, fill it out locally, and paste it back), validate (every problem is reported at once, per variable), and save. Optional external DNS records (`dns.seed`) are managed on the same page.
2. **`/deploy`** - tick the services you want (dependencies are added automatically), press Deploy, and watch the live log. "Select all" deploys the full catalog in dependency order: chrony, rsyslog, ca, technitium, depot, keycloak, authentik, zitadel, netbox, s3, sftp, dns-sync.
3. **`/`** - the dashboard: certificates (step-ca), DNS zones (Technitium), IPAM (NetBox), container state, and recent errors at a glance.

After the CA is deployed the control plane issues its own certificate; restart the container (`docker restart labprovider-control-plane`) to serve the UI over HTTPS.

**The UI has no authentication (v1).** Run it on a trusted lab network only.

### Required open ports

`install.sh` and the control plane do not manage the firewall. If the host runs ufw or similar, open the service ports you deploy:

| Service | Ports |
|---------|-------|
| Control plane UI | 8445/tcp |
| Technitium DNS | 53/tcp+udp, 5380/tcp, 53443/tcp |
| Chrony | 123/udp |
| rsyslog | 514/tcp+udp (SYSLOG_PORT) |
| step-ca | 9000/tcp |
| Depot | 80/tcp, 443/tcp |
| Keycloak | 8443/tcp |
| Authentik | 9443/tcp |
| Zitadel | 7443/tcp |
| NetBox | 8444/tcp |
| S3 | 8333/tcp |
| SFTPGo | 2022/tcp, 8080/tcp |

Ports are the example-config defaults; adjust to your values.

## Transitional: the bash bootstrap

The original bash bootstrap (`bootstrap/labprovider.sh` plus per-service modules) still works and is documented below. It deploys chrony and rsyslog as host-native systemd services (the control plane runs them containerized) and does not deploy the containerized chrony/rsyslog or the control-plane engine features. It will be removed once the control-plane path has proven parity; new deployments should use `install.sh`.

## Overview

![labprovider Overview](docs/images/labprovider-overview.png)
*labprovider v2 architecture: the control plane, the containerized Docker Compose services, the host foundation, and external dependencies.*

## Table of Contents

- [Quick Start](#quick-start)
- [Host Assumptions](#host-assumptions)
- [Choosing Services](#choosing-services)
- [Service Runtime Model](#service-runtime-model)
- [Removing Services](#removing-services)
- [Upgrading Services](#upgrading-services)
- [Configuration Model](#configuration-model)
- [Dependency Updates](#dependency-updates)
- [Service Notes](#service-notes)
- [Module Reference](#module-reference)
- [Secrets Inventory](#secrets-inventory)
- [Troubleshooting](#troubleshooting)
- [VCF Lab Companion](#vcf-lab-companion)
- [Design Trade-offs](#design-trade-offs)
- [Repository Layout](#repository-layout)
- [Development Safeguards](#development-safeguards)
- [Failure Handling](#failure-handling)
- [Operational Notes](#operational-notes)
- [Scope](#scope)

## Quick Start

### 1. Copy the example files

```bash
cp config/labprovider.env.example config/labprovider.env
```

Optionally, to publish external/custom DNS records (VCF nodes, gateways, and other non-labprovider hosts), copy the seed file. It is imported into NetBox by `--dns-sync` (and by `--netbox` when the file exists), and the reconcile loop then publishes the records via Technitium:

```bash
cp config/dns.seed.example config/dns.seed
```

The copy is optional for a minimal deployment. Built-in labprovider service records never come from this file.

### 2. Update configuration files

- `config/labprovider.env` defines all service configuration
- `config/dns.seed` (optional) defines external and custom bring-up records

Built-in labprovider service FQDNs are generated automatically from the `*_FQDN` values in `labprovider.env`. You do not add built-in service records to `config/dns.seed`.

### Quick Password Setup

To quickly replace all placeholder passwords with a single value:

```bash
PASSWORD='VMware1!VMware1!' \
SECRET_KEY=$(openssl rand -base64 48 | sed 's/[&]/\\&/g') \
&& sed -i \
  -e "s|CHANGE_ME_WITH_AT_LEAST_50_RANDOM_CHARACTERS_BEFORE_USE|$SECRET_KEY|g" \
  -e "s|CHANGE_ME|$PASSWORD|g" \
  config/labprovider.env
```

### 3. Run the bootstrap script

Run only the services you want, or use `--all` to deploy all services in the correct order:

```bash
sudo bash bootstrap/labprovider.sh --ntp
sudo bash bootstrap/labprovider.sh --rsyslog
sudo bash bootstrap/labprovider.sh --ca
sudo bash bootstrap/labprovider.sh --technitium
sudo bash bootstrap/labprovider.sh --depot
sudo bash bootstrap/labprovider.sh --keycloak
sudo bash bootstrap/labprovider.sh --authentik
sudo bash bootstrap/labprovider.sh --netbox
sudo bash bootstrap/labprovider.sh --dns-sync
sudo bash bootstrap/labprovider.sh --s3
sudo bash bootstrap/labprovider.sh --sftp
sudo bash bootstrap/labprovider.sh --all
```

Ordering rules when running services individually:

- `--ca` must run before any service that uses a step-ca-issued certificate: `--technitium`, `--depot`, `--keycloak`, `--authentik`, `--netbox`, `--sftp`, and `--dns-sync`
- `--dns-sync` must run after both `--technitium` and `--netbox`

`--all` deploys Technitium right after the CA (Technitium needs a step-ca certificate). `--dns-sync` is never part of `--all`; run it explicitly after `--all`:

```bash
sudo bash bootstrap/labprovider.sh --all
sudo bash bootstrap/labprovider.sh --dns-sync
```

## Host Assumptions

labprovider assumes:

- Ubuntu or Debian-based host (labprovider is developed and tested on Debian GNU/Linux 13 (trixie), but should work on recent Ubuntu releases)
- root or `sudo` access
- static IP and prefix already configured on the host
- network connectivity from lab consumers to this host
- access to Debian or Ubuntu package repositories (required packages are installed automatically)
- access to Docker package repositories (required for containerized services)

labprovider uses Docker Compose via `docker compose` (Compose v2). Docker installation is idempotent:

- If Docker with Compose v2 already works, existing Docker packages are left untouched and the `docker` service is enabled
- If Docker exists but Compose v2 is missing, only `docker-compose-plugin` is installed
- If Docker is absent, Docker CE is installed from Docker's official Debian apt repository (this fallback assumes a Debian host; on Ubuntu, install Docker with Compose v2 yourself before running bootstrap)

## Choosing Services

### Minimum required for VCF bring-up

- DNS: Technitium (with NetBox and dns-sync)
- Chrony for NTP

### Recommended for realistic lab environments

- rsyslog
- SFTPGo for file transfer
- step-ca
- VCF offline depot
- Keycloak
- NetBox

### Optional depending on use case

- SeaweedFS for S3-compatible storage
- Authentik as an alternative identity provider when OIDC plus outbound SCIM 2.0 provisioning is required (for example VCF 9 identity federation)

Services are intended to remain independently deployable unless a dependency is explicit and documented.

Examples:

- `--netbox` does not require Technitium
- `--s3` and `--sftp` do not require unrelated service configuration
- step-ca is an intentional dependency for services that use labprovider-issued TLS certificates
- `--dns-sync` intentionally depends on Technitium and NetBox; it is the bridge between them

## Service Runtime Model

labprovider uses a mixed runtime model. Host-based services modify the local system and are not managed via `--remove` (they must be removed manually using system package/service management), while Docker-based services are isolated and can be removed using `--remove`.

| Service   | Runtime |
|-----------|---------|
| Chrony   | Host (native service) |
| rsyslog  | Host (native service) |
| step-ca  | Docker Compose |
| Technitium DNS | Docker Compose |
| dns-sync | Docker Compose (image built locally from `services/dns-sync`) |
| VCF offline depot | Docker Compose |
| Keycloak | Docker Compose |
| Authentik | Docker Compose |
| NetBox   | Docker Compose |
| SeaweedFS (S3) | Docker Compose |
| SFTPGo   | Docker Compose |

## Removing Services

Docker-based services can be removed with `--remove`:

```bash
sudo bash bootstrap/labprovider.sh --netbox --remove
sudo bash bootstrap/labprovider.sh --depot --remove
sudo bash bootstrap/labprovider.sh --sftp --remove
sudo bash bootstrap/labprovider.sh --technitium --remove
sudo bash bootstrap/labprovider.sh --dns-sync --remove
sudo bash bootstrap/labprovider.sh --all --remove
```

Removal stops and removes containers with `docker compose down` and deletes generated runtime files under `WORKDIR`. Persistent data directories are preserved. The remove path is idempotent and safe to run multiple times.

When using `--all --remove`, services are removed in reverse dependency order. `--all --remove` covers only the services `--all` always deploys (SFTPGo, S3, NetBox, Authentik, Keycloak, depot, step-ca); remove Technitium and dns-sync explicitly with their own `--remove` flags.

`--technitium --remove` additionally restores the stock host resolver configuration: it deletes the labprovider `systemd-resolved` drop-in, points `/etc/resolv.conf` back at the `systemd-resolved` stub, and restarts `systemd-resolved`.

Host-based services (`--ntp`, `--rsyslog`) do not support `--remove` and fail fast if it is passed; remove them manually with system package/service management.

See [Module Reference](#module-reference) for exactly what each `--remove` deletes and preserves.

## Upgrading Services

Container image versions are pinned in `config/labprovider.env` (see [Dependency Updates](#dependency-updates)). To move a containerized service to a new image version, change its `*_IMAGE` pin and redeploy that service; the bootstrap re-runs its configuration idempotently and the persisted data directory carries state forward.

Before a major-version bump, review the upstream project's release notes for breaking changes to the parts labprovider drives (APIs, settings parameters, data directory format, ports, and the container's user/permissions model), and take a backup of the service's persistent data directory so a rollback is possible.

General upgrade procedure for a containerized service:

```bash
# 1. Back up the persistent data directory (rollback insurance)
sudo tar czf /opt/labprovider/<service>-backup-$(date +%F).tgz -C /opt/labprovider <service>

# 2. Update the pinned image version in config/labprovider.env
#    (edit the relevant *_IMAGE line; never use :latest)

# 3. Redeploy the single service
sudo bash bootstrap/labprovider.sh --<service> --remove
sudo bash bootstrap/labprovider.sh --<service>
```

Rollback: stop the service, restore the pre-upgrade data-directory backup, repin the previous image version, and redeploy.

### Technitium DNS (13.x -> 15.x)

Reviewed release: `docker.io/technitium/dns-server:15.3.0` (upgrade from `13.4.2`, assessed 2026-07-08). The API surface labprovider uses (web service TLS settings, `createToken`, forwarder settings, zone/record CRUD), the data directory layout, ports, and the container uid are unchanged or backward-compatible; the query-string token form still works. A 13.x data directory migrates in place on first start of 15.x (existing zones, records, and API tokens are preserved), so the standard redeploy procedure above applies.

- **Forward-only.** Once 15.x starts on a data directory it rewrites the `*.config` files; a 15.x data directory must NOT be run under 13.x afterward. Rollback to 13.x requires restoring the pre-upgrade backup taken in step 1 - there is no in-place downgrade.
- **DNS stays up across the swap.** `--technitium` pre-pulls the pinned image before stopping the running container, so when Technitium is the host resolver the image is already cached when DNS briefly goes down during recreate. If the pull fails, the deploy aborts with the old server still running.
- **Behavioral deltas that do not affect labprovider** (documented in `services/dns-sync/TECHNITIUM_API.md`): built-in `internal` reverse zones no longer appear in `zones/list` on 15.x, and deleting a non-existent zone or record now returns an error instead of succeeding silently.

## Configuration Model

`config/labprovider.env` defines all service configuration.

Validation is strict and runs per selected service before deployment changes are made.

Pinned container image versions for Docker-based services are also defined centrally in `config/labprovider.env`.

For step-ca, no repository-shipped password file is required. labprovider uses `CA_PASSWORD_FILE` when the file exists, materializes `CA_PASSWORD` into a managed `0600` file when set, or generates a random password automatically under `CA_DATA_DIR` when neither input is provided.

### General validation behavior

labprovider rejects:

- empty required values
- invalid FQDNs
- invalid IPs or CIDRs
- invalid absolute-path requirements
- placeholder secret values such as `CHANGE_ME`
- malformed DNS record entries

### Host IP and canonical identity

`HOST_IP` uses IPv4 CIDR notation, for example:

```bash
HOST_IP="192.168.12.121/24"
```

labprovider derives the raw host IPv4 address when services need a plain address and preserves the subnet information when it is useful for NetBox IPAM import.

`LABPROVIDER_FQDN` defines the canonical host identity for the labprovider node.

This distinction is intentional:

- `LABPROVIDER_FQDN` is the canonical host FQDN for the shared labprovider host IP
- service FQDNs such as `DNS_FQDN`, `CA_FQDN`, `DEPOT_FQDN`, `KEYCLOAK_FQDN`, `NETBOX_FQDN`, `S3_FQDN`, `SFTP_FQDN`, and `SYSLOG_FQDN` remain service endpoints on the same host

### DNS record format

`config/dns.seed` supports:

```text
<fqdn> <ip>
<fqdn> <ip/cidr>
```

Behavior:

- If a record includes CIDR information, labprovider can derive the surrounding subnet for NetBox
- If a record includes only a plain IP, labprovider imports the host address without guessing the subnet
- Built-in labprovider service records are generated automatically and should not be duplicated in `config/dns.seed`

### DNS model

The `--technitium` module deploys the DNS server, and the `--dns-sync` module imports `config/dns.seed` into NetBox (when the file exists) and runs a continuous NetBox-to-Technitium reconcile loop. After bring-up, NetBox is the source of truth for lab records; change records in NetBox, not in the seed file.

Technitium forwards external queries to `DNS_FORWARDER`. It applies its default recursion policy, which serves RFC1918 (private) client networks; if the lab uses non-RFC1918 ranges, adjust the recursion access control list in the Technitium console so those clients can resolve.

Built-in labprovider service records are generated automatically from the `*_FQDN` values in `labprovider.env`: dns-sync synthesizes them into the desired record set on every reconcile. They are not stored in NetBox, which enforces global IP uniqueness and holds a single canonical host IP object (`LABPROVIDER_FQDN`); that object also remains the reverse PTR target for the host IP.

### Template rendering

Environment variables are exported before template rendering so `envsubst` can populate the service templates consistently.

## Dependency Updates

Container image versions are centrally defined in `config/labprovider.env.example` and kept up to date using Renovate in the labprovider repository.

Users consume updated versions by pulling changes to the repository.

## Service Notes

### Technitium DNS

- Runs the Technitium DNS server via Docker Compose
- Requires step-ca to be initialized first (`--ca`)
- Serves DNS on port 53 (TCP and UDP)
- Web console over HTTP at `http://<DNS_FQDN>:<TECHNITIUM_HTTP_PORT>` (`5380` by default)
- Web console and API over HTTPS at `https://<DNS_FQDN>:<TECHNITIUM_HTTPS_PORT>` (`53443` by default), using a step-ca-issued certificate
- Persists zone and settings data under `TECHNITIUM_DATA_DIR` and certificates under `TECHNITIUM_CERT_DIR`
- Configures `DNS_FORWARDER` as the upstream forwarder over the settings API and verifies external resolution before pointing the host at itself. The technitium module is the only owner of the forwarder setting; dns-sync never touches it.

Bootstrap behavior:

- Technitium requires its web TLS certificate as PKCS#12, so the module converts the step-ca PEM material into `technitium.pfx` with a generated password persisted at `TECHNITIUM_CERT_DIR/technitium-pfx-password`. The bundle is rebuilt automatically whenever the certificate is reissued.
- An API token for dns-sync is created via the Technitium API and stored at `DNS_SYNC_SECRETS_DIR/technitium.token` (mode `0600`). A stored token is validated and reused while Technitium still accepts it.
- Bootstrap-phase API calls authenticate with Technitium's first-boot `admin`/`admin` credentials over HTTP on `127.0.0.1`. Change the admin password in the Technitium console after bootstrap.
- On stock Ubuntu, the `systemd-resolved` stub listener holds `127.0.0.53:53`. The module disables the stub listener via a `resolved.conf.d` drop-in (keeping host resolution working through the transition) before starting the container. If any other service holds port 53, the module fails fast and does not stop it automatically.
- After the DNS listener, forwarder, HTTPS endpoint, and API token are all verified, the module points `/etc/resolv.conf` at Technitium (`127.0.0.1`).

Removal behavior:

- `--technitium --remove` runs `docker compose down`, removes runtime files under `WORKDIR/technitium`, and restores the stock `systemd-resolved` configuration (stub listener re-enabled, `/etc/resolv.conf` pointed back at the stub)
- Persistent data in `TECHNITIUM_DATA_DIR` and certificates in `TECHNITIUM_CERT_DIR` (including the pfx bundle and its password) are preserved

### dns-sync

- Continuously reconciles DNS records from NetBox IPAM into Technitium
- Requires `--ca`, `--technitium`, and `--netbox` to have run first; both readiness gates pin the lab FQDNs to `127.0.0.1`, so nothing depends on the zone it is about to populate
- The container image (`DNS_SYNC_IMAGE`) is built locally from `services/dns-sync` during bootstrap; no registry access is needed
- Runs with host networking so its `127.0.0.1` pins reach the host-published NetBox and Technitium ports
- Reconciles every `DNS_SYNC_INTERVAL` (for example `30s`, `5m`, `1h`): one A record per NetBox IP object with a `dns_name`, one PTR per IP (using a deterministically chosen canonical name when several names share an IP), and the built-in service records below
- Built-in labprovider service records are synthesized from the `*_FQDN` values in `labprovider.env` on every reconcile pass. They are deliberately not stored in NetBox (NetBox enforces global IP uniqueness; the host IP is one canonical object with `LABPROVIDER_FQDN` as `dns_name`), and they are A records only so `LABPROVIDER_FQDN` stays the sole PTR target.
- Imports `config/dns.seed` into NetBox before starting the loop when the file exists (idempotent; skipped with a notice otherwise)
- Expects API tokens at `DNS_SYNC_SECRETS_DIR/netbox.token` and `DNS_SYNC_SECRETS_DIR/technitium.token`. Both are auto-provisioned (`--netbox` and `--technitium` respectively); placing decrypted tokens there out of band (for example via SOPS/age) is the operator override and wins while the token stays valid.
- After the first reconcile, bootstrap verifies over real DNS that `LABPROVIDER_FQDN` and every built-in service FQDN resolve via Technitium
- Logs: `docker compose -f ${WORKDIR}/dns-sync/docker-compose.yml logs -f`

Removal behavior:

- `--dns-sync --remove` runs `docker compose down` and removes runtime files under `WORKDIR/dns-sync`
- Secrets in `DNS_SYNC_SECRETS_DIR` are preserved

### Chrony

- Uses configured upstream NTP servers
- Provides NTP service to internal networks

### rsyslog

- Runs natively on the host
- Exposes centralized syslog via UDP and TCP
- Intended for log aggregation, not long-term analytics
- Stores logs under `SYSLOG_LOG_DIR`

### step-ca

- Runs as a single-node Smallstep CA via Docker Compose
- Acts as the internal PKI for labprovider services
- Exposed at `https://<CA_FQDN>:<CA_PORT>`
- Persists data under `CA_DATA_DIR` (keys, `ca.json`) and stores CA state in a
  dedicated PostgreSQL backend (`stepca-postgres`)
- Allows service certificates up to `SERVICE_CERT_DURATION` (`8760h` by default)

Behavior:

- Initializes automatically on first start
- Uses `CA_PASSWORD_FILE` as-is when that file already exists
- Materializes `CA_PASSWORD` to a managed `0600` file when provided
- Generates a random CA password automatically when no password input is provided
- Running `--ca` configures the provisioner default and maximum X.509 certificate duration from `SERVICE_CERT_DURATION`

PostgreSQL backend:

- step-ca stores its state in a DEDICATED postgres container (`stepca-postgres`),
  never shared with NetBox/Authentik (module independence, CA isolation).
- step-ca uses postgres as an opaque key-value store (one table per bucket,
  each `nkey`/`nvalue` `BYTEA`), not a relational schema. Cert attributes live
  inside the `BYTEA` blobs, so anything reading the DB parses the blobs; it
  cannot filter/join on cert fields in SQL.
- The postgres owner password is supplied to step-ca via a mounted `.pgpass`
  file (`PGPASSFILE`), so it never appears in `ca.json`'s `dataSource` DSN.
- The postgres port is published on `127.0.0.1:<CA_POSTGRES_PORT>` only, for the
  host-networked dashboard's read-only cert panel. It is never exposed off-host.
- `--ca` also creates a read-only role (`CA_POSTGRES_RO_USER`) with `SELECT` on
  the cert tables only; the dashboard reads through it.
- CRL is enabled (`crl.enabled`) so revocation is served. The remote admin API
  is NOT enabled and there is no write/revoke path in this design.
- On first init the container self-initializes with badger, then `--ca`
  rewrites `ca.json`'s `db` stanza to postgresql, restarts, and moves the unused
  badger dir aside (`db.pre-postgres.<timestamp>`, retained, not deleted).
  Switching backends does NOT migrate data: badger state does not move to
  postgres.

Important notes:

- `CA_PASSWORD` is convenient for lab use, but when set in `labprovider.env` it is still stored there in plaintext.
- Reinitialization requires deleting the contents of `CA_DATA_DIR`. `--ca`
  refuses to run against an existing badger-backed CA: Phase 2 rebuilds on
  postgres rather than migrating in place.
- `CA_POSTGRES_DATA_DIR` MUST be a sibling of `CA_DATA_DIR`, never nested under
  it (the `chown -R 1000:1000 CA_DATA_DIR` step would corrupt postgres data).
- No repository-shipped static CA password file is required
- The root certificate is available from `/roots.pem`

Rebuild + reissue runbook (run on-host; `--ca` does not do the destructive
steps for you):

1. Stop dependents you are about to reissue, then remove the CA runtime:
   `sudo bash bootstrap/labprovider.sh --ca --remove`.
2. Wipe the CA state (lab certs are disposable): remove `CA_DATA_DIR` and
   `CA_POSTGRES_DATA_DIR`. Wiping both keeps the new root and the empty postgres
   store consistent; `--ca` refuses a new root against a non-empty store.
3. `sudo bash bootstrap/labprovider.sh --ca` - initializes on postgres, enables
   CRL, and creates the read-only role.
4. Reissue every service certificate against the new root, one at a time,
   verifying each before the next. The order is:
   `--technitium`, `--netbox`, `--authentik`, `--depot`, `--sftp`, `--dashboard`,
   then re-run `--dns-sync` (its NetBox/Technitium tokens are reissued too).
   Keycloak (if deployed) reissues the same way. Verify each leaf chains to the
   new root, e.g.
   `openssl verify -CAfile "$CA_DATA_DIR/certs/root_ca.crt" <service-leaf>.crt`.
5. Confirm CRL is served: `curl --cacert "$CA_DATA_DIR/certs/root_ca.crt"
   --resolve "$CA_FQDN:$CA_PORT:127.0.0.1" https://$CA_FQDN:$CA_PORT/crl`.

Certificate issuance is DNS-independent by design. Every module that requests a certificate (Technitium, depot, Keycloak, Authentik, NetBox, SFTPGo) pins `CA_FQDN` to `127.0.0.1` with `--add-host`/`--resolve` instead of resolving it, so certificates can be issued before any DNS backend exists. This relies on the single-node assumption: step-ca and every certificate-consuming service run on the same host, so `127.0.0.1` always reaches the CA. The dns-sync readiness gates and the bootstrap health checks use the same pinning for the same reason.

### VCF offline depot

- Runs as a single-node nginx container via Docker Compose
- Requires step-ca to be initialized first
- Exposes:
  - HTTP over `http://<DEPOT_FQDN>:<DEPOT_HTTP_PORT>`
  - HTTPS over `https://<DEPOT_FQDN>:<DEPOT_HTTPS_PORT>`
- Uses a step-ca-issued certificate stored under `DEPOT_CERT_DIR`
- Stores the managed `htpasswd` file under `DEPOT_AUTH_DIR`
- Persists depot content under `DEPOT_DATA_DIR`
- Creates the expected `PROD/COMP`, `PROD/metadata`, and `PROD/vsan/hcl` directory layout during bootstrap
- Serves both HTTP and HTTPS directly in phase 1 with no forced redirect
- Protects `/PROD/metadata/`, `/PROD/COMP/`, and `/PROD/COMP/Compatibility/VxrailCompatibilityData.json` with basic auth
- Leaves `/PROD/vsan/hcl/`, `/healthz`, `/products/v1/bundles/all`, and `/products/v1/bundles/lastupdatedtime` accessible without authentication
- Renders runtime files under `WORKDIR/depot`

Removal behavior:

- `--depot --remove` runs `docker compose down`
- Generated runtime files under `WORKDIR/depot` are removed
- The managed `htpasswd` file is removed and recreated on the next bootstrap run
- Persistent depot content under `DEPOT_DATA_DIR` is preserved
- step-ca-issued certificates under `DEPOT_CERT_DIR` are preserved

### Keycloak

- Runs via Docker Compose
- Requires step-ca to be initialized first
- Uses a certificate issued by step-ca
- Exposed at `https://<KEYCLOAK_FQDN>:<KEYCLOAK_PORT>` (`8443` by default)
- Seeds an opinionated initial realm from a repository-managed realm import template on first deployment

Key files:

- `keycloak.crt` for the Keycloak HTTPS certificate file
- `keycloak.key` for the private key
- `keycloak-ca-chain.pem` for CA chain material
- `keycloak-ca-roots.pem` for roots-only trust use cases
- `keycloak-full-chain.pem` for VCF SSO certificate-chain upload

VCF SSO expects the full IdP TLS chain in leaf, intermediate, root order. Use `keycloak-full-chain.pem` for that upload field.

Realm bootstrap:

- Uses a repository-managed realm template derived from a working Keycloak realm export and adapted for labprovider
- Imports one opinionated initial realm, one bootstrap group, and one baseline OIDC client for VCF-style integration
- Bootstraps one initial lab user in the bootstrap realm using `KEYCLOAK_BOOTSTRAP_USERNAME`, `KEYCLOAK_BOOTSTRAP_USER_PASSWORD`, and `KEYCLOAK_BOOTSTRAP_USER_EMAIL_DOMAIN`
- Seeds initial realm state only; it does not provide a generic realm-management framework
- Changes to the realm template are only applied on initial bootstrap; existing realms are not reconciled or modified on subsequent runs

### Authentik

- Runs via Docker Compose with Authentik server, Authentik worker, and PostgreSQL
- Requires step-ca to be initialized first
- Intended for VMware Cloud Foundation 9 identity federation with OIDC authentication and outbound SCIM 2.0 provisioning (which Keycloak lacks)
- Runs in parallel with Keycloak on separate FQDNs and ports when both are deployed (including via `--all`); federate VCF against one of them, using Authentik when SCIM provisioning is required
- Exposed at `https://<AUTHENTIK_FQDN>:<AUTHENTIK_PORT>` (`9443` by default)
- Persists application data under `${AUTHENTIK_DIR}/data` and PostgreSQL data under `${AUTHENTIK_DIR}/postgres`
- Uses a step-ca-issued certificate stored under `${AUTHENTIK_DIR}/certs/<AUTHENTIK_FQDN>` as `fullchain.pem` and `privkey.pem`, picked up by Authentik's built-in certificate discovery
- Bootstraps the `akadmin` password from `AUTHENTIK_ADMIN_PASSWORD` and an API token from `AUTHENTIK_API_TOKEN` on first start
- Seeds an opinionated bootstrap blueprint on startup: one group, one lab user, one OIDC provider (`vcf-oidc`), and one hidden `VCF` application for VCF-style integration
- Sets the default brand web certificate to the discovered step-ca keypair after startup
- OIDC discovery is served at `https://<AUTHENTIK_FQDN>:<AUTHENTIK_PORT>/application/o/vcf/.well-known/openid-configuration`

Blueprint bootstrap:

- Seeds initial state only; existing objects are not overwritten in ways that discard operator changes (the bootstrap user is created once and left alone afterwards)
- Changes to bootstrap client settings in `labprovider.env` are re-applied to the provider on subsequent runs

VCF integration notes:

- Import `${CA_DATA_DIR}/certs/root_ca.crt` into VCF's trusted certificate authorities
- After configuring the VCF Identity Broker, create the SCIM provider in Authentik manually using the SCIM base URL and bearer token that VCF generates, and assign it as the backchannel provider on the `VCF` application. The SCIM URL and token only exist after the VCF side is configured, so this step is not automated.

### Zitadel

- Deployed by the control plane (Go deploy engine); not available in the legacy `bootstrap/*.sh` path
- Runs Zitadel v4 with its decoupled Login V2 UI, so the stack is four containers: PostgreSQL 17, the core server, the `zitadel-login` container, and an nginx TLS terminator that fronts both (v4 dropped CockroachDB support)
- Requires step-ca to be initialized first
- Runs in parallel with Keycloak and Authentik on separate FQDNs and ports when more than one is deployed (including via "Select all")
- Exposed at `https://<ZITADEL_FQDN>:<ZITADEL_PORT>` (`7443` by default), served by the nginx terminator using the step-ca-issued certificate (mounted from `${ZITADEL_DIR}/certs/<ZITADEL_FQDN>`)
- The core runs plain HTTP behind the proxy (`--tlsMode external`, `ExternalSecure=true`); nginx routes `/ui/v2/login` to the login container and everything else to the core
- Persists application state in PostgreSQL 17 under `${ZITADEL_DIR}/postgres`
- `ZITADEL_MASTERKEY` must be EXACTLY 32 characters (Zitadel requirement)
- On first start Zitadel's FirstInstance init creates a human admin (`ZITADEL_ADMIN_USERNAME`/`ZITADEL_ADMIN_PASSWORD`), an admin service account whose PAT is written to `WORKDIR/zitadel/machinekey/pat.txt`, and the `login-client` service account whose PAT (`WORKDIR/zitadel/machinekey/login-client.pat`) the Login V2 container authenticates with
- Post-deploy, the control plane uses the admin PAT against the Management API to create a bootstrap project, an OIDC application with `ZITADEL_BOOTSTRAP_CLIENT_REDIRECT_URIS`, a project role (`ZITADEL_BOOTSTRAP_GROUP_NAME`), and a lab user granted that role; the steps tolerate pre-existing objects on re-runs
- Zitadel generates the OIDC client id/secret on creation, so the deploy writes the real issuer/client id/secret to `${ZITADEL_DIR}/certs/<ZITADEL_FQDN>/zitadel-oidc-client.txt` for use with VCF SSO
- **Multi-tenant**: set `ZITADEL_TENANTS` to a comma-separated list of org names to seed each as an isolated organization (its own vcf-sso project, OIDC client, role, and lab user) instead of a single set in the default org. Orgs share the one instance URL (`https://<ZITADEL_FQDN>:<ZITADEL_PORT>`) - the generated org domain (`<name>.<fqdn>`) is a logical identifier for login names and org discovery, not a DNS record or cert. Each tenant's generated client id/secret, issuer, and org login scope (`urn:zitadel:iam:org:id:<orgId>`, which a VCF OIDC request can pass to pin sign-in to that tenant) are written to `zitadel-oidc-<name>.txt`. All tenants currently share the bootstrap client/user template; the default org stays admin-only
- OIDC discovery is served at `https://<ZITADEL_FQDN>:<ZITADEL_PORT>/.well-known/openid-configuration`

### NetBox

- Runs via Docker Compose with NetBox, PostgreSQL, Redis, and a small HTTPS terminator
- Requires step-ca to be initialized first
- Intended as an IPAM, DCIM, and infrastructure source-of-truth service
- Exposed at `https://<NETBOX_FQDN>:<NETBOX_PORT>`
- Persists media under `NETBOX_MEDIA_DIR`
- Persists PostgreSQL data under `NETBOX_POSTGRES_DATA_DIR`
- Persists Redis data under `NETBOX_REDIS_DATA_DIR`
- Uses a step-ca-issued certificate stored under `${NETBOX_DIR}/certs`
- Bootstraps the initial superuser from `NETBOX_SUPERUSER_*` variables on first start
- Seeds labprovider service endpoints into NetBox via the NetBox API after startup
- Imports DNS records from `config/dns.seed` into NetBox via the API during NetBox bootstrap when the file exists (skipped with a notice otherwise)
- Re-run `sudo bash bootstrap/labprovider.sh --netbox` after changing `config/dns.seed` if you want the changes reflected in NetBox

API tokens (NetBox 4.6):

- NetBox 4.6 hashes API tokens (v2 tokens) and requires a pepper. Bootstrap generates one (or materializes the optional `NETBOX_API_TOKEN_PEPPER`) and persists it at `NETBOX_DIR/secrets/api_token_pepper`, injecting it into the container as `API_TOKEN_PEPPER_1`. The persisted file is authoritative on re-runs. Do not change or delete it once tokens exist: changing the pepper invalidates every existing API token, including the dns-sync token.
- v2 tokens are used as the composite `nbt_<key>.<token>` with an `Authorization: Bearer` header. The `token` part is only returned at provisioning time. The legacy `Token <key>` header fails against 4.6 with 403 "Invalid v1 token".
- `--netbox` auto-provisions a dedicated API token for dns-sync (description "labprovider dns-sync") and stores the composite at `DNS_SYNC_SECRETS_DIR/netbox.token` (mode `0600`). A stored, still-valid token is reused, so an operator-placed token (for example decrypted via SOPS/age) wins over auto-provisioning. Provisioning is skipped with a notice when `DNS_SYNC_SECRETS_DIR` is unset, keeping `--netbox` deployable standalone.

IPAM behavior:

- `LABPROVIDER_FQDN` is used as the canonical `dns_name` for the shared labprovider host IP object
- Built-in labprovider service FQDNs are stored in that canonical host IP object description
- Built-in service FQDNs remain service endpoints on the same host
- The canonical labprovider host IP object is created explicitly from `HOST_IP` and `LABPROVIDER_FQDN`, not from DNS record imports
- Prefix objects are created when CIDR information is available
- IP address objects use the actual configured mask when CIDR is known, for example `192.168.12.121/24`
- `/32` is used only when subnet information is not available
- One NetBox IP address object is created per unique address value
- Built-in labprovider service FQDNs share the canonical host IP object instead of creating duplicates

This canonical host-IP model is NetBox seeding behavior only. It does not require Technitium to be deployed.

### SeaweedFS S3

- Single-node S3-compatible object storage
- Exposed at `http://<S3_FQDN>:<S3_PORT>` (no TLS by default)
- Data persisted under `S3_DATA_DIR`

Bucket creation example for Velero:

The S3 service must be deployed first:

```bash
sudo bash bootstrap/labprovider.sh --s3
```

Install AWS CLI on macOS:

```bash
brew install awscli
```

Install AWS CLI on Debian/Ubuntu:

```bash
sudo apt-get update
sudo apt-get install -y awscli
```

Configure an AWS CLI profile using the S3 credentials from `config/labprovider.env`:

```bash
aws configure --profile labprovider-s3
```

Use:

```
AWS Access Key ID: <S3 access key>
AWS Secret Access Key: <S3 secret key>
Default region name: us-east-1
Default output format: json
```

Create a `velero-backups` bucket:

```bash
aws --profile labprovider-s3 \
  --endpoint-url http://<S3_FQDN>:<S3_PORT> \
  s3api create-bucket \
  --bucket velero-backups
```

Verify the bucket:

```bash
aws --profile labprovider-s3 \
  --endpoint-url http://<S3_FQDN>:<S3_PORT> \
  s3api list-buckets
```

### SFTPGo

- Single-node SFTP service via Docker Compose
- Requires step-ca to be initialized first for the HTTPS admin UI certificate
- Exposes:
  - SFTP endpoint
  - Client UI over `https://<SFTP_FQDN>:<SFTP_ADMIN_PORT>/web/client/login`
  - Admin UI over `https://<SFTP_FQDN>:<SFTP_ADMIN_PORT>/web/admin/login`
- Uses a step-ca-issued certificate for the HTTPS admin UI
- Stores the SFTPGo UI certificate under `SFTP_CERT_DIR`
- Bootstraps the initial admin UI user from `SFTP_ADMIN_USER` and `SFTP_ADMIN_PASSWORD`
- Default admin bootstrap applies only when no SFTPGo admin user already exists
- Optionally creates one backup user when `SFTP_BACKUP_USERNAME`, `SFTP_BACKUP_PASSWORD`, and `SFTP_BACKUP_HOME_DIR` are all set
- Existing backup users are left unchanged on later bootstrap runs

The SFTP protocol service remains separate from the HTTPS UI configuration.

### Dashboard (read-only)

`services/control-plane` is a **read-only** "current state" view of the labprovider
services. Deploy it with `--dashboard` (also run by `--all`, last) or run it
standalone with `services/control-plane/scripts/run.sh` (see
`services/control-plane/README.md`). It has its own listener and does not alter any
other service.

- **What it shows.** Five panels, each fetched on page load under its own short
  timeout and isolated so a dead or unconfigured source renders "unavailable" /
  "not configured" without blanking the page:
  1. Certificates (step-ca) - active certs, subject/SANs, provisioner,
     notBefore/notAfter, days-to-expiry against a warn threshold. Reads step-ca's
     dedicated postgres over `127.0.0.1:<CA_POSTGRES_PORT>` with a `SELECT`-only
     role, decoding the opaque cert blobs (see `STEPCA_STORAGE.md`). The RO
     password is provisioned by `--dashboard` into `CONTROL_PLANE_SECRETS_DIR`.
  2. DNS (Technitium) - zones, managed record counts, forwarder, TLS reachability.
  3. IPAM (NetBox) - prefix/IP counts and the `dns_name` inventory.
  4. Services (Docker) - container state/health/uptime/image for the stacks.
  5. Recent errors - a bounded per-container log tail, parsing `dns-sync`'s slog
     JSON for `level>=error`.
- **How to run it.** Add the `CONTROL_PLANE_*` block from
  `config/labprovider.env.example` to your `config/labprovider.env`, then
  `sudo bash bootstrap/labprovider.sh --dashboard` (issues the cert from
  step-ca, brings up the stack, verifies HTTPS, and publishes `CONTROL_PLANE_FQDN`).
  The scoped read-only tokens are optional - without them the NetBox/Technitium
  panels show "not configured". Standalone use is still supported via
  `services/control-plane/scripts/run.sh`. HTTPS is default; a missing cert logs a
  warning and falls back to HTTP rather than failing to start.
- **Security posture.** Read-only everywhere (no upstream write path). It uses a
  **dedicated minimum-read-scope NetBox token** (never the dns-sync/bootstrap
  admin token), a scoped Technitium token, the step-ca DB read-only via snapshot,
  and the Docker socket mounted `:ro`. Tokens come from files/env, never
  hardcoded or logged. It serves HTTPS with a step-ca-issued cert (HTTP fallback
  with a logged warning in a lab). **v1 has no auth on the UI itself** - this is
  acceptable only on a trusted internal lab network. **TODO: front it with auth
  (the repo's IdP or a reverse proxy) before any non-lab use.**
- **Phase 2 (out of v1 scope).** History/collector (time series) and UI
  authentication. (The `--dashboard` bootstrap module and `--all` inclusion have
  landed.)

## Module Reference

All flags are passed to `sudo bash bootstrap/labprovider.sh <flag>`. "Depends on" lists other labprovider modules only; every module also needs `config/labprovider.env`.

| Flag | Purpose | Depends on | Data / runtime dirs | Ports | Secrets it creates | `--remove` behavior |
|------|---------|------------|---------------------|-------|--------------------|---------------------|
| `--ntp` | Chrony NTP server | none | `/etc/chrony/chrony.conf` | 123/udp | none | not supported |
| `--rsyslog` | Central syslog collector | none | `SYSLOG_LOG_DIR`, `/etc/rsyslog.d/labprovider.conf` | `SYSLOG_PORT`/udp+tcp | none | not supported |
| `--ca` | step-ca private CA | none | `CA_DATA_DIR`; runtime under `WORKDIR/step-ca` | `CA_PORT`/tcp | CA password file (`CA_PASSWORD_FILE`) | removes runtime dir; preserves `CA_DATA_DIR` (keys, password) |
| `--technitium` | Containerized DNS server | `--ca` | `TECHNITIUM_DATA_DIR`, `TECHNITIUM_CERT_DIR`; runtime under `WORKDIR/technitium` | 53/tcp+udp, `TECHNITIUM_HTTP_PORT`/tcp, `TECHNITIUM_HTTPS_PORT`/tcp | pfx password (`TECHNITIUM_CERT_DIR/technitium-pfx-password`), dns-sync API token (`DNS_SYNC_SECRETS_DIR/technitium.token`) | removes runtime dir, restores `systemd-resolved`; preserves data, certs, and token |
| `--depot` | VCF offline depot (nginx) | `--ca` | `DEPOT_DATA_DIR`, `DEPOT_CERT_DIR`, `DEPOT_AUTH_DIR`; runtime under `WORKDIR/depot` | `DEPOT_HTTP_PORT`/tcp, `DEPOT_HTTPS_PORT`/tcp | `htpasswd` under `DEPOT_AUTH_DIR` | removes runtime dir and `htpasswd`; preserves data and certs |
| `--keycloak` | Keycloak identity provider | `--ca` | `KEYCLOAK_DIR`; runtime under `WORKDIR/keycloak` | `KEYCLOAK_PORT`/tcp | none (credentials come from env) | removes runtime dir; preserves `KEYCLOAK_DIR` (certs, data) |
| `--authentik` | Authentik identity provider (OIDC + SCIM) | `--ca` | `AUTHENTIK_DIR`; runtime under `WORKDIR/authentik` | `AUTHENTIK_PORT`/tcp | none (credentials come from env) | removes runtime dir; preserves `AUTHENTIK_DIR` (certs, data, postgres) |
| `--netbox` | NetBox IPAM/DCIM source of truth | `--ca` | `NETBOX_DIR`, `NETBOX_MEDIA_DIR`, `NETBOX_POSTGRES_DATA_DIR`, `NETBOX_REDIS_DATA_DIR` (runtime files live in `NETBOX_DIR`) | `NETBOX_PORT`/tcp | API token pepper (`NETBOX_DIR/secrets/api_token_pepper`), dns-sync token (`DNS_SYNC_SECRETS_DIR/netbox.token`) | removes compose file, nginx.conf, and `NETBOX_DIR/certs`; preserves media, postgres, redis, and secrets |
| `--s3` | SeaweedFS S3-compatible storage | none | `S3_DATA_DIR`; runtime under `WORKDIR/s3` | `S3_PORT`/tcp | none (credentials come from env) | removes runtime dir; preserves `S3_DATA_DIR` |
| `--sftp` | SFTPGo file transfer | `--ca` | `SFTP_DATA_DIR`, `SFTP_HOME_DIR`, `SFTP_CERT_DIR`; runtime under `WORKDIR/sftpgo` | `SFTP_PORT`/tcp, `SFTP_ADMIN_PORT`/tcp | none (credentials come from env) | removes runtime dir; preserves data, home, and certs |
| `--dns-sync` | NetBox-to-Technitium reconcile loop | `--ca`, `--technitium`, `--netbox` | `DNS_SYNC_DIR`, `DNS_SYNC_SECRETS_DIR`; runtime under `WORKDIR/dns-sync` | none (host networking, outbound only) | none (consumes tokens created by `--netbox` and `--technitium`) | removes runtime dir; preserves `DNS_SYNC_SECRETS_DIR` |
| `--dashboard` | Read-only "current state" view of the services | `--ca` (reads `--netbox`, `--technitium`, `--dns-sync`, Docker when present) | `CONTROL_PLANE_CERT_DIR`, `CONTROL_PLANE_SECRETS_DIR`; runs the compose under `services/control-plane` | `CONTROL_PLANE_ADDR` port (host networking) | none (issues its own leaf cert; scoped read-only tokens are operator-placed and optional) | brings the container down; preserves `CONTROL_PLANE_CERT_DIR` and `CONTROL_PLANE_SECRETS_DIR` |
| `--all` | Deploy everything except dns-sync (dashboard included, last) | n/a | see individual modules | see individual modules | see individual modules | `--all --remove` removes the dashboard, SFTPGo, S3, NetBox, Authentik, Keycloak, depot, and step-ca only |

Notes:

- `--technitium` runs right after `--ca` under `--all`
- Firewall rules are added with `ufw allow` for each service port; failures are ignored when ufw is absent

## Secrets Inventory

Every secret labprovider generates or persists, where it lives, and what losing or regenerating it means:

| Secret | Path | Owner / mode | Created by | Consequence of loss or regeneration |
|--------|------|--------------|------------|--------------------------------------|
| CA password | `CA_PASSWORD_FILE` (default `CA_DATA_DIR/secrets/password.txt`) | `1000:1000`, `0600` | `--ca` (from `CA_PASSWORD` or generated) | Without it the CA key cannot be decrypted: step-ca stops starting and no certificates can be issued or renewed. It cannot be regenerated; losing it means reinitializing the CA (delete `CA_DATA_DIR` contents) and re-running every certificate-consuming module, then redistributing the new root certificate. |
| NetBox API token pepper | `NETBOX_DIR/secrets/api_token_pepper` | root, `0600` | `--netbox` (from optional `NETBOX_API_TOKEN_PEPPER` or generated) | Changing or deleting it invalidates every existing NetBox API token, including the dns-sync token. Recover by re-running `--netbox` (provisions a fresh dns-sync token) and re-issuing any operator tokens. |
| dns-sync NetBox token | `DNS_SYNC_SECRETS_DIR/netbox.token` (composite `nbt_<key>.<token>`) | `1000:1000`, `0600` | `--netbox` (or operator-placed via SOPS/age) | dns-sync stops reconciling (NetBox reads fail). Re-run `--netbox` to provision a replacement; old tokens with the description "labprovider dns-sync" are retired automatically. |
| dns-sync Technitium token | `DNS_SYNC_SECRETS_DIR/technitium.token` | `1000:1000`, `0600` | `--technitium` (or operator-placed via SOPS/age) | dns-sync stops writing to Technitium. Re-run `--technitium` to provision a replacement (idempotent; a still-valid stored token is reused). |
| Technitium pfx password | `TECHNITIUM_CERT_DIR/technitium-pfx-password` | `1000:1000`, `0600` | `--technitium` | Needed to rebuild and open `technitium.pfx`. If lost, delete it together with `technitium.pfx` and re-run `--technitium`; a new password and bundle are generated and re-applied via the settings API. |
| Depot htpasswd | `DEPOT_AUTH_DIR/htpasswd` | root, `0644` | `--depot` (from `DEPOT_BASIC_AUTH_USER`/`_PASSWORD`) | Depot basic auth fails until recreated; regenerated from env on every `--depot` run. |

Secrets that live only in `config/labprovider.env` (admin passwords, `NETBOX_SECRET_KEY`, `AUTHENTIK_SECRET_KEY`, S3 keys, and so on) are the operator's responsibility; the file is gitignored but stored in plaintext on the host.

## Troubleshooting

Real failure modes with the messages they produce:

### Port 53 is already in use

```text
Error: Port 53 is already in use and labprovider will not stop the holder automatically.
```

`--technitium` preflights port 53. If `systemd-resolved`'s stub listener holds it, the module disables the stub listener automatically; any other holder (a leftover unbound, dnsmasq) must be stopped manually before re-running. Check `ss -lntup 'sport = :53'`.

### step-ca did not initialize

```text
Error: step-ca did not initialize. Check: docker logs step-ca-step-ca-1
```

`--ca` waits for `CA_DATA_DIR/config/ca.json` and then the health endpoint. A partially initialized `CA_DATA_DIR` (for example certs present but no `config/ca.json`, or a password file the container user cannot read) keeps first-start initialization from running. Check the container logs; if the data dir is inconsistent, move aside or delete the contents of `CA_DATA_DIR` and re-run `--ca` (this reinitializes the CA and invalidates previously issued certificates).

### 403 "Invalid v1 token" / "Invalid v2 token" from NetBox

NetBox 4.6 rejects the legacy `Token <key>` header (`Invalid v1 token`) and rejects Bearer composites whose hash no longer matches (`Invalid v2 token`, typically after the API token pepper changed). Use `Authorization: Bearer nbt_<key>.<token>`, and if the pepper was regenerated, re-run `--netbox` so a fresh dns-sync token is provisioned.

### HTTP 400 when browsing NetBox by IP

Django only serves hosts listed in `ALLOWED_HOSTS`. `NETBOX_ALLOWED_HOSTS` defaults to the NetBox FQDN only, so `https://<host-ip>:<NETBOX_PORT>/` returns a plain `Bad Request (400)`. Browse by FQDN, or add the IP to `NETBOX_ALLOWED_HOSTS` and re-run `--netbox`.

### labprovider.env appears outdated

```text
Error: config/labprovider.env appears outdated.
Missing variables from config/labprovider.env.example:
```

After pulling a newer checkout, new variables in the example must be added to your local `config/labprovider.env` by hand; labprovider never modifies it. A mixed-version symptom of the same root cause is a module failing with `Missing required variable: <NAME>` for a variable your env file predates.

### dns-sync reconcile failures

`docker compose -f ${WORKDIR}/dns-sync/docker-compose.yml logs -f` shows structured JSON logs. `status 403` from NetBox means the stored token is no longer valid (see the pepper note above); `invalid-token` from Technitium means `technitium.token` was revoked. Re-run `--netbox` or `--technitium` to provision replacements.

## VCF Lab Companion

labprovider provides a lightweight external infrastructure services platform for VMware Cloud Foundation lab and PoC environments.

VCF depends on external services that are not provided by the platform itself.

### Pre-deployment requirements

- DNS for forward and reverse resolution
- NTP for time synchronization

### Post-deployment operational dependencies

- identity provider for OIDC or federation
- centralized logging
- certificate authority
- optional object storage and file transfer services

labprovider packages these services into a single reproducible node so VCF labs can be built without depending on external enterprise infrastructure.

This is especially useful in isolated, homelab, and lab environments where the supporting service plane must be self-contained.

## Design Trade-offs

labprovider is intentionally single-node and not highly available.

It prioritizes:

- simplicity
- reproducibility
- low resource footprint

Over:

- redundancy
- production-grade resilience
- orchestration complexity

It is opinionated for labs and PoCs, not for HA production deployment patterns.

## Repository Layout

```text
bootstrap/
  authentik.sh
  ca.sh
  depot.sh
  dns-sync.sh
  keycloak.sh
  netbox.sh
  ntp.sh
  labprovider.sh
  rsyslog.sh
  s3.sh
  sftp.sh
  technitium.sh

config/
  dns.seed.example
  labprovider.env.example

services/
  dns-sync/       Go source for the dns-sync and dns-seed binaries (image built locally by --dns-sync)
  dashboard/      Go source for the read-only dashboard (image + compose + scripts here; deployed by --dashboard or run standalone)

templates/
  chrony.conf.tpl
  rsyslog.conf.tpl
  docker-compose.step-ca.yml.tpl
  docker-compose.technitium.yml.tpl
  docker-compose.dns-sync.yml.tpl
  docker-compose.depot.yml.tpl
  docker-compose.keycloak.yml.tpl
  keycloak-realm.json.tpl
  docker-compose.authentik.yml.tpl
  authentik-blueprint.yaml.tpl
  docker-compose.netbox.yml.tpl
  docker-compose.s3.yml.tpl
  docker-compose.sftpgo.yml.tpl
  depot-nginx.conf.tpl
  netbox-nginx.conf.tpl

docs/
  images/         Architecture diagram sources and exports
```

## Development Safeguards

This repository can optionally be used with local `pre-commit` hooks to catch hygiene issues and prevent accidentally committing secrets.

Install:

```bash
pipx install pre-commit
pre-commit install
```

Run manually:

```bash
pre-commit run --all-files
```

The configured Gitleaks hook scans for secrets before commits are created.

## Failure Handling

The bootstrap process fails fast if:

- required files are missing
- package installation fails
- required commands are unavailable
- validation fails
- configuration is malformed

This keeps deployments predictable and reproducible.

## Operational Notes

- Use FQDNs instead of raw IPs where possible
- Ensure both forward and reverse DNS are configured
- Import `keycloak-ca-chain.pem` into VCF when configuring OIDC
- Use `keycloak-ca-roots.pem` only when a roots-only trust bundle is required
- Built-in labprovider service DNS records are generated automatically; reserve `config/dns.seed` for external and custom records

### DNS behavior warning

Deploying DNS takes over host name resolution: `--technitium` disables the `systemd-resolved` stub listener (via a marked drop-in) and points `/etc/resolv.conf` at Technitium after verifying resolution works; `--technitium --remove` restores the stock configuration.

## Scope

labprovider focuses on a simple, modular, and reproducible way to deploy shared infrastructure services on a single host for lab and PoC environments.

It is intentionally:

- shell-based
- template-driven
- explicit
- single-node
- easy to reason about

It does not aim to introduce orchestration layers, HA patterns, or broad production abstractions.
