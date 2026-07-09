# step-ca PostgreSQL Migration + CA Admin Console - Phase 1 Plan

Investigation and plan ONLY. No code changes, no CA rebuild in this phase.
Approve this document before any Phase 2 work begins.

Pinned version under investigation: `CA_IMAGE=docker.io/smallstep/step-ca:0.30.2`
(config/provider-box.env.example:46). All findings below are stated for this
version and its bundled smallstep/nosql (v0.8.0, matching STEPCA_STORAGE.md);
items that cannot be proven in a sandbox are called out in "Needs live
confirmation".

---

## 0. Current state (what we are changing)

- step-ca runs as a single container from `templates/docker-compose.step-ca.yml.tpl`,
  data bind-mounted at `CA_DATA_DIR` (`/opt/provider-box/step-ca`).
- Backend is embedded **BadgerDB v3** (manifest v7). No `db` stanza is written
  today; step-ca defaults to badger under `${CA_DATA_DIR}/db`.
- Root + intermediate keypair live on the FILESYSTEM
  (`certs/root_ca.crt`, `certs/intermediate_ca.crt`, `secrets/*_ca_key`),
  NOT in the database.
- Provisioners are defined in `config/ca.json`, NOT in the database (remote
  admin is off).
- The dashboard's Certificates panel reads issued/revoked certs by
  snapshot-copying the live badger dir and parsing it
  (`services/dashboard/internal/certs/certs.go`, pinned to badger/v3). This is
  the version-fragile reader documented in STEPCA_STORAGE.md.
- Password handling convention in this repo is FILE-based for step-ca
  (`CA_PASSWORD_FILE`, mode 0600, under `CA_DATA_DIR`). NetBox/Authentik
  postgres, by contrast, pass `POSTGRES_PASSWORD` inline via compose env.

Module-independence rule (AGENTS.md / PROJECT_CONTEXT.md) is fixed input:
step-ca gets its OWN dedicated postgres. No sharing with netbox/authentik. Not
up for discussion in this plan.

---

## 1. PostgreSQL backend for step-ca 0.30.2

### 1a. Exact ca.json db stanza

step-ca supports badger, bolt, mysql, postgresql. For postgresql the stanza is:

```json
"db": {
  "type": "postgresql",
  "dataSource": "postgresql://stepca@stepca-postgres:5432/stepca?sslmode=disable",
  "database": "stepca"
}
```

Notes:
- `dataSource` is a libpq/pgx DSN. `database` names the target DB (pgx uses it
  to override the DSN path).
- The nosql pgx backend AUTO-CREATES the database if missing (on ping error
  `3D000` it connects to `template1` and runs `CREATE DATABASE`) and
  AUTO-CREATES each bucket table with `CREATE TABLE IF NOT EXISTS`. So step-ca
  needs a role that can create the DB and tables on first start; the postgres
  container's `POSTGRES_DB=stepca` pre-creating the DB is compatible.
- `sslmode=disable` is acceptable only because step-ca <-> its dedicated
  postgres is a container-local link on one host. Revisit if that ever spans
  hosts (see Needs live confirmation).

### 1b. How the password is supplied (file-based, matching repo convention)

step-ca's `dataSource` normally carries the password INLINE in the DSN, which
would write the postgres password into `ca.json`. To keep the repo's
file-based secret convention (as with `CA_PASSWORD_FILE`), omit the password
from the DSN and let pgx read it from `PGPASSFILE`:

- The pinned nosql backend uses **pgx v5** (`github.com/jackc/pgx/v5`).
- pgx v5 `ParseConfig` honors libpq env vars including **`PGPASSFILE`** and
  falls back to a `.pgpass` file when the DSN carries no password.

Recommended wiring:
- DSN in `ca.json`: no password
  (`postgresql://stepca@stepca-postgres:5432/stepca?sslmode=disable`).
- Mount a `.pgpass`-format secret file (mode 0600, owner uid 1000) into the
  step-ca container, e.g. `/home/step/secrets/pgpass` with a single line
  `stepca-postgres:5432:stepca:stepca:<password>`.
