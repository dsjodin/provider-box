# stepca-api — Design Blueprint

> **Historical.** This blueprint described a standalone certificate API service
> (`services/stepca-api`, now removed). Its "list issued certificates" intent
> now lives in `services/dashboard` as the read-only Certificates panel, which
> reuses the step-ca BadgerDB reader documented in `STEPCA_STORAGE.md`. The
> collector/API surface below (SQLite inventory, token-authed REST) is deferred
> to the dashboard's phase 2. Kept as background, not an active spec.

A thin management/API layer in front of step-ca for the provider-box fork. It exists to close the one gap step-ca leaves open — **listing issued certificates** — while presenting a clean, dashboard-friendly REST surface for the things step-ca already does (issue, revoke).

> Decision context: step-ca was chosen over Vault/OpenBao because seal/unseal is the wrong operational model for a frequently-rebuilt lab. The cost of that choice is this wrapper. Keep it small.

---

## 1. Scope

**In scope**
- `GET /certs` — list/search the certificate inventory (the missing piece).
- `GET /certs/{serial}` — detail for one cert.
- `POST /certs` — issue a new cert (proxies to step-ca).
- `DELETE /certs/{serial}` — revoke (proxies to step-ca `/revoke`).
- An inventory store the service owns, so listing never depends on step-ca internals.

**Out of scope (delegate to step-ca / existing UIs)**
- ACME issuance/renewal — clients talk to step-ca's ACME directory directly; the wrapper does not proxy ACME.
- Provisioner/admin management — leave to `step ca admin` unless a need appears.
- Secrets management — handled separately (SOPS/age), not here.

---

## 2. Architecture

```
 dashboard ──HTTP/JSON──> stepca-api ──┬── step CLI / HTTPS API ──> step-ca   (issue, revoke)
                                       └── SQLite (owned inventory)
        step-ca ──issuance webhook──> stepca-api  (populate inventory in real time)
```

The dashboard talks **only** to stepca-api, never to step-ca directly. stepca-api is the single seam.

stepca-api runs as its own container alongside step-ca, sharing access to the CA root and a provisioner credential so it can issue/revoke. It listens on its own port (TLS, cert issued by step-ca itself).

---

## 3. Inventory data flow

This is the only non-trivial part, because step-ca does not expose "list all certs."

**Primary feed — issuance webhook (verify per version).**
step-ca supports per-provisioner webhooks invoked during the issuance flow. Configure one pointing at `stepca-api`'s `/hooks/issued` endpoint. On each issuance, stepca-api records the cert in SQLite.

> ⚠️ Verification step before relying on this: step-ca's webhooks are designed as *enriching/authorizing* hooks called **during** the request, not strictly as post-issuance notifications. Confirm against your step-ca version that the webhook payload includes what inventory needs — **serial number, subject, SANs, notBefore, notAfter, provisioner**. If the serial isn't available at hook time (it may be assigned at signing), fall back to the reconcile job below as the authoritative source and treat the webhook as a low-latency hint only.

**Backstop / source of truth — startup + periodic reconcile.**
A reconcile job reads step-ca's database (BadgerDB by default) to enumerate issued + revoked certs and upserts them into SQLite. This:
- backfills history from before the webhook existed,
- catches anything the webhook missed,
- keeps revocation status accurate even for certs revoked outside the wrapper (e.g. via `step ca revoke` directly).

> ⚠️ Reading Badger couples to step-ca's internal storage layout, which can change across versions. Isolate this in one module behind an interface so a version bump only touches one file. If Badger proves fragile, the alternative is parsing step-ca's audit log / enabling its X.509 storage and reading that instead.

**Revocation state.**
Because revoke goes *through* stepca-api, the wrapper authoritatively knows everything it revoked. The reconcile job reconciles anything revoked out-of-band.

---

## 4. SQLite schema

Single file, e.g. `/data/stepca-api.db`. One table is enough to start.

```sql
CREATE TABLE certificates (
    serial        TEXT PRIMARY KEY,      -- hex serial, step-ca canonical form
    common_name   TEXT NOT NULL,
    sans          TEXT,                  -- JSON array of SAN strings
    provisioner   TEXT,
    not_before    TEXT NOT NULL,         -- ISO 8601 UTC
    not_after     TEXT NOT NULL,         -- ISO 8601 UTC
    status        TEXT NOT NULL          -- 'active' | 'revoked' | 'expired'
                  DEFAULT 'active',
    revoked_at    TEXT,                  -- ISO 8601 UTC, null unless revoked
    revoke_reason TEXT,
    source        TEXT,                  -- 'webhook' | 'reconcile'
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_certs_status     ON certificates(status);
CREATE INDEX idx_certs_not_after  ON certificates(not_after);
CREATE INDEX idx_certs_cn         ON certificates(common_name);
```

