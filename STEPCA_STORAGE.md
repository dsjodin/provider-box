# step-ca Storage Layout (BadgerDB)

> **Current location.** This reader now lives in
> `services/dashboard/internal/certs/certs.go` (the dashboard's Certificates
> panel), migrated from the removed `services/stepca-api`. The bucket layout,
> key encoding, and snapshot-on-read approach documented below are unchanged and
> still load-bearing. References below to `internal/reconcile/badger.go`,
> `cmd/stepca-api`, and the SQLite inventory (including the hex-vs-decimal serial
> note) describe the old service and are historical; the dashboard reader keeps
> serials in step-ca's native form and has no SQLite store.

Reference for how the dashboard's certificate reader reads issued/revoked certificates directly from step-ca's embedded database. This is the **version-fragile** part — it depends on step-ca's internal storage format, which is not a stable public API.

> All of this is isolated in one file (`services/dashboard/internal/certs/certs.go`). When step-ca bumps and something here changes, that is the only file to fix.

---

## Verified against

- **step-ca**: v0.30.2
- **smallstep/nosql**: v0.8.0 (the Badger v2 codepath)

Re-verify against source on any step-ca minor bump. The facts below were read from `smallstep/nosql` and step-ca source, **not** confirmed against a live CA dir from the build environment — run the live check in the last section when you have access to `/opt/provider-box/step-ca/db`.

---

## Key encoding (the #1 gotcha)

smallstep/nosql's Badger backend does **not** use a human-readable `"bucket/key"` string key. `badger/v2/badger.go::toBadgerKey` encodes keys as **binary**:

```
[2-byte LE bucket length][bucket bytes][2-byte LE key length][key bytes]
```

So a prefix scan for an ASCII string like `"x509_certs/"` matches **zero entries** and the reconcile silently returns an empty inventory (no error — the worst failure mode). The correct prefix to iterate a bucket is:

```
[2-byte LE bucket length][bucket bytes]
```

Implemented as `bucketPrefix(name)`.

---

## Buckets

| Bucket | Key | Value | Notes |
|---|---|---|---|
| `x509_certs` | decimal serial string | `crt.Raw` (raw **DER** bytes) | The issued cert itself |
| `x509_certs_data` | decimal serial string | `CertificateData` JSON | Provisioner metadata; **lowercase** JSON tags |
| `revoked_x509_certs` | decimal serial string | `RevokedCertificateInfo` | **Go-style capitalized** field names, no JSON tags |

### Value formats — two more gotchas

**`x509_certs` holds DER, not PEM.** From step-ca `db/db.go::StoreCertificate`:

```go
db.Set(certsTable, []byte(crt.SerialNumber.String()), crt.Raw)
```

`crt.Raw` is raw DER. Do **not** `pem.Decode` first — that skips every cert. Parse directly:

```go
cert, err := x509.ParseCertificate(raw)
```

**`x509_certs_data`** value is JSON shaped roughly like `CertificateData{ Provisioner: {id, name, type}, ... }` with **lowercase** json tags. A point lookup here populates the inventory's `provisioner` column (otherwise null).

**`revoked_x509_certs`** value (`RevokedCertificateInfo`) has **no JSON tags** — Go default capitalized field names (`Serial`, `Reason`, `RevokedAt`). The mirror struct used to unmarshal must use matching capitalized tags or the fields come back empty.

---

## Serial number form (the #3 gotcha)

step-ca keys all three buckets by the **decimal** string of the serial (`cert.SerialNumber.String()`). The `stepca-api` design specifies **hex** for the SQLite primary key.

Resolution: normalize to **lowercase hex** at read time for both issued and revoked rows:

```go
serialHex := fmt.Sprintf("%x", cert.SerialNumber) // big.Int
```

This matters beyond cosmetics: revoke-by-serial later must match the stored row. If issuance stores decimal and revoke looks up hex (or vice versa), the join silently fails. Normalizing both write paths to hex keeps the primary key internally consistent regardless of step-ca's representation.

---

## Concurrency / read-only lock (resolved: snapshot-on-reconcile)

Badger is an embedded KV store; step-ca holds an **exclusive lock** on its data directory while it is running. Opening the same dir from the reconciler with `WithReadOnly(true)` may fail with a lock error on some filesystems and is fragile in general.

**Resolved: snapshot-on-reconcile.** The reconciler never opens the live step-ca DB. Before each Badger read it copies the configured `-ca-db` directory to a unique temp directory under `-snapshot-dir` (default `os.TempDir()`), opens the COPY read-only, reads, then removes the copy. Implementation is `BadgerSource.withSnapshot` in `internal/reconcile/badger.go`.

A failed or inconsistent copy (e.g. step-ca wrote mid-copy, MANIFEST disagrees with vlog, snapshot dir is out of space) is returned as an error from `Issued`/`Revoked`. `reconcile.tick()` logs it and returns; the loop retries on the next interval. The service does not crash.

Two copies per reconcile pass (one for issued, one for revoked) - the store layer's `Upsert` preserves revocation across passes, so any window where the two snapshots disagree is self-healing on the next tick.

**`cp` vs filesystem snapshot.** The current implementation is a Go-level file copy (`io.Copy` per file under `filepath.Walk`) - works on any filesystem and is the right default for a small CA DB. On a filesystem that supports cheap snapshots (ZFS, btrfs, LVM thin-provisioned), the same idea can be implemented as a snapshot mount and would be faster + atomic. The interface (a fresh read-only Badger handle scoped to one pass) does not change; only the snapshot mechanism does. Swap the `copyTree` call for a snapshot/mount call if/when warranted.

The alternatives below remain valid future paths but are not the current choice:

- **Backfill-only Badger read + webhook for currency** - read Badger once at startup for history, then keep the inventory live via the step-ca issuance webhook. Matches the original layered design; no ongoing copy cost.
- **Concurrent read-only open of the live dir** - was the fragile option this section resolved away from. Don't.

---

## If the reader returns an empty inventory

Order of suspicion:

1. **Wrong backend.** Some step-ca configs use Badger **v1**, or a **SQL** backend (MySQL/Postgres), not Badger v2. This entire document only applies to the v2 Badger codepath. Check what's actually on disk:
   ```
   ls /opt/provider-box/step-ca/db/
   ```
   Badger v2 shows `*.sst`, `*.vlog`, `MANIFEST`, `KEYREGISTRY`, `LOCK`. If it looks like a SQL DSN config instead, the reader needs a different `Source` implementation.
2. **Key encoding regressed** — re-confirm `toBadgerKey` in the pinned smallstep/nosql version.
3. **Bucket names changed** — re-confirm `x509_certs` / `x509_certs_data` / `revoked_x509_certs` against step-ca source.

---

## Live verification

Run against the real CA dir when available:

```bash
STEPCA_API_TOKEN_FILE=/path/to/token \
go run ./cmd/stepca-api \
  -db /tmp/stepca-api.db \
  -ca-db /opt/provider-box/step-ca/db \
  -addr :8443 \
  -reconcile-interval 10s

# then, in another shell:
curl -s -H "Authorization: Bearer $(cat /path/to/token)" \
  http://localhost:8443/certs | jq 'length, .[].common_name'
```

Pass condition: the certs you know step-ca issued during deployment (the **NetBox leaf**, **depot**, and **Keycloak** certs) appear in the list. If they do, the layout is confirmed live and the service can proceed to the revoke proxy. If they don't, start with the empty-inventory checklist above.
