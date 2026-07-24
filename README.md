# labprovider

labprovider is a lightweight, single-node platform for standing up shared infrastructure services on a single dedicated host. It provides a self-contained infrastructure services layer for lab environments.

It is designed for lab and proof-of-concept environments, especially VMware Cloud Foundation (VCF). Services (all containerized via Docker Compose):

- DNS via Technitium (NetBox-driven)
- dns-sync for reconciling NetBox IPAM records into Technitium
- Chrony for NTP (containerized; image built locally)
- rsyslog for centralized syslog collection (containerized; image built locally)
- step-ca for a lightweight private certificate authority (dedicated PostgreSQL backend)
- VCF offline depot served by nginx
- Keycloak for identity
- Authentik for identity federation with OIDC and outbound SCIM 2.0 provisioning
- Zitadel for identity with OIDC (Management API-provisioned bootstrap client, optional multi-tenant orgs)
- NetBox for IPAM, DCIM, and infrastructure source-of-truth
- SeaweedFS for S3-compatible object storage
- SFTPGo for SFTP file transfer
- The labprovider control plane: a web UI with a configuration wizard, service selection + deployment with live progress, a read-only dashboard, CSR signing, and an optional Microsoft-CA web-enrollment emulator for VCF

## labprovider v2: the control plane

labprovider is derived from Rutger Blom's provider-box. Where the original relied on a CLI bash bootstrap, labprovider v2 replaces it entirely with a docker-based control plane that owns config, deploy, and dashboard. One script installs it; everything else happens in the browser:

```bash
git clone https://github.com/dsjodin/labprovider.git
cd labprovider
sudo bash install.sh
```

`install.sh` is the only shell in v2. It installs Docker if absent (Debian and Ubuntu), does the one-time host preparation (disables the systemd-resolved stub listener so Technitium can own port 53, disables systemd-timesyncd because chrony runs containerized), builds the control-plane image from the checkout, and starts it (root, host network, with the docker socket, `/opt/labprovider`, and `/host/etc` mounted). It prints the UI URL when done (port 8445 by default).

Then, in the UI:

1. **`/config`** - edit or paste `labprovider.env` (or download it, fill it out locally, and paste it back), validate (every problem is reported at once, per variable), and save. Optional external DNS records (`dns.seed`) are managed on the same page.
2. **`/deploy`** - tick the services you want (dependencies are added automatically), press Deploy, and watch the live log stream over SSE. "Select all" deploys the full catalog in dependency order: chrony, rsyslog, ca, technitium, depot, keycloak, authentik, zitadel, netbox, s3, sftp, dns-sync.
3. **`/`** - the dashboard: certificates (step-ca), DNS zones (Technitium), IPAM (NetBox), container state, and recent errors at a glance.
4. **`/csr`** - paste a PKCS#10 CSR and have step-ca sign it, returning the full chain.

After the CA is deployed the control plane issues its own certificate; restart the container (`docker restart labprovider-control-plane`) to serve the UI over HTTPS.

**The UI has no authentication (v1).** Run it on a trusted lab network only.

### Required open ports

`install.sh` and the control plane do not manage the firewall. If the host runs ufw or similar, open the service ports you deploy:

| Service | Ports |
|---------|-------|
| Control plane UI | 8445/tcp |
| MSCA emulator (optional) | 8446/tcp (VMSCA_PORT) |
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

### Reverse proxy (Traefik)

With `TRAEFIK_ENABLE=true`, a single Traefik ingress on `:80`/`:443` fronts the
HTTP(S) services so you reach each at its bare FQDN with no port -
`https://netbox.sddc.lab`, `https://certsrv.sddc.lab/certsrv`, and so on. Traefik
terminates TLS with one step-ca-issued `*.<SEARCH_DOMAIN>` wildcard leaf (served
as its default certificate) and routes by `Host`:

- bridge service stacks are discovered via docker labels on a shared external
  `proxy` network, created by `install.sh`;
- the host-networked control plane and the certsrv emulator are wired through
  Traefik's file provider (reachable at `https://dashboard.<domain>` and
  `https://certsrv.<domain>`).

Because it holds `:80`/`:443`, open those in the firewall when Traefik is enabled.
The non-HTTP services keep their own ports regardless: DNS (53), NTP (123), syslog
(514), SFTP (2022), and step-ca (9000). This is a lab-grade setup: Traefik talks
plain HTTP to backends over the `proxy` network.

Migration is staged. Currently fronted: the control plane, certsrv, SeaweedFS S3
(path-style: `https://s3.<domain>/<bucket>`), and the SFTPGo admin UI. The
remaining stacks (depot, netbox, keycloak, authentik, zitadel) still publish their
own ports and are migrated in later increments.

## Overview

![labprovider Overview](docs/images/labprovider-overview.png)
*labprovider v2 architecture: the control plane, the containerized Docker Compose services, the host foundation, and external dependencies.*

## Table of Contents

