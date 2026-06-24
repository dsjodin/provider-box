# technitium-dns — Design Blueprint

Replace provider-box's Unbound stage with **Technitium DNS Server** to get an API- and dashboard-driven DNS layer, with **NetBox as the source of truth** and a reconcile sync pushing records into Technitium.

> Decision context: Unbound in 1.0 is config-file-rendered and re-run-driven — predictable, but it can't be edited live (runtime changes get overwritten on the next `--unbound`) and has no API or UI. Technitium gives a real HTTP API + web console in a single container, doing both authoritative and recursive resolution. This is the piece that turns "edit a flat file and re-render" into "NetBox drives DNS."

---

## 1. Scope

**In scope**
- New bootstrap stage deploying Technitium as a container (replaces / supersedes `--unbound`).
- A **bring-up seed file** the operator pre-populates, applied at first deploy (see §5).
- NetBox -> Technitium reconcile sync (forward + reverse zones).
- Keep NetBox authoritative; Technitium is a generated/downstream consumer.

**Out of scope**
- Forcing anyone onto it. `--unbound` can remain for backward compat, or be deprecated — but neither DNS stage goes silently into `--all` (the `--all`/Unbound resolv.conf hijack is a known 1.0 trap; don't repeat it).
- Replacing the user's existing external DNS (e.g. MS DNS) is a *deployment choice*, not forced by the fork. See §7.

---

## 2. Architecture

```
 NetBox (source of truth) --API read--> reconcile-sync --Technitium HTTP API--> Technitium
                                                                                    |
                                              lab clients / VCF --DNS queries-------+
       humans --web console--> Technitium   (view; edits flow FROM NetBox, not here)
```

NetBox holds IPs + DNS names (IPAM). The sync reads NetBox and reconciles Technitium's zones to match — create/update/delete. Technitium serves the lab; its console is for *viewing* and emergency use, not the primary edit path.

This is the upgrade over Unbound: 1.0 had NetBox and Unbound both fed from the same `unbound.records` file (NetBox passive). Here NetBox is authoritative and *drives* DNS via API.

---

## 3. Technitium capabilities

What it brings (single container, free/open-source):
- Authoritative **and** recursive resolver in one process — covers what Unbound did.
- Full **HTTP API** for zones and records (create/update/delete forward and reverse).
- A **web console** (dashboard) — the "see and manage DNS" surface you wanted.
- Conditional forwarders (point non-lab queries upstream), DoH/DoT, query logging.

> WARNING — verify before scripting: Technitium's API is token-authenticated and JSON over HTTP, with endpoints along the lines of `/api/zones/create`, `/api/zones/records/add`, `/api/zones/records/update`, `/api/zones/records/delete`, and a login/token-create call to obtain the API token. The **exact endpoint paths, parameter names, and token flow should be confirmed against the running version's API docs** — this describes the shape from memory and these can shift between releases. Stand up the container, create an API token in the console, and pin the sync to the real endpoints.

---

## 4. Sync model (replaces the unbound.records render)

A small `dns-sync` service/job, idempotent and reconcile-based (the robust pattern, not webhook-fragile):

1. Read from NetBox via API: IP addresses and their assigned DNS name(s), plus the prefixes (for reverse zones).
2. Compute desired state: forward A/AAAA records per name, PTR records per IP.
3. Diff against Technitium's current zone contents (read via API).
4. Apply create/update/delete to converge.
5. Run on a short interval (systemd timer / cron, e.g. 30-60s) and on demand.

Reconcile beats event/webhook here: simpler, survives missed events and restarts, and is easy to reason about. Same philosophy as the `stepca-api` reconcile job.

**Direction of truth — commit to it.** NetBox is authoritative; Technitium is generated. Do **not** hand-edit Technitium and expect it to stick — the next reconcile overwrites it, exactly like re-running `--unbound` overwrote manual Unbound edits. Same lesson, new tool. The console is read/observe/break-glass only.

---

## 5. Bring-up seed file (first-deploy pre-population)

**The problem it solves.** At first deploy there is a chicken-and-egg: the lab needs working DNS *before* the NetBox-driven pipeline is live. NetBox isn't populated yet, the sync needs NetBox reachable, and NetBox's own FQDN (and the other service FQDNs) must already resolve. VCF nodes, gateways, and the provider-box services need names immediately. The seed file gives the operator a declarative list of records that the bootstrap applies **directly to Technitium at deploy time**, so DNS is functional from the first boot — independent of NetBox readiness.

**Format — reuse what already exists.** Use the same format as 1.0's `unbound.records`:

```
<fqdn> <ip>
<fqdn> <ip/cidr>     # cidr lets the seed derive the reverse zone / NetBox prefix
```

Reusing this format means the operator's existing records carry over unchanged, and the **MS DNS PowerShell exporter already produces it** — so an existing zone can be exported straight into the seed file. Suggested path: `config/dns.seed` (or keep the `unbound.records` filename for drop-in continuity).

**How it stays consistent with "NetBox is the source of truth."** This is the part to get right, or the first reconcile wipes the seeded records. The seed is applied to **two** places at bring-up:

1. **Directly into Technitium** via API (idempotent) — immediate availability, no dependency on NetBox.
2. **Imported into NetBox** as initial IPAM data — so the seeded names/IPs become part of the source of truth.

Once NetBox holds them, the ongoing reconcile sees the seeded records as *desired state* and leaves them in place. The seed is therefore "initial load of the source of truth + an immediate direct-to-DNS apply," not a parallel authority. This mirrors what 1.0 did (seed both DNS and NetBox from one file), adapted to the API-driven model.

**Operator discipline (same lesson again).** The seed file is a **first-deploy convenience, not the ongoing edit path.** After bring-up, NetBox is the truth — editing `dns.seed` later and expecting reconcile to honor it will disappoint, because reconcile reflects NetBox. Re-applying the seed on a later deploy is safe (idempotent) but the canonical place to change records post-bootstrap is NetBox.

**Auto-generated service records are separate.** The bootstrap still auto-generates provider-box's own service FQDNs (ca, netbox, auth, depot, s3, sftp, syslog -> host IP) and applies them at bring-up, exactly as 1.0's Unbound did — the operator does **not** need to put these in the seed file. The seed file is for *external* infra (VCF nodes, gateways, vCenter, ESXi, etc.).

**Alternative if you don't want everything in NetBox.** If some seeded records should never live in NetBox, the other option is a non-destructive reconcile: tag sync-managed records (or scope the sync to zones/records it owns) so it only deletes what it created, leaving seed/console records alone. The import-to-NetBox approach above is preferred for a single source of truth; tagging is the fallback if you need DNS records outside NetBox's scope.

---

## 6. Reverse zones / PTR

The old Unbound render generated PTRs automatically; don't lose that. The sync (and the seed apply) must:
- Ensure the needed reverse zones exist in Technitium (e.g. per-/24 `in-addr.arpa`), creating them if absent.
- Generate one PTR per IP from the IP->name mapping (NetBox for ongoing, the seed's `ip/cidr` for bring-up).
- Handle the one-PTR-per-IP rule (an IP maps to a single canonical name) — pick the primary/canonical name when an IP has several DNS names.

VCF is strict about forward+reverse consistency, so PTR correctness is not optional for this lab's purpose.

---

## 7. Relationship to existing DNS (MS DNS today)

Currently the lab uses MS DNS, and provider-box's Unbound was skipped. Two coherent end states:

- **Technitium becomes the lab's DNS**, replacing MS DNS. Seed at bring-up, then NetBox->Technitium sync is the pipeline; clients/VCF point at Technitium. Cleanest for an all-in-one provider-box.
- **Technitium runs but MS DNS stays primary** — less useful; mostly only worth it if MS DNS must remain for non-lab reasons.

Migration aid: the `unbound.records` you already exported from MS DNS is a ready **seed file** (§5) — drop it in as `config/dns.seed` and first bring-up populates both Technitium and NetBox from it.

Bonus: the **IPv4/IPv6 family-mismatch** validation error you hit (stray AAAA from MS DNS auto-registration) disappears with a controlled Technitium zone driven by NetBox/seed — you only create the records you intend, so no surprise AAAA records.

---

## 8. Auth / secrets

- Technitium admin password and the API token -> the fork's secrets mechanism (SOPS/age), not plaintext env.
- `dns-sync` needs a NetBox API token (read) and a Technitium API token (write); both via the same secrets path.
- TLS on Technitium's console/API; cert from step-ca (dogfood the CA).

---

## 9. Bootstrap integration

- New stage: `provider-box.sh --technitium` (or `--dns` with a backend choice). Deprecate or keep `--unbound` alongside.
- Neither DNS stage is pulled into `--all` without an explicit opt-in/service list — fix the `--all` trap at the same time.
- **Bring-up order**: deploy Technitium -> apply auto-generated service records + the `dns.seed` file directly to Technitium (DNS now works) -> when NetBox is up, import the seed into NetBox -> start the `dns-sync` timer. DNS is usable well before NetBox is ready.
- Does **not** rewrite the host's `/etc/resolv.conf` or disable `systemd-resolved` unless the operator opts in — 1.0's Unbound did this unconditionally and it's hostile to existing-DNS setups.
- Runs as a non-root container uid; data dir chowned at deploy (apply the CA-bug ownership lesson proactively).
- `dns-sync` ships as its own stage/timer, depends on NetBox + Technitium being up.

---

## 10. Open decisions

- **Technitium vs PowerDNS**: Technitium = simplest (one container, console + API built in). PowerDNS = SQL-backed, existing NetBox integrations (netbox-plugin-dns + PowerDNS sync tooling) so less custom sync code, but more moving parts (auth + recursor + PowerDNS-Admin UI). For footprint/simplicity -> Technitium; to reuse existing integrations and write less sync code -> PowerDNS. This blueprint assumes Technitium; the sync/seed design is largely portable.
- **Seed consistency model**: import seed into NetBox (single source of truth, preferred) vs tagged non-destructive reconcile (allows DNS records outside NetBox). Decide in §5 terms.
- **Sync language**: align with whatever `stepca-api` uses (likely Go) so the fork has one toolchain — but the sync is mostly NetBox-API-read + DNS-API-write, so Python is also fine here. Decide once for both services.
- **Keep `--unbound`?**: deprecate-and-remove (clean) vs keep-as-option (compat). Lab-fork bias -> deprecate once Technitium proves out.

---

## 11. Suggested build order

1. Stand up Technitium in a container; create an API token; **confirm the real API endpoints** (closes the §3 verification gap before any code).
2. Seed applier: parse `config/dns.seed` (the `unbound.records` format) -> create forward + reverse records directly via the Technitium API, idempotently. This alone gives a working first-boot DNS.
3. `dns-sync`: NetBox read -> desired forward records -> reconcile into Technitium (forward only first).
4. Add reverse zones + PTR generation (shared between seed applier and sync).
5. Seed -> NetBox import at bring-up, so reconcile is consistent with seeded records.
6. Conditional forwarder config (non-lab queries -> upstream).
7. Bootstrap stage + compose template + secrets wiring + bring-up ordering (§9).
8. Cut clients/VCF over (or seed from the existing `unbound.records`).
9. Deprecate `--unbound`.