- Set `PGPASSFILE=/home/step/secrets/pgpass` in the step-ca container env.

This mirrors the existing `CA_PASSWORD_FILE` pattern (managed 0600 file under
`CA_DATA_DIR`) instead of leaking the DB password into `ca.json`.
Needs live confirmation that 0.30.2's bundled pgx honors `PGPASSFILE` at
runtime (pgx docs confirm the API; the pinned build must be exercised).

### 1c. Opaque key-value caveat - CONFIRMED

step-ca uses postgres as a **key-value store, not a relational database**.
Upstream docs state it plainly: "step-ca uses PostgreSQL as a simple key-value
store, not as a relational database" and "An entry in the database is a
[]byte value that is indexed by []byte table and []byte key."

What the pgx nosql backend actually creates (verified from
smallstep/nosql postgresql source):

```sql
CREATE TABLE IF NOT EXISTS <bucket> (
  nkey   BYTEA CHECK (octet_length(nkey) <= 255),
  nvalue BYTEA,
  PRIMARY KEY (nkey)
);
-- Set: INSERT ... ON CONFLICT (nkey) DO UPDATE SET nvalue = excluded.nvalue
-- Get: SELECT nvalue FROM <bucket> WHERE nkey = $1
```

So, answering the task question directly:

- The documented tables **DO exist and ARE queryable** - one real SQL table
  per bucket: `x509_certs`, `x509_certs_data`, `revoked_x509_certs`,
  `acme_*`, `ssh_*`, etc. `SELECT * FROM x509_certs` returns rows.
- BUT every table is exactly two columns: `nkey BYTEA`, `nvalue BYTEA`. The
  value is an **opaque blob** - raw DER for a cert, JSON for metadata. There
  are no `common_name`, `not_after`, `provisioner` columns.
- Consequence: you get NO relational query power. You cannot
  `WHERE common_name = ...` or join. A reader can `SELECT nkey, nvalue` and
  count rows, but must decode each `nvalue` blob (parse DER / unmarshal JSON)
  in application code - exactly what the current badger reader already does.
- Unlike badger, the pgx table IS the bucket, so `nkey` is the PLAIN key
  (the decimal serial string), not badger's `[len][bucket][len][key]` binary
  concat. The bucket-prefix / `toBadgerKey` encoding in certs.go is NOT needed
  against postgres.

Bottom line for a console: reading step-ca's postgres "with SQL" only means
`SELECT nvalue` then decode blobs. It does not turn cert state into
queryable columns. Any tool that claims to "read the tables" is still parsing
DER/JSON, or it is talking to step-ca's API instead.

Needs live confirmation: stand up 0.30.2 + postgres, issue a leaf, and confirm
`x509_certs`/`x509_certs_data`/`revoked_x509_certs` exist with `bytea` values
that parse as DER/JSON. (Attempted in-sandbox; see section 6.)

### 1d. Backend switch requires a fresh CA database (not necessarily a new root)

There is **no migration path** between backends. Badger data is opaque KV with
no export/import tool; upstream docs offer none. So:

- The issued-cert inventory, revocation list, and ACME accounts/orders in
  badger DO NOT move to postgres. Postgres starts empty.
- IMPORTANT nuance: the **root + intermediate keypair are on the filesystem**,
  not in the DB. Editing only the `db` stanza does NOT invalidate the root.
  Two distinct options:

  Option A - preserve keys, empty DB (keys survive, history does not):
  keep `certs/` + `secrets/`, add the `db` stanza, restart. Existing leaf
  certs stay VALID and trusted (same intermediate). What is LOST: the record
  of previously issued certs, the revocation list, and ACME state. CRL/OCSP
  would no longer list anything revoked before the switch. Dashboard inventory
  shows only certs issued after the switch until services are re-issued.

  Option B - clean rebuild (new root, reissue everything):
  wipe `CA_DATA_DIR`, re-init with postgres, reissue all service leaves and
  redistribute the new root. Same operation we just performed.