- [Configuration](#configuration)
- [Host Assumptions](#host-assumptions)
- [Choosing Services](#choosing-services)
- [Runtime Model](#runtime-model)
- [Deploying and Removing Services](#deploying-and-removing-services)
- [Upgrading Services](#upgrading-services)
- [Configuration Model](#configuration-model)
- [Dependency Updates](#dependency-updates)
- [Service Notes](#service-notes)
- [Service Reference](#service-reference)
- [Secrets Inventory](#secrets-inventory)
- [Troubleshooting](#troubleshooting)
- [VCF Lab Companion](#vcf-lab-companion)
- [Design Trade-offs](#design-trade-offs)
- [Repository Layout](#repository-layout)
- [Development Safeguards](#development-safeguards)
- [Failure Handling](#failure-handling)
- [Operational Notes](#operational-notes)
- [Scope](#scope)

## Configuration

All configuration is a single flat file, `labprovider.env`, edited in the control plane's `/config` wizard and saved to `/opt/labprovider/control-plane/labprovider.env`. The shipped `config/labprovider.env.example` is the schema source of truth and the completeness reference; a deploy refuses a config missing any variable the example defines.

### Filling it out

Open `/config` and either edit the pre-filled example in place or download it, fill it out locally, and paste it back. The wizard validates on save and reports every problem at once, per variable.

To quickly replace all placeholder passwords with a single value before pasting the file in, you can pre-process a local copy:

```bash
cp config/labprovider.env.example labprovider.env
PASSWORD='VMware1!VMware1!' \
SECRET_KEY=$(openssl rand -base64 48 | sed 's/[&]/\\&/g') \
&& sed -i \
  -e "s|CHANGE_ME_WITH_AT_LEAST_50_RANDOM_CHARACTERS_BEFORE_USE|$SECRET_KEY|g" \
  -e "s|CHANGE_ME|$PASSWORD|g" \
  labprovider.env
```

### External DNS records (optional)

To publish external/custom DNS records (VCF nodes, gateways, and other non-labprovider hosts), edit the `dns.seed` block on the same `/config` page. It is imported into NetBox during the netbox and dns-sync deploys, and the reconcile loop then publishes the records via Technitium.

Built-in labprovider service FQDNs are generated automatically from the `*_FQDN` values in `labprovider.env`. You do not add built-in service records to `dns.seed`.

## Host Assumptions

labprovider assumes:

- Ubuntu or Debian-based host (labprovider is developed and tested on Debian GNU/Linux 13 (trixie), but should work on recent Ubuntu releases)
- root or `sudo` access
- static IP and prefix already configured on the host
- network connectivity from lab consumers to this host
- access to Debian or Ubuntu package repositories (required packages are installed automatically by `install.sh`)
- access to Docker package repositories (required for containerized services)

labprovider uses Docker Compose via `docker compose` (Compose v2). `install.sh` installs Docker idempotently:

- If Docker with Compose v2 already works, existing Docker packages are left untouched and the `docker` service is enabled
- If Docker exists but Compose v2 is missing, only `docker-compose-plugin` is installed
- If Docker is absent, Docker CE is installed from Docker's official apt repository for the detected distribution (`debian` or `ubuntu`); other distributions fail fast with a message to install Docker manually first

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

### Optional depending on use case

- SeaweedFS for S3-compatible storage
- Authentik as an alternative identity provider when OIDC plus outbound SCIM 2.0 provisioning is required (for example VCF 9 identity federation)
- Zitadel as an alternative OIDC identity provider, optionally multi-tenant
- The MSCA emulator when VCF should replace certificates automatically via its "Microsoft CA" integration

The `/deploy` page adds dependencies automatically when you select a service. Services otherwise remain independently deployable:

- NetBox does not require Technitium
- S3 and SFTPGo do not require unrelated service configuration
- step-ca is an intentional dependency for services that use labprovider-issued TLS certificates
- dns-sync intentionally depends on Technitium and NetBox; it is the bridge between them

## Runtime Model

Everything is containerized. The control plane is a Go binary in a container (root, host network, with the docker socket, `/opt/labprovider`, and `/host/etc` mounted); it execs the bundled docker CLI (compose v2) against the host daemon. Each service is a Docker Compose stack.

| Service   | Runtime |
|-----------|---------|
| Control plane | Docker container (built locally from the checkout by `install.sh`) |
| Chrony   | Docker Compose (image built locally; `cap_add: SYS_TIME` only) |
| rsyslog  | Docker Compose (image built locally) |
| step-ca  | Docker Compose (dedicated `stepca-postgres` backend) |
| Technitium DNS | Docker Compose |
| dns-sync | Docker Compose (image built locally from `services/dns-sync`) |
| VCF offline depot | Docker Compose |
| Keycloak | Docker Compose |
| Authentik | Docker Compose (server, worker, PostgreSQL) |
| Zitadel  | Docker Compose (core, login v2, PostgreSQL, nginx terminator) |
| NetBox   | Docker Compose (NetBox, PostgreSQL, Redis, HTTPS terminator) |
| SeaweedFS (S3) | Docker Compose |
| SFTPGo   | Docker Compose |

chrony, rsyslog, dns-sync, and the control plane have no official upstream image, so their images are built locally by the engine (or by `install.sh`) from embedded or checkout sources; no registry access is needed for them.

## Deploying and Removing Services

Deploy from the `/deploy` page: select services (dependencies are added automatically) and press Deploy. Deploys run sequentially in dependency order, single-flight, with progress streamed live. Docker is the source of truth for what is running; `state.json` is advisory deploy history only.

Removing a service (from the `/deploy` page) stops its containers with `docker compose down` and deletes generated runtime files under `WORKDIR`. Persistent data directories, certificates, and operator secrets are always preserved. The remove path is idempotent and safe to run multiple times.

Removing Technitium additionally restores the stock host resolver configuration: it deletes the labprovider `systemd-resolved` drop-in, points `/etc/resolv.conf` back at the `systemd-resolved` stub, and restarts `systemd-resolved`.

See [Service Reference](#service-reference) for exactly what each service's remove deletes and preserves.

## Upgrading Services

Container image versions are pinned in `labprovider.env` (see [Dependency Updates](#dependency-updates)). To move a containerized service to a new image version, change its `*_IMAGE` pin in `/config`, save, and redeploy that service from `/deploy`; the deploy re-runs its configuration idempotently and the persisted data directory carries state forward.

Before a major-version bump, review the upstream project's release notes for breaking changes to the parts labprovider drives (APIs, settings parameters, data directory format, ports, and the container's user/permissions model), and take a backup of the service's persistent data directory so a rollback is possible.

General upgrade procedure for a containerized service:

```bash
# 1. Back up the persistent data directory (rollback insurance)
sudo tar czf /opt/labprovider/<service>-backup-$(date +%F).tgz -C /opt/labprovider <service>

# 2. Update the pinned image version in /config (edit the relevant *_IMAGE line;
#    never use :latest) and save.

# 3. Remove, then redeploy the single service from /deploy.
```

Rollback: remove the service, restore the pre-upgrade data-directory backup, repin the previous image version, and redeploy.

### Technitium DNS (13.x -> 15.x)

Reviewed release: `docker.io/technitium/dns-server:15.3.0` (upgrade from `13.4.2`, assessed 2026-07-08). The API surface labprovider uses (web service TLS settings, `createToken`, forwarder settings, zone/record CRUD), the data directory layout, ports, and the container uid are unchanged or backward-compatible; the query-string token form still works. A 13.x data directory migrates in place on first start of 15.x (existing zones, records, and API tokens are preserved), so the standard redeploy procedure above applies.

- **Forward-only.** Once 15.x starts on a data directory it rewrites the `*.config` files; a 15.x data directory must NOT be run under 13.x afterward. Rollback to 13.x requires restoring the pre-upgrade backup taken in step 1 - there is no in-place downgrade.
- **DNS stays up across the swap.** The technitium deploy pre-pulls the pinned image before stopping the running container, so when Technitium is the host resolver the image is already cached when DNS briefly goes down during recreate. If the pull fails, the deploy aborts with the old server still running.
- **Behavioral deltas that do not affect labprovider** (documented in `services/dns-sync/TECHNITIUM_API.md`): built-in `internal` reverse zones no longer appear in `zones/list` on 15.x, and deleting a non-existent zone or record now returns an error instead of succeeding silently.

## Configuration Model

`labprovider.env` defines all service configuration. Validation is strict and lives in one schema table (`services/control-plane/internal/envfile/schema.go`): one entry per variable with its validator and the services that require it. The wizard reports every finding at once, per variable, before any deployment changes are made.

Pinned container image versions for Docker-based services are also defined centrally in `labprovider.env`.

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

The `dns.seed` block supports:

```text
<fqdn> <ip>
<fqdn> <ip/cidr>
```

Behavior:

- If a record includes CIDR information, labprovider can derive the surrounding subnet for NetBox
- If a record includes only a plain IP, labprovider imports the host address without guessing the subnet
- Built-in labprovider service records are generated automatically and should not be duplicated in `dns.seed`

### DNS model

The technitium deploy stands up the DNS server; the netbox and dns-sync deploys import `dns.seed` into NetBox (when set) and dns-sync runs a continuous NetBox-to-Technitium reconcile loop. After bring-up, NetBox is the source of truth for lab records; change records in NetBox, not in the seed.

Technitium forwards external queries to `DNS_FORWARDER`. It applies its default recursion policy, which serves RFC1918 (private) client networks; if the lab uses non-RFC1918 ranges, adjust the recursion access control list in the Technitium console so those clients can resolve.

Built-in labprovider service records are generated automatically from the `*_FQDN` values in `labprovider.env`: dns-sync synthesizes them into the desired record set on every reconcile. They are not stored in NetBox, which enforces global IP uniqueness and holds a single canonical host IP object (`LABPROVIDER_FQDN`); that object also remains the reverse PTR target for the host IP.

### Template rendering

Service configuration is rendered from Go text/template files embedded in the control-plane binary, with `missingkey=error`: a reference to an unset variable fails the render rather than silently producing an empty string. Golden render tests pin every template's output.

## Dependency Updates

Container image versions are centrally defined in `config/labprovider.env.example` and kept up to date using Renovate in the labprovider repository.

Users consume updated versions by pulling changes to the repository and rebuilding the control-plane image (`install.sh`), then repinning `*_IMAGE` values in `/config` as desired.

## Service Notes

### Technitium DNS

- Runs the Technitium DNS server via Docker Compose
- Requires step-ca to be initialized first
- Serves DNS on port 53 (TCP and UDP)
- Web console over HTTP at `http://<DNS_FQDN>:<TECHNITIUM_HTTP_PORT>` (`5380` by default)
- Web console and API over HTTPS at `https://<DNS_FQDN>:<TECHNITIUM_HTTPS_PORT>` (`53443` by default), using a step-ca-issued certificate
- Persists zone and settings data under `TECHNITIUM_DATA_DIR` and certificates under `TECHNITIUM_CERT_DIR`
- Configures `DNS_FORWARDER` as the upstream forwarder over the settings API and verifies external resolution before pointing the host at itself. The technitium deploy is the only owner of the forwarder setting; dns-sync never touches it.

Deploy behavior:

- Technitium requires its web TLS certificate as PKCS#12, so the deploy converts the step-ca PEM material into `technitium.pfx` with a generated password persisted at `TECHNITIUM_CERT_DIR/technitium-pfx-password`. The bundle is rebuilt automatically whenever the certificate is reissued.
- An API token for dns-sync is created via the Technitium API and stored at `DNS_SYNC_SECRETS_DIR/technitium.token` (mode `0600`). A stored token is validated and reused while Technitium still accepts it.
- The first-boot `admin`/`admin` credentials are rotated to `TECHNITIUM_ADMIN_PASSWORD` on first deploy and used on re-runs.
- On stock Ubuntu, the `systemd-resolved` stub listener holds `127.0.0.53:53`; `install.sh` disables the stub listener up front. If any other service holds port 53, the deploy fails fast and does not stop it automatically.
- After the DNS listener, forwarder, HTTPS endpoint, and API token are all verified, the deploy points `/etc/resolv.conf` at Technitium (`127.0.0.1`).

Removal behavior:

- Removing Technitium runs `docker compose down`, removes runtime files under `WORKDIR/technitium`, and restores the stock `systemd-resolved` configuration (stub listener re-enabled, `/etc/resolv.conf` pointed back at the stub)
- Persistent data in `TECHNITIUM_DATA_DIR` and certificates in `TECHNITIUM_CERT_DIR` (including the pfx bundle and its password) are preserved

### dns-sync

- Continuously reconciles DNS records from NetBox IPAM into Technitium
- Requires step-ca, Technitium, and NetBox first; both readiness gates pin the lab FQDNs to `127.0.0.1`, so nothing depends on the zone it is about to populate
- The container image (`DNS_SYNC_IMAGE`) is built locally from `services/dns-sync` (baked into the control-plane image); no registry access is needed
- Runs with host networking so its `127.0.0.1` pins reach the host-published NetBox and Technitium ports
- Reconciles every `DNS_SYNC_INTERVAL` (for example `30s`, `5m`, `1h`): one A record per NetBox IP object with a `dns_name`, one PTR per IP (using a deterministically chosen canonical name when several names share an IP), and the built-in service records below
- Built-in labprovider service records are synthesized from the `*_FQDN` values in `labprovider.env` on every reconcile pass. They are deliberately not stored in NetBox (NetBox enforces global IP uniqueness; the host IP is one canonical object with `LABPROVIDER_FQDN` as `dns_name`), and they are A records only so `LABPROVIDER_FQDN` stays the sole PTR target.
- Imports the managed `dns.seed` into NetBox before starting the loop when it is set (idempotent; skipped with a notice otherwise)
- Expects API tokens at `DNS_SYNC_SECRETS_DIR/netbox.token` and `DNS_SYNC_SECRETS_DIR/technitium.token`. Both are auto-provisioned (by the netbox and technitium deploys respectively); placing decrypted tokens there out of band (for example via SOPS/age) is the operator override and wins while the token stays valid.
- After the first reconcile, the deploy verifies over real DNS that `LABPROVIDER_FQDN` and every built-in service FQDN resolve via Technitium
- Logs: `docker compose -f ${WORKDIR}/dns-sync/docker-compose.yml logs -f`

Removal behavior:

- Removing dns-sync runs `docker compose down` and removes runtime files under `WORKDIR/dns-sync`
- Secrets in `DNS_SYNC_SECRETS_DIR` are preserved

### Chrony

- Runs containerized (host networking, `cap_add: SYS_TIME` only); the image is built locally because there is no official chrony container
- Uses configured upstream NTP servers
- Provides NTP service to internal networks
- Persists drift data under `CHRONY_DIR`

### rsyslog

- Runs containerized (host networking); the image is built locally because there is no official rsyslog container
- Exposes centralized syslog via UDP and TCP
- Config is validated (`rsyslogd -N1`) before start
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
- Deploying step-ca configures the provisioner default and maximum X.509 certificate duration from `SERVICE_CERT_DURATION`

PostgreSQL backend:

- step-ca stores its state in a DEDICATED postgres container (`stepca-postgres`),
  never shared with NetBox/Authentik/Zitadel (module independence, CA isolation).
- step-ca uses postgres as an opaque key-value store (one table per bucket,
  each `nkey`/`nvalue` `BYTEA`), not a relational schema. Cert attributes live
  inside the `BYTEA` blobs, so anything reading the DB parses the blobs; it
  cannot filter/join on cert fields in SQL.
- The postgres owner password is supplied to step-ca via a mounted `.pgpass`
  file (`PGPASSFILE`), so it never appears in `ca.json`'s `dataSource` DSN.
- The postgres port is published on `127.0.0.1:<CA_POSTGRES_PORT>` only, for the
  host-networked control plane's read-only cert panel. It is never exposed off-host.
- Deploying step-ca also creates a read-only role (`CA_POSTGRES_RO_USER`) with `SELECT` on
  the cert tables only; the dashboard reads through it.
- CRL is enabled (`crl.enabled`) so revocation is served. The remote admin API
  is NOT enabled and there is no write/revoke path in this design.
- On first init the container self-initializes with badger, then the deploy
  rewrites `ca.json`'s `db` stanza to postgresql, restarts, and moves the unused
  badger dir aside (`db.pre-postgres.<timestamp>`, retained, not deleted).
  Switching backends does NOT migrate data: badger state does not move to
  postgres.

Important notes:

- `CA_PASSWORD` is convenient for lab use, but when set in `labprovider.env` it is still stored there in plaintext.
- Reinitialization requires deleting the contents of `CA_DATA_DIR`. The deploy
  refuses to run against an existing badger-backed CA: Phase 2 rebuilds on
  postgres rather than migrating in place.
- `CA_POSTGRES_DATA_DIR` MUST be a sibling of `CA_DATA_DIR`, never nested under
  it (the `chown -R 1000:1000 CA_DATA_DIR` step would corrupt postgres data).
- No repository-shipped static CA password file is required
- The root certificate is available from `/roots.pem`

Rebuild + reissue runbook (run on-host; the deploy does not do the destructive
steps for you):

1. Remove the CA from `/deploy` (stops the stack, preserves data).
2. Wipe the CA state (lab certs are disposable): remove `CA_DATA_DIR` and
   `CA_POSTGRES_DATA_DIR`. Wiping both keeps the new root and the empty postgres
   store consistent; the deploy refuses a new root against a non-empty store.
3. Redeploy step-ca - initializes on postgres, enables CRL, and creates the
   read-only role.
4. Reissue every service certificate against the new root by redeploying each
   certificate-consuming service, one at a time, verifying each before the next.
   The order is: technitium, netbox, authentik, zitadel, depot, sftp, then
   re-run dns-sync (its NetBox/Technitium tokens are reissued too). Keycloak (if
   deployed) reissues the same way. Verify each leaf chains to the new root, e.g.
   `openssl verify -CAfile "$CA_DATA_DIR/certs/root_ca.crt" <service-leaf>.crt`.
5. Confirm CRL is served: `curl --cacert "$CA_DATA_DIR/certs/root_ca.crt"
   --resolve "$CA_FQDN:$CA_PORT:127.0.0.1" https://$CA_FQDN:$CA_PORT/crl`.

Certificate issuance is DNS-independent by design. Every service that requests a certificate (Technitium, depot, Keycloak, Authentik, Zitadel, NetBox, SFTPGo, the control plane) pins `CA_FQDN` to `127.0.0.1` with `--add-host`/`--resolve` instead of resolving it, so certificates can be issued before any DNS backend exists. This relies on the single-node assumption: step-ca and every certificate-consuming service run on the same host, so `127.0.0.1` always reaches the CA. The dns-sync readiness gates and the deploy health checks use the same pinning for the same reason.

### CSR signing and the MSCA emulator

The control plane exposes step-ca's signing to two additional front doors, both going through the same `SignCSR` path (provisioner `admin`, full-chain guarantee) the deployers use:

- **`/csr` page and `POST /api/csr/sign`** - paste a PKCS#10 CSR and get the signed leaf plus chain back.
- **Microsoft-CA web-enrollment emulator** - a `certsrv`-shaped listener so VCF / SDDC Manager can automate certificate replacement using its "Microsoft CA" integration (step-ca offers no such interface natively). Enable it with `VMSCA_ENABLE=true`; it starts as a second listener on `VMSCA_PORT` (default 8446), reusing the control plane's own step-ca TLS leaf (which carries `VMSCA_FQDN` as an additional SAN, so the certsrv name validates over TLS), and serves the endpoints an ADCS web-enrollment client drives (`certfnsh.asp`, `certnew.cer`, `certnew.p7b`, `certcarc.asp`, and the `/certsrv/` credential probe) behind HTTP Basic Auth (`VMSCA_USERNAME`/`VMSCA_PASSWORD`). When enabled, `VMSCA_FQDN` (default `certsrv.sddc.lab`) is published in DNS as an A record to `HOST_IPV4` like every other service name. Point SDDC Manager's Certificate Authority at `https://<VMSCA_FQDN>:<VMSCA_PORT>/certsrv` with CA Type "Microsoft" and Template Name `VMSCA_TEMPLATE`. See `vcf-msca-emulation_design.md` for the full contract, risks, and validation.

### VCF offline depot

- Runs as a single-node nginx container via Docker Compose
- Requires step-ca to be initialized first
- Exposes:
  - HTTP over `http://<DEPOT_FQDN>:<DEPOT_HTTP_PORT>`
  - HTTPS over `https://<DEPOT_FQDN>:<DEPOT_HTTPS_PORT>`
- Uses a step-ca-issued certificate stored under `DEPOT_CERT_DIR`
- Stores the managed `htpasswd` file under `DEPOT_AUTH_DIR` (generated with a native APR1-MD5 implementation; no host package needed)
- Persists depot content under `DEPOT_DATA_DIR`
- Creates the expected `PROD/COMP`, `PROD/metadata`, and `PROD/vsan/hcl` directory layout during deploy
- Serves both HTTP and HTTPS directly with no forced redirect
- Protects `/PROD/metadata/`, `/PROD/COMP/`, and `/PROD/COMP/Compatibility/VxrailCompatibilityData.json` with basic auth
- Leaves `/PROD/vsan/hcl/`, `/healthz`, `/products/v1/bundles/all`, and `/products/v1/bundles/lastupdatedtime` accessible without authentication
- Renders runtime files under `WORKDIR/depot`

Removal behavior:

- Removing the depot runs `docker compose down`
- Generated runtime files under `WORKDIR/depot` are removed
- The managed `htpasswd` file is removed and recreated on the next deploy
- Persistent depot content under `DEPOT_DATA_DIR` is preserved
- step-ca-issued certificates under `DEPOT_CERT_DIR` are preserved

### Keycloak

- Runs via Docker Compose
- Requires step-ca to be initialized first
- Uses a certificate issued by step-ca
- Exposed at `https://<KEYCLOAK_FQDN>:<KEYCLOAK_PORT>` (`8443` by default)
- Seeds an opinionated initial realm from a repository-managed realm import on first deployment

Key files:

- `keycloak.crt` for the Keycloak HTTPS certificate file
- `keycloak.key` for the private key
- `keycloak-ca-chain.pem` for CA chain material
- `keycloak-ca-roots.pem` for roots-only trust use cases
- `keycloak-full-chain.pem` for VCF SSO certificate-chain upload

VCF SSO expects the full IdP TLS chain in leaf, intermediate, root order. Use `keycloak-full-chain.pem` for that upload field.

Realm bootstrap:

- Uses a repository-managed realm derived from a working Keycloak realm export and adapted for labprovider
- Imports one opinionated initial realm, one bootstrap group, and one baseline OIDC client for VCF-style integration
- Bootstraps one initial lab user in the bootstrap realm using `KEYCLOAK_BOOTSTRAP_USERNAME`, `KEYCLOAK_BOOTSTRAP_USER_PASSWORD`, and `KEYCLOAK_BOOTSTRAP_USER_EMAIL_DOMAIN`
- Seeds initial realm state only; it does not provide a generic realm-management framework
- Realm changes are only applied on initial bootstrap; existing realms are not reconciled or modified on subsequent runs

### Authentik

- Runs via Docker Compose with Authentik server, Authentik worker, and PostgreSQL
- Requires step-ca to be initialized first
- Intended for VMware Cloud Foundation 9 identity federation with OIDC authentication and outbound SCIM 2.0 provisioning (which Keycloak lacks)
- Runs in parallel with Keycloak and Zitadel on separate FQDNs and ports when more than one is deployed (including via "Select all"); federate VCF against one of them, using Authentik when SCIM provisioning is required
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

- Runs via Docker Compose as four containers: PostgreSQL 17, the Zitadel v4 core server, the `zitadel-login` (Login V2) container, and an nginx TLS terminator that fronts both (v4 dropped CockroachDB support)
- Requires step-ca to be initialized first
- Runs in parallel with Keycloak and Authentik on separate FQDNs and ports when more than one is deployed (including via "Select all")
- Exposed at `https://<ZITADEL_FQDN>:<ZITADEL_PORT>` (`7443` by default), served by the nginx terminator using the step-ca-issued certificate (mounted from `${ZITADEL_DIR}/certs/<ZITADEL_FQDN>`)
- The core runs plain HTTP behind the proxy (`--tlsMode external`, `ExternalSecure=true`); nginx routes `/ui/v2/login` to the login container and everything else to the core
- Persists application state in PostgreSQL 17 under `${ZITADEL_DIR}/postgres`
- `ZITADEL_MASTERKEY` must be EXACTLY 32 characters (Zitadel requirement)
- On first start Zitadel's FirstInstance init creates a human admin (`ZITADEL_ADMIN_USERNAME`/`ZITADEL_ADMIN_PASSWORD`), an admin service account whose PAT is written to `${ZITADEL_DIR}/machinekey/pat.txt`, and the `login-client` service account whose PAT (`${ZITADEL_DIR}/machinekey/login-client.pat`) the Login V2 container authenticates with
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
- Imports DNS records from the managed `dns.seed` into NetBox via the API during the netbox deploy when it is set (skipped with a notice otherwise)
- Redeploy NetBox after changing `dns.seed` if you want the changes reflected in NetBox

API tokens (NetBox 4.6):

- NetBox 4.6 hashes API tokens (v2 tokens) and requires a pepper. The deploy generates one (or materializes the optional `NETBOX_API_TOKEN_PEPPER`) and persists it at `NETBOX_DIR/secrets/api_token_pepper`, injecting it into the container as `API_TOKEN_PEPPER_1`. The persisted file is authoritative on re-runs. Do not change or delete it once tokens exist: changing the pepper invalidates every existing API token, including the dns-sync token.
- v2 tokens are used as the composite `nbt_<key>.<token>` with an `Authorization: Bearer` header. The `token` part is only returned at provisioning time. The legacy `Token <key>` header fails against 4.6 with 403 "Invalid v1 token".
- The netbox deploy auto-provisions a dedicated API token for dns-sync (description "labprovider dns-sync") and stores the composite at `DNS_SYNC_SECRETS_DIR/netbox.token` (mode `0600`). A stored, still-valid token is reused, so an operator-placed token (for example decrypted via SOPS/age) wins over auto-provisioning. The per-run superuser seeding token is retired at the end of the deploy.

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

Bucket creation example for Velero (deploy the S3 service first):

Install AWS CLI on macOS:

```bash
brew install awscli
```

Install AWS CLI on Debian/Ubuntu:

```bash
sudo apt-get update
sudo apt-get install -y awscli
```

Configure an AWS CLI profile using the S3 credentials from `labprovider.env`:

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
- Existing backup users are left unchanged on later deploys

The SFTP protocol service remains separate from the HTTPS UI configuration.

### Dashboard (read-only)

The dashboard is the control plane's `/` page: a **read-only** "current state" view of the labprovider services. It has its own listener and does not alter any other service. See `services/control-plane/README.md` for the full description.

- **What it shows.** Five panels, each fetched on page load under its own short
  timeout and isolated so a dead or unconfigured source renders "unavailable" /
  "not configured" without blanking the page:
  1. Certificates (step-ca) - active certs, subject/SANs, provisioner,
     notBefore/notAfter, days-to-expiry against a warn threshold. Reads step-ca's
     dedicated postgres over `127.0.0.1:<CA_POSTGRES_PORT>` with a `SELECT`-only
     role, decoding the opaque cert blobs (see `STEPCA_STORAGE.md`).
  2. DNS (Technitium) - zones, managed record counts, forwarder, TLS reachability.
  3. IPAM (NetBox) - prefix/IP counts and the `dns_name` inventory.
  4. Services (Docker) - container state/health/uptime/image for the stacks.
  5. Recent errors - a bounded per-container log tail, parsing `dns-sync`'s slog
     JSON for `level>=error`.
- **Security posture.** Read-only everywhere (no upstream write path). It uses a
  **dedicated minimum-read-scope NetBox token** (never the dns-sync/bootstrap
  admin token), a scoped Technitium token, the step-ca DB read-only via a
  `SELECT`-only role, and the Docker socket mounted `:ro`. The scoped read-only
  tokens are auto-provisioned by the netbox and technitium deploys; operator-placed
  (SOPS/age) tokens win. Tokens come from files/env, never hardcoded or logged.
  **The control plane UI has no auth (v1)** - acceptable only on a trusted internal
  lab network. **TODO: front it with auth (the repo's IdP or a reverse proxy)
  before any non-lab use.**
- **Phase 2 (out of v1 scope).** History/collector (time series) and UI
  authentication.

## Service Reference

Services are selected on the `/deploy` page; the engine adds dependencies automatically and deploys in dependency order. "Depends on" lists other labprovider services only; every service also needs a valid `labprovider.env`.

| Service | Purpose | Depends on | Data / runtime dirs | Ports | Secrets it creates | Remove behavior |
|---------|---------|------------|---------------------|-------|--------------------|-----------------|
| chrony | Containerized NTP server | none | `CHRONY_DIR`; runtime under `WORKDIR/chrony` | 123/udp | none | removes runtime dir; preserves `CHRONY_DIR` |
| rsyslog | Containerized central syslog collector | none | `SYSLOG_LOG_DIR`; runtime under `WORKDIR/rsyslog` | `SYSLOG_PORT`/udp+tcp | none | removes runtime dir; preserves `SYSLOG_LOG_DIR` |
| ca | step-ca private CA (dedicated postgres) | none | `CA_DATA_DIR`, `CA_POSTGRES_DATA_DIR`; runtime under `WORKDIR/step-ca` | `CA_PORT`/tcp, `CA_POSTGRES_PORT`/tcp (loopback) | CA password file, `.pgpass`, RO role password | removes runtime dir; preserves data dirs, keys, password |
| technitium | Containerized DNS server | ca | `TECHNITIUM_DATA_DIR`, `TECHNITIUM_CERT_DIR`; runtime under `WORKDIR/technitium` | 53/tcp+udp, `TECHNITIUM_HTTP_PORT`/tcp, `TECHNITIUM_HTTPS_PORT`/tcp | pfx password, dns-sync + dashboard API tokens | removes runtime dir, restores `systemd-resolved`; preserves data, certs, tokens |
| depot | VCF offline depot (nginx) | ca | `DEPOT_DATA_DIR`, `DEPOT_CERT_DIR`, `DEPOT_AUTH_DIR`; runtime under `WORKDIR/depot` | `DEPOT_HTTP_PORT`/tcp, `DEPOT_HTTPS_PORT`/tcp | `htpasswd` | removes runtime dir and `htpasswd`; preserves data and certs |
| keycloak | Keycloak identity provider | ca | `KEYCLOAK_DIR`; runtime under `WORKDIR/keycloak` | `KEYCLOAK_PORT`/tcp | none (credentials from env) | removes runtime dir; preserves `KEYCLOAK_DIR` |
| authentik | Authentik identity provider (OIDC + SCIM) | ca | `AUTHENTIK_DIR`; runtime under `WORKDIR/authentik` | `AUTHENTIK_PORT`/tcp | none (credentials from env) | removes runtime dir; preserves `AUTHENTIK_DIR` (certs, data, postgres) |
| zitadel | Zitadel identity provider (OIDC, multi-tenant) | ca | `ZITADEL_DIR`; runtime under `WORKDIR/zitadel` | `ZITADEL_PORT`/tcp | machine-user PATs under `ZITADEL_DIR/machinekey` | removes runtime dir; preserves `ZITADEL_DIR` (certs, postgres, machinekey) |
| netbox | NetBox IPAM/DCIM source of truth | ca | `NETBOX_DIR`, `NETBOX_MEDIA_DIR`, `NETBOX_POSTGRES_DATA_DIR`, `NETBOX_REDIS_DATA_DIR` | `NETBOX_PORT`/tcp | API token pepper, dns-sync + dashboard tokens | removes runtime files; preserves media, postgres, redis, certs, and secrets |
| s3 | SeaweedFS S3-compatible storage | none | `S3_DATA_DIR`; runtime under `WORKDIR/s3` | `S3_PORT`/tcp | none (credentials from env) | removes runtime dir; preserves `S3_DATA_DIR` |
| sftp | SFTPGo file transfer | ca | `SFTP_DATA_DIR`, `SFTP_HOME_DIR`, `SFTP_CERT_DIR`; runtime under `WORKDIR/sftpgo` | `SFTP_PORT`/tcp, `SFTP_ADMIN_PORT`/tcp | none (credentials from env) | removes runtime dir; preserves data, home, and certs |
| dns-sync | NetBox-to-Technitium reconcile loop | ca, technitium, netbox | `DNS_SYNC_DIR`, `DNS_SYNC_SECRETS_DIR`; runtime under `WORKDIR/dns-sync` | none (host networking, outbound only) | none (consumes netbox/technitium tokens) | removes runtime dir; preserves `DNS_SYNC_SECRETS_DIR` |

Notes:

- The control plane itself is installed by `install.sh`, not deployed from `/deploy`; it issues its own leaf certificate after the CA is deployed.
- The firewall is not managed; open the ports in [Required open ports](#required-open-ports) for the services you deploy.

## Secrets Inventory

Every secret labprovider generates or persists, where it lives, and what losing or regenerating it means:

| Secret | Path | Owner / mode | Created by | Consequence of loss or regeneration |
|--------|------|--------------|------------|--------------------------------------|
| CA password | `CA_PASSWORD_FILE` (default `CA_DATA_DIR/secrets/password.txt`) | `1000:1000`, `0600` | ca deploy (from `CA_PASSWORD` or generated) | Without it the CA key cannot be decrypted: step-ca stops starting and no certificates can be issued or renewed. It cannot be regenerated; losing it means reinitializing the CA (delete `CA_DATA_DIR` contents) and redeploying every certificate-consuming service, then redistributing the new root certificate. |
| NetBox API token pepper | `NETBOX_DIR/secrets/api_token_pepper` | root, `0600` | netbox deploy (from optional `NETBOX_API_TOKEN_PEPPER` or generated) | Changing or deleting it invalidates every existing NetBox API token, including the dns-sync token. Recover by redeploying netbox (provisions a fresh dns-sync token) and re-issuing any operator tokens. |
| dns-sync NetBox token | `DNS_SYNC_SECRETS_DIR/netbox.token` (composite `nbt_<key>.<token>`) | `1000:1000`, `0600` | netbox deploy (or operator-placed via SOPS/age) | dns-sync stops reconciling (NetBox reads fail). Redeploy netbox to provision a replacement; old tokens with the description "labprovider dns-sync" are retired automatically. |
| dns-sync Technitium token | `DNS_SYNC_SECRETS_DIR/technitium.token` | `1000:1000`, `0600` | technitium deploy (or operator-placed via SOPS/age) | dns-sync stops writing to Technitium. Redeploy technitium to provision a replacement (idempotent; a still-valid stored token is reused). |
| Technitium pfx password | `TECHNITIUM_CERT_DIR/technitium-pfx-password` | `1000:1000`, `0600` | technitium deploy | Needed to rebuild and open `technitium.pfx`. If lost, delete it together with `technitium.pfx` and redeploy technitium; a new password and bundle are generated and re-applied via the settings API. |
| Depot htpasswd | `DEPOT_AUTH_DIR/htpasswd` | root, `0644` | depot deploy (from `DEPOT_BASIC_AUTH_USER`/`_PASSWORD`) | Depot basic auth fails until recreated; regenerated from env on every depot deploy. |
| Zitadel machine PATs | `ZITADEL_DIR/machinekey/{pat.txt,login-client.pat}` | `1000:1000`, `0600` | zitadel first-instance init | Written only during first-instance init on an empty database. If lost while the DB persists, init will not rewrite them; recover by removing the `postgres` and `machinekey` dirs under `ZITADEL_DIR` and redeploying. |

Secrets that live only in `labprovider.env` (admin passwords, `NETBOX_SECRET_KEY`, `AUTHENTIK_SECRET_KEY`, `ZITADEL_MASTERKEY`, `VMSCA_PASSWORD`, S3 keys, and so on) are the operator's responsibility; the managed file is stored in plaintext on the host under `/opt/labprovider/control-plane/`.

## Troubleshooting

Real failure modes with the messages they produce:

### Port 53 is already in use

```text
Error: Port 53 is already in use and labprovider will not stop the holder automatically.
```

The technitium deploy preflights port 53. `install.sh` disables the `systemd-resolved` stub listener up front; any other holder (a leftover unbound, dnsmasq) must be stopped manually before redeploying. Check `ss -lntup 'sport = :53'`.

### step-ca did not initialize

```text
Error: step-ca did not initialize. Check: docker compose -f <workdir>/step-ca/docker-compose.yml logs step-ca
```

The ca deploy waits for `CA_DATA_DIR/config/ca.json` and then the health endpoint. A partially initialized `CA_DATA_DIR` (for example certs present but no `config/ca.json`, or a password file the container user cannot read) keeps first-start initialization from running. Check the container logs; if the data dir is inconsistent, move aside or delete the contents of `CA_DATA_DIR` and redeploy ca (this reinitializes the CA and invalidates previously issued certificates).

### 403 "Invalid v1 token" / "Invalid v2 token" from NetBox

NetBox 4.6 rejects the legacy `Token <key>` header (`Invalid v1 token`) and rejects Bearer composites whose hash no longer matches (`Invalid v2 token`, typically after the API token pepper changed). Use `Authorization: Bearer nbt_<key>.<token>`, and if the pepper was regenerated, redeploy netbox so a fresh dns-sync token is provisioned.

### HTTP 400 when browsing NetBox by IP

Django only serves hosts listed in `ALLOWED_HOSTS`. `NETBOX_ALLOWED_HOSTS` defaults to the NetBox FQDN only, so `https://<host-ip>:<NETBOX_PORT>/` returns a plain `Bad Request (400)`. Browse by FQDN, or add the IP to `NETBOX_ALLOWED_HOSTS` and redeploy netbox.

### Config appears outdated

```text
Error: labprovider.env appears outdated.
Missing variables from the shipped example:
```

After pulling a newer checkout and rebuilding the control-plane image, new variables in the example must be added to your managed config in `/config`; saving is blocked while variables the example defines are missing. A mixed-version symptom of the same root cause is a deploy failing with `Missing required variable: <NAME>`.

### dns-sync reconcile failures

`docker compose -f ${WORKDIR}/dns-sync/docker-compose.yml logs -f` shows structured JSON logs. `status 403` from NetBox means the stored token is no longer valid (see the pepper note above); `invalid-token` from Technitium means `technitium.token` was revoked. Redeploy netbox or technitium to provision replacements.

## VCF Lab Companion

labprovider provides a lightweight external infrastructure services platform for VMware Cloud Foundation lab and PoC environments.

VCF depends on external services that are not provided by the platform itself.

### Pre-deployment requirements

- DNS for forward and reverse resolution
- NTP for time synchronization

### Post-deployment operational dependencies

- identity provider for OIDC or federation
- centralized logging
- certificate authority (with optional automated cert replacement via the MSCA emulator)
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
install.sh          The only shell: Docker install, host prep, build + run the control plane

config/
  dns.seed.example
  labprovider.env.example   The schema source of truth and completeness reference

services/
  control-plane/    Go control plane: config wizard, deploy engine (SSE progress),
                    read-only dashboard, CSR signing, MSCA emulator. Deployers live
                    under internal/deploy/ (one file per service); templates are
                    embedded Go text/template under internal/deploy/templates/.
  dns-sync/         Go source for the dns-sync and dns-seed binaries (image built
                    locally, baked into the control-plane image)

docs/
  images/           Architecture diagram sources and exports
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

For control-plane development:

```bash
cd services/control-plane
go build ./... && go vet ./... && go test ./...
```

## Failure Handling

Deployment fails fast if:

- required variables are missing or malformed
- a step-ca certificate cannot be issued
- a service does not become reachable at its user-facing endpoint
- an image build or pull fails

This keeps deployments predictable and reproducible. Readiness checks probe the externally exposed endpoint (a started container does not imply readiness).

## Operational Notes

- Use FQDNs instead of raw IPs where possible
- Ensure both forward and reverse DNS are configured
- Import `keycloak-ca-chain.pem` into VCF when configuring OIDC
- Use `keycloak-ca-roots.pem` only when a roots-only trust bundle is required
- Built-in labprovider service DNS records are generated automatically; reserve `dns.seed` for external and custom records

### DNS behavior warning

Deploying DNS takes over host name resolution: `install.sh` disables the `systemd-resolved` stub listener up front, and the technitium deploy points `/etc/resolv.conf` at Technitium after verifying resolution works; removing Technitium restores the stock configuration.

## Scope

labprovider focuses on a simple, modular, and reproducible way to deploy shared infrastructure services on a single host for lab and PoC environments.

It is intentionally:

- fully containerized
- control-plane driven
- template-driven
- explicit
- single-node
- easy to reason about

It does not aim to introduce orchestration layers, HA patterns, or broad production abstractions.