`status` is derived: `expired` is computed from `not_after < now` at read time (don't rely on a background job to flip it; compute on query so it's always correct).

---

## 5. REST surface

All responses JSON. All mutating routes require auth (section 6).

| Method | Path | Purpose | Notes |
|---|---|---|---|
| `GET` | `/certs` | List inventory | Query params: `status`, `cn`, `expiring_before`, `limit`, `offset` |
| `GET` | `/certs/{serial}` | One cert's detail | 404 if unknown |
| `POST` | `/certs` | Issue a cert | Body: `{ "common_name", "sans": [], "duration", "provisioner" }` → returns cert + key (key streamed once, not stored) |
| `DELETE` | `/certs/{serial}` | Revoke | Body optional: `{ "reason" }`; proxies to step-ca `/revoke`, then marks SQLite |
| `POST` | `/hooks/issued` | Internal webhook sink | Only reachable from step-ca; not part of the public API |
| `GET` | `/healthz` | Liveness | Unauthenticated |

**Issuance** (`POST /certs`) mirrors what the bootstrap already does: shell out to `step ca certificate` against the CA container using the `admin` JWK provisioner + password file, or call step-ca's HTTPS sign API. Return the cert; stream the private key in the response body exactly once and never persist it.

**Revocation** (`DELETE /certs/{serial}`) calls step-ca revoke by serial, then updates SQLite (`status='revoked'`, `revoked_at`, `revoke_reason`).

---

## 6. Auth

Keep it proportional to a lab, but not open:
- A single API token (long random string) required on all routes except `/healthz`, passed as `Authorization: Bearer <token>`.
- `/hooks/issued` secured separately — either a distinct shared secret step-ca sends, or network-restricted so only the step-ca container can reach it.
- TLS on the listener, cert issued by step-ca (dogfooding the CA).

Token lives in the same secrets mechanism the fork adopts (SOPS/age), **not** plaintext in the env file.

---

## 7. step-ca wiring

- **Provisioner credential**: stepca-api needs to issue/revoke, so it mounts the CA data dir (read of root, use of the `admin` provisioner password file) the same way the bootstrap's `step ca certificate` calls do — runs as uid 1000 to match.
- **Webhook config**: add a webhook to the relevant provisioner in `ca.json` pointing at `https://stepca-api:<port>/hooks/issued`, with the shared secret. Templated like every other config in the fork.
- **CRL/OCSP**: optional later — if you want revocation honored by clients, ensure step-ca's CRL/OCSP is enabled; the wrapper's revoke already hits step-ca, so CRL just needs turning on CA-side.

---

## 8. Deployment / bootstrap integration

- New stage: `provider-box.sh --stepca-api` (and folded into a service list, **not** silently into `--all` — respecting the opt-out lesson from the `--all`/Unbound trap).
- Rendered compose `docker-compose.stepca-api.yml` + a config template, same pattern as the other services.
- Runs as uid 1000; data dir (`SQLite`) chowned to 1000 at deploy time — applying the same ownership fix learned from the CA bug, proactively.
- Depends on step-ca being up and initialized.

---

## 9. Open decisions

- **Language**: Go (matches step-ca + ecosystem, single static binary, easy to read Badger with the same library step-ca uses) vs Python/FastAPI (fastest to a working REST+SQLite service, larger runtime). For a small owned service that may need to read step-ca's Badger DB, **Go has a real edge** — same storage library, single binary, no interpreter in the image. Python wins only on raw speed-to-first-version.
- **Webhook vs reconcile-only**: if the webhook payload turns out not to carry the serial reliably, consider skipping the webhook entirely and running reconcile on a short interval (e.g. 30s) as the sole feed — simpler, one code path, slightly higher latency on the inventory view. Decide after the verification step in section 3.

---

## 10. Suggested build order

1. SQLite layer + schema + the `status` derivation.
2. Reconcile job reading step-ca's DB (the authoritative feed) — get inventory populating *without* the webhook first; this de-risks the uncertain part early.
3. `GET /certs` + `GET /certs/{serial}` on top of the store — now you have the missing feature.
4. `DELETE /certs/{serial}` revoke proxy.
5. `POST /certs` issue proxy.
6. Webhook sink + step-ca webhook config (only if reconcile latency isn't good enough).
7. Auth + TLS.
8. Bootstrap stage + compose template.
9. Dashboard against the API.