Recommendation: **Option B (clean rebuild)** for this lab. Rationale: the
whole point of the change is a clean, reproducible CA on a DB backend; Option A
leaves stale badger data on disk and a revocation list that silently resets,
which is worse to reason about than a deliberate reissue. The environment is
rebuild-friendly and we have done this reissue chain before. Document Option A
only as the "keep the root if a full reissue is truly impossible" fallback.

Reissue chain (services that pull a leaf from step-ca via `step ca certificate`,
from grep of bootstrap/*.sh): **netbox, keycloak, authentik, technitium, depot,
sftp, dashboard**. Each re-runs its own bootstrap to re-issue after the CA is
rebuilt; the new `root_ca.crt` must also be re-fetched by every service that
bundles it.

---

## 2. Enable-the-knobs review: Remote/Admin Provisioner API + CRL

### 2a. Remote Provisioner Management (Admin API)

- Moves provisioner + admin config OUT of `ca.json` and INTO the database.
  Enabled on an existing CA with `"enableAdmin": true` in the authority block
  (on `step ca init`, the flag is `--remote-management`).
- On enable, step-ca migrates `ca.json` provisioners into the DB and creates an
  "Admin JWK" provisioner plus a Super Admin (username `step`).
- Auth model: two tiers - Admins (manage provisioners) and Super Admins
  (also manage other admins). Callers authenticate with the admin provisioner's
  scheme (JWK password, or `--admin-cert`/`--admin-key` for unattended use).
- Security implications (material - this is the trust root):
  - Admin privilege is NOT scoped per provisioner. Any admin can add/modify/
    remove ANY provisioner, i.e. mint a provisioner that issues arbitrary
    certs. This is effectively CA-root-equivalent power exposed over the
    network API.
  - Passive cert revocation does not revoke admin access; an admin must be
    explicitly removed.
  - Turning this on is the prerequisite that a write-capable console
    (stepca-web) needs. It is ALSO the single biggest new attack surface in
    this whole change. Do not enable it unless a console actually needs write
    access. A read-only console does not.

Decision input: if v1 console is read-only (recommended, section 3), we do NOT
need to enable remote provisioner management at all. Keep provisioners in
`ca.json` and leave `enableAdmin` off. This removes the largest security
concern from Phase 2.

### 2b. CRL

```json
"crl": {
  "enabled": true,
  "generateOnRevoke": true,
  "cacheDuration": "24h"
}
```

- With `enabled: true`, step-ca generates a CRL and serves it; clients can
  fetch the revocation list. `generateOnRevoke` regenerates immediately on a
  revoke instead of waiting for the cache window.
- CRL requires the CA to persist revocation state in its DB. It works with the
  DB backend we are moving to. Enabling it is a config-only change.
- Low risk to enable; it only exposes a signed revocation list. Recommended to
  turn on as part of Phase 2 since we are already reinitializing the CA and a
  DB backend makes revocation state first-class.
- Needs live confirmation of the exact 0.30.2 CRL endpoint path and that our
  clients that should honor it are configured to fetch it (most lab clients
  will not check CRL by default - enabling it is cheap insurance, not an
  enforced control here).

---

## 3. stepca-web evaluation (security-sensitive dependency)

Repo: https://github.com/damhau/stepca-web

### 3a. Project health (reported honestly)

- Single maintainer (damhau). Created 2025-04-16. Last push 2026-04-29.
  10 stars, 5 forks, 0 open issues, MIT-ish/unspecified, not archived.
- Commit activity is bursty: clusters in Apr/May/Jul/Sep 2025 and a small
  Apr 2026 touch ("update readme", "cleanup"). No release cadence, no
  tags/releases discipline, tiny adoption.
- Assessment: this is an early, low-adoption, single-maintainer hobby project.
  That profile is acceptable for a throwaway internal tool. It is a LIABILITY
  for something sitting in the CA trust path with DB and admin-API access.

### 3b. What access it needs (blast radius)

Per its README, stepca-web wants ALL of:
1. Direct **PostgreSQL** access to step-ca's DB (reads `x509_certs`,
   `acme_*`, etc.).
2. step-ca **Remote Provisioner / Admin API** enabled (to manage
   provisioners + admins).
3. The admin **JWK provisioner key** (`jwk_key.json`) for JWT auth - i.e. a
   credential that can issue certs.
4. **systemd control** of the step-ca service (start/stop/restart).
5. Read/write of **`ca.json`** through the GUI (edit + validate).

Blast radius if compromised: total CA compromise. Items 2-5 give it the
ability to mint provisioners, issue arbitrary certs, rewrite CA config, and
stop the CA. This is the opposite of least privilege. It also assumes a
systemd/host deployment model, which does not match our containerized step-ca
(no systemd inside the container) - items 4 and the ca.json-edit flow do not
map cleanly onto our compose deployment.

### 3c. Auth backends / Authentik

- Selectable via `AUTH_BACKEND`: `ldap` (default), `radius`, `saml`, `oidc`,
  `local`.
- OIDC exists, so authenticating against our Authentik is plausible in
  principle, but the README does not document the OIDC config surface (issuer,
  client id/secret, claim mapping). Would need source-level verification before
  trusting it as the sole gate on a CA admin UI.
- `local` mode stores users IN-MEMORY in code
  (`app/libs/auth/local_backend.py`): username + scrypt-hashed password +
  role, no external persistence. Fine for a demo, not something to depend on
  for CA administration.

### 3d. Does it read step-ca's postgres directly, and does that even work?

- Yes, it queries the postgres tables directly (`db_x509.py`, `db_acme.py`,
  `db_step.py`) in ADDITION to using the admin API.
- Given section 1c (tables are `nkey`/`nvalue` bytea blobs), "reads the tables"
  necessarily means it SELECTs `nvalue` and decodes DER/JSON in Python - the
  same class of version-fragile decode our dashboard reader already does, just
  in a second codebase we would not control. If step-ca changes the stored
  value shape, stepca-web breaks the same way our reader would, and we would be
  waiting on an external single maintainer to fix a component in our CA path.
- This must be validated against 0.30.2 specifically before any trust: a UI
  written against a different step-ca/schema assumption could silently
  mis-decode.

### 3e. Verdict

**Do not adopt stepca-web.** Reasons, in order:
1. Blast radius: it demands DB creds + admin API + a cert-issuing JWK key +
   ca.json write + service control. A compromise is a full CA compromise.
2. Maintenance risk: single-maintainer, low-adoption, no release discipline,
   in the trust root path.
3. Deployment mismatch: assumes systemd host control; our step-ca is a
   container. Bolting on host-level control widens the surface further.
4. It duplicates the fragile direct-DB decode we already own in the dashboard,
   in a codebase we do not control.

**Recommendation: build a thin, READ-ONLY, in-repo console.** In v1 it has NO
write and NO revoke path, so it needs NONE of stepca-web's dangerous access:
no admin API, no JWK key, no ca.json write, no systemd. This also means we do
NOT enable remote provisioner management (section 2a) in Phase 2. Prefer to
extend the EXISTING `services/dashboard` (already a read-only Go service that
surfaces cert state) rather than stand up a separate service - it keeps one
read-only seam instead of two. A dedicated `services/stepca-console` is the
alternative if the dashboard should stay purely a status board; recommend
folding into the dashboard to avoid a second service in the CA path.

Write operations (issue/revoke from a UI) are explicitly OUT of v1. If they are
ever wanted, that is a separate, scoped decision that would require enabling
the admin API and is re-evaluated then - not smuggled in now.

---

## 4. Dashboard cert panel after the change

Today: snapshot-copy badger dir, iterate buckets with the binary key encoding,
parse DER/JSON (certs.go, badger/v3 pin).

After moving step-ca to postgres, three options:

- **A. Read the dedicated postgres directly, read-only (recommended).**
  Point the panel at `stepca-postgres` with a SEPARATE read-only role
  (`GRANT SELECT` on the step-ca tables only; NOT the role step-ca uses).
  Query `SELECT nkey, nvalue FROM x509_certs` (nkey = decimal serial,
  nvalue = DER), `revoked_x509_certs`, and `x509_certs_data`; reuse the exact
  DER/JSON parsing already in certs.go. This DELETES the snapshot-copy dance,
  the badger/v3 dependency and pin, and the binary bucket-key encoding -
  a net simplification. It stays coupled to step-ca's stored value SHAPE (DER
  is stable; the JSON metadata shape is the fragile part, same as today).
- **B. Read via step-ca's API.** Cleanest decoupling from storage internals,
  but step-ca has no "list all certs" endpoint (the gap the retired stepca-api
  design existed to fill). Not viable for the list view without an extra
  inventory service. Rejected for v1.
- **C. Read via stepca-web.** Rejected (section 3).

Recommendation: **Option A.** Retire the badger reader, add a read-only
postgres reader in the same `internal/certs` package behind the same `Reader`
shape, gated by a read-only DB role. Blast radius of the dashboard stays
"can read cert metadata", which is what it is today.

Boundary note: the dashboard reading the CA's dedicated postgres is a
cross-module read, but the dashboard ALREADY crosses boundaries (it reads
NetBox and Technitium) and ALREADY reads step-ca's badger files directly. A
read-only role is the mitigation and keeps this within the established
dashboard pattern.

---

## 5. Proposed Phase 2 change-set

### 5a. New / changed files

- `config/provider-box.env.example` - add a `CA_POSTGRES_*` block mirroring the
  `NETBOX_POSTGRES_*` naming:
  `CA_POSTGRES_IMAGE`, `CA_POSTGRES_DB=stepca`, `CA_POSTGRES_USER=stepca`,
  `CA_POSTGRES_PASSWORD`, `CA_POSTGRES_DATA_DIR=/opt/provider-box/step-ca/postgres`,
  plus a dashboard read-only role `CA_POSTGRES_RO_USER`/`CA_POSTGRES_RO_PASSWORD`.
- `templates/docker-compose.step-ca.yml.tpl` - add a `stepca-postgres` service
  (dedicated, own bind-mount volume, healthcheck like the netbox postgres),
  and add `PGPASSFILE` env + the pgpass secret mount to the `step-ca` service.
  step-ca `depends_on` postgres healthy.
- New template `templates/ca.json.tpl` OR a post-init edit step in
  `bootstrap/ca.sh` that writes the `db` (postgresql), `crl`, and (only if ever
  needed) authority stanzas. Given step-ca still self-inits, the simplest
  reproducible path is: let init run, then overwrite the `db` stanza and add
  `crl` before the final restart. Keep it explicit and templated per repo
  convention.
- `bootstrap/ca.sh` - materialize the pgpass secret file (0600, uid 1000) the
  same way `CA_PASSWORD_FILE` is materialized; create the read-only DB role;
  add postgres readiness wait before `wait_for_ca_init`.
- `services/dashboard` - replace the badger reader in `internal/certs` with a
  read-only postgres reader (drop badger/v3 dep + snapshot code); add
  `DASHBOARD_CA_POSTGRES_*` config for the read-only DSN.
- Docs: update `STEPCA_STORAGE.md` (badger reader retired) and add a short
  "step-ca on postgres" section to README.

No new orchestration, no shared postgres, no reverse proxy. One new container
in the CA module, consistent with the netbox pattern.

### 5b. Migration runbook (Option B, clean rebuild)

Preconditions (hard gates):
1. Blank-mount guard is in place (`require_ca_root_not_corrupted`, ca.sh) -
   DONE.
2. **Backup MUST precede the rebuild.** Take a full copy of `CA_DATA_DIR`
   (root/intermediate keys, `ca.json`, badger `db/`, secrets) to an
   off-box/retained location BEFORE touching anything. This is the rollback
   anchor. Do not proceed without a verified backup.

Sequence:
1. Announce a CA outage window (all issuance/renewal pauses).
2. Back up `CA_DATA_DIR` in full; verify the backup is readable.
3. `bootstrap/provider-box.sh --ca --remove` (stops CA, preserves data by
   current semantics) - then, for a clean rebuild, move the old data dir aside
   (do NOT delete until success is confirmed).
4. Deploy `stepca-postgres` (empty DB) via the updated CA compose.
5. Re-init step-ca against a fresh `CA_DATA_DIR`; write the postgresql `db`
   stanza + `crl.enabled`; supply the DB password via `PGPASSFILE`.
6. Bring step-ca up; confirm health endpoint and that the bucket tables are
   created in postgres.
7. Reissue leaves + redistribute the new root by re-running each dependent
   bootstrap: netbox, keycloak, authentik, technitium, depot, sftp, dashboard.
8. Redeploy the dashboard with the postgres read-only reader; confirm the
   Certificates panel lists the freshly issued leaves.
9. Only after all services validate: retire the moved-aside old data dir.

### 5c. Rollback

- Any failure before step 9: stop the new CA, restore `CA_DATA_DIR` from the
  step-2 backup (badger + original `ca.json`, no `db` stanza), bring step-ca
  back on badger, and the previously issued leaves + root are unchanged so no
  service needs reissuing. The dashboard reverts to the badger reader
  (previous image).
- The old root never has to change under rollback because Option B keeps the
  old key material in the backup until success is confirmed. This is the reason
  step 3 MOVES rather than deletes.

---

## 6. What needs live, on-host confirmation (sandbox cannot cover)

Attempted in-sandbox verification and its limits are noted per item.

1. **Real 0.30.2 + postgres schema.** Confirm `x509_certs`,
   `x509_certs_data`, `revoked_x509_certs` tables appear with `nkey`/`nvalue`
   bytea and that `nvalue` parses as DER/JSON after issuing a leaf. Source-level
   findings above are firm; a live issue-and-inspect is the final proof.
2. **PGPASSFILE at runtime.** Confirm the pinned image's bundled pgx actually
   reads `PGPASSFILE` (no password in the DSN) and connects. pgx docs confirm
   the behavior; the built binary must be exercised.
3. **The reissue chain on the real host.** netbox/keycloak/authentik/
   technitium/depot/sftp/dashboard re-issuing against the rebuilt CA, including
   host-resolver behavior (`--add-host`/`--resolve` to 127.0.0.1) and the new
   root propagating to every trust bundle. This is host- and timing-dependent
   and cannot be validated in a sandbox.
4. **PG TLS posture.** Whether `sslmode=disable` on the container-local link is
   acceptable long term, or a step-ca-issued cert should protect the
   step-ca <-> postgres hop.
5. **CRL endpoint + client honoring.** The exact 0.30.2 CRL path and whether
   any client is configured to check it.
6. **Read-only DB role scope.** That `GRANT SELECT` to the dashboard role,
   with step-ca auto-creating tables at runtime, does not race (role created
   before tables exist) - may need a grant on future tables / default
   privileges.

---

## 7. Summary of recommendations (for approval)

1. Move step-ca 0.30.2 to a DEDICATED postgres (`stepca-postgres`) in the CA
   module. Password via `PGPASSFILE`, not inline in `ca.json`.
2. Treat this as a clean CA rebuild + reissue (Option B). Backup first; the
   backup is the rollback anchor.
3. Enable `crl.enabled` during the rebuild. Do NOT enable remote provisioner
   management (`enableAdmin`) - a read-only console does not need it, and it is
   the largest new attack surface.
4. Do NOT adopt stepca-web (blast radius + maintenance risk + deployment
   mismatch). Build a thin READ-ONLY console by extending the existing
   dashboard.
5. Repoint the dashboard cert panel at the postgres tables read-only, deleting
   the badger reader, snapshot copy, and badger/v3 pin.

No code changes were made in Phase 1.
