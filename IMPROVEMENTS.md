# Improvement Suggestions

Non-documentation issues found during the 2026-07-08 documentation
reconciliation audit. Every entry was verified against the code on main
before inclusion. None of these are implemented by the documentation pass;
each needs its own change.

Format per entry: what, where, why it matters, suggested fix, estimated
blast radius.

---

## 1. Technitium bootstrap uses hardcoded first-boot admin credentials

- What: All bootstrap-phase Technitium API calls authenticate with the
  literal first-boot credentials `admin`/`admin`, and the admin password is
  never rotated.
- Where: `bootstrap/technitium.sh:295` (login for settings calls),
  `bootstrap/technitium.sh:417` (createToken).
- Why it matters: On a re-run after the operator changed the admin password
  (as the module's own comments tell them to), every API step fails. Until
  the operator changes it, the DNS server's admin console is reachable on
  the lab network with default credentials.
- Suggested fix: Add `TECHNITIUM_ADMIN_USER` / `TECHNITIUM_ADMIN_PASSWORD`
  to `provider-box.env`, and have `--technitium` rotate the first-boot
  password to the configured value via `/api/user/changePassword` (verified
  endpoint family in TECHNITIUM_API.md) on first bootstrap, then
  authenticate with the env credentials on re-runs.
- Blast radius: Medium. `technitium.sh` only, plus two env example lines
  and validation; changes re-run behavior on hosts where the password was
  already changed manually.

## 2. --netbox leaks one live superuser API token per run

- What: `netbox_api_auth_header` provisions a fresh superuser token on every
  `--netbox` run for the seeding calls and never deletes it afterwards.
- Where: `bootstrap/netbox.sh:411-439` (provision), no corresponding
  revoke anywhere in `do_netbox`.
- Why it matters: Re-running `--netbox` accumulates unbounded live
  superuser API tokens in NetBox; each one is full-privilege and never
  expires.
- Suggested fix: DELETE the seeding token (its id is in the provision
  response) at the end of `seed_netbox_via_api`, or reuse the dns-sync
  token flow's description tagging so the sweep in suggestion 3 catches it.
- Blast radius: Small. One function in `netbox.sh`; no operator-visible
  behavior change.

## 3. Orphaned-token sweep only matches description-tagged tokens

- What: The pre-provisioning cleanup deletes only tokens whose description
  is exactly "provider-box dns-sync".
- Where: `bootstrap/netbox.sh:479-499` (filter at line 484).
- Why it matters: Tokens created by older code versions (before the
  description was added) or with an edited description are never retired,
  so stale live credentials linger; combined with suggestion 2 the token
  list grows on every deploy.
- Suggested fix: Keep the description filter as the primary match but log a
  count of remaining tokens owned by the superuser, so accumulation is at
  least visible; document manual cleanup. Full name-pattern matching is
  probably over-engineering for the lab scope.
- Blast radius: Small. One function in `netbox.sh`.

## 4. docker_pkgs Docker CE fallback hardcodes the Debian repo

- What: The Docker CE install path always uses
  `https://download.docker.com/linux/debian` with the host's
  `VERSION_CODENAME`.
- Where: `bootstrap/provider-box.sh:192` (gpg key URL),
  `bootstrap/provider-box.sh:198-201` (repo line).
- Why it matters: On Ubuntu the codename (for example `noble`) does not
  exist in the Debian repo, so `apt-get update` 404s and bootstrap fails.
  README documents the Debian assumption, but the code could just handle
  it.
- Suggested fix: Read `ID` from `/etc/os-release` and use
  `linux/${ID}` (docker.com publishes matching `debian` and `ubuntu`
  repos), failing fast on other IDs.
- Blast radius: Small. One function; only affects hosts without Docker
  preinstalled.

## 5. fail messages and checks hardcode compose container names

- What: Error text and a runtime check assume default compose project
  naming (`<dir>-<service>-1`).
- Where: `bootstrap/ca.sh:62`, `bootstrap/ca.sh:73`
  (`docker logs step-ca-step-ca-1`), `bootstrap/dns-sync.sh:175`
  (`docker inspect ... dns-sync-dns-sync-1`, with a compose-based
  fallback).
- Why it matters: If compose changes its naming convention (or a
  `COMPOSE_PROJECT_NAME` is set), the suggested command in the fail message
  is wrong and the inspect fast-path never matches.
- Suggested fix: Use `docker compose -f <file> logs step-ca` phrasing in
  fail messages and rely solely on the `docker compose ps` form in
  `verify_dns_sync_running`.
- Blast radius: Small. Message strings plus one conditional.

## 6. Vestigial TECHNITIUM_FORWARDER reference and unused set-forwarder subcommand

- What: `TECHNITIUM_FORWARDER` was removed from the env model (CHANGELOG
  2026-07-06), but dns-seed still reads it as the default for
  `-forwarders`, and no bootstrap module invokes
  `dns-seed set-forwarder` anymore (the technitium module owns the
  forwarder setting).
- Where: `services/dns-sync/cmd/dns-seed/main.go:175` (env read);
  `runSetForwarder` at `services/dns-sync/cmd/dns-seed/main.go:169-205`.
- Why it matters: Dead configuration surface: an operator setting
  `TECHNITIUM_FORWARDER` expects an effect it no longer has, and the
  subcommand invites a second writer for a setting that has exactly one
  owner by design.
- Suggested fix: Drop the env fallback (keep the explicit flag), or remove
  the `set-forwarder` subcommand entirely.
- Blast radius: Small. dns-seed CLI only; no bootstrap path uses it.

## 7. services/stepca-api absorbed into services/dashboard (RESOLVED)

- What: The design-stage `services/stepca-api` has been folded into the new
  `services/dashboard` as its read-only "Certificates" panel. The reusable
  step-ca BadgerDB reader (`reconcile/badger.go`) was migrated to
  `services/dashboard/internal/certs`; the phase-2 collector parts (SQLite
  inventory, reconcile loop, token-authed HTTP API) were dropped as they are
  explicitly out of v1 scope. The `services/stepca-api/` directory has been
  removed.
- Where: now `services/dashboard/internal/certs/`.
- Status: `STEPCA_STORAGE.md` is retained - the dashboard's cert reader still
  depends on the storage-format details it documents. `step-ca_api_design.md`
  is now historical (it describes the collector API that became phase 2 of the
  dashboard); kept as background, not an active spec.
- Note: `services/dashboard` itself is standalone and not yet wired into
  bootstrap - that (a `--dashboard` module, inclusion in `--all`) is the
  dashboard's phase 2, tracked in `services/dashboard/README.md`.

## 8. --unbound host resolver takeover has no restore path

- What: `configure_resolv_conf` disables `systemd-resolved` and rewrites
  `/etc/resolv.conf` unconditionally, and `--unbound` has no `--remove`.
- Where: `bootstrap/provider-box.sh:215-224`, called from
  `bootstrap/dns.sh:38`.
- Why it matters: The technitium module got a careful marker-based
  disable/restore flow plus resolution verification for the same
  operation; the unbound path still breaks host DNS permanently if unbound
  fails to start, with manual recovery only.
- Suggested fix: Reuse the technitium module's pattern: verify resolution
  before/after, use a marked drop-in for the stub listener instead of
  disabling the whole service, and add a `--unbound --remove` that
  restores stock configuration.
- Blast radius: Medium. Changes host-level behavior of the default
  backend; needs care on hosts already converted by the old flow.

## 9. Four copies of the same JSON field extractor, six copies of the CA readiness gate

- What: `json_string_field` (netbox.sh:400),
  `technitium_json_string_field` (technitium.sh:282),
  `sftp_json_string_field` (sftp.sh:204), and
  `authentik_json_string_field` (authentik.sh:228) are the same sed
  one-liner; `require_ca_ready_for_<service>` is duplicated nearly
  verbatim in six modules (authentik.sh:110, depot.sh:66, keycloak.sh:133,
  netbox.sh:183, sftp.sh:72, technitium.sh:55).
- Why it matters: Fixes to one copy (for example the CA reachability check
  or a JSON edge case) silently miss the other five; this is beyond the
  "three similar lines beat an abstraction" threshold.
- Suggested fix: Hoist one `json_string_field` and one
  `require_ca_ready_for <service-label>` helper into
  `bootstrap/provider-box.sh` beside the other shared helpers.
- Blast radius: Medium surface (seven files) but mechanical; no behavior
  change intended.

## 10. Dead fallback ${KEYCLOAK_PORT:-8443} in the NetBox service seed

- What: The service seed block defaults `KEYCLOAK_PORT` to 8443 even
  though `require_netbox_vars` already fails when it is unset.
- Where: `bootstrap/netbox.sh:118` vs the requirement at
  `bootstrap/netbox.sh:135`.
- Why it matters: The fallback can never fire, and it suggests a
  different contract (optional variable) than the validation enforces.
- Suggested fix: Use `${KEYCLOAK_PORT}` plainly.
- Blast radius: Trivial.

## 11. remove_netbox deletes the certificate directory; every other module preserves certs

- What: `--netbox --remove` runs `rm -rf "${NETBOX_DIR}/certs"`, while
  depot/technitium/sftp removals explicitly preserve their cert dirs (and
  validate at deploy time that cert dirs live outside the runtime dir for
  exactly that reason).
- Where: `bootstrap/netbox.sh:762`.
- Why it matters: Redeploying NetBox after a remove always burns a new
  step-ca certificate instead of reusing a valid one, inconsistent with
  the documented "certificates are preserved" convention.
- Suggested fix: Stop deleting `NETBOX_DIR/certs` on remove; the
  identity-aware reuse logic already handles stale certs.
- Blast radius: Small. One line; behavior change only on remove/redeploy
  cycles.

## 12. Technitium settings API secrets travel in the query string

- What: The bootstrap settings calls send the session token and the pfx
  password as URL query parameters
  (`webServiceTlsCertificatePassword=...`).
- Where: `bootstrap/technitium.sh:358-364` (TLS enable), token usage
  throughout the API helpers.
- Why it matters: Query strings can end up in shell history, process
  listings, and proxy/server logs. The Technitium API requires the token
  as a query parameter (verified in TECHNITIUM_API.md), so exposure is
  partly inherent, but the calls run over plain HTTP on 127.0.0.1 during
  bootstrap.
- Suggested fix: Use `curl --data-urlencode` with POST (drop `--get`) so
  parameters move to the request body if the API accepts it (Technitium
  accepts POST form bodies for `/api/settings/set`); verify against the
  pinned image first.
- Blast radius: Small. Local-only exposure today; two functions.

## 13. AGENTS.md is stale (not edited by this pass by instruction)

- What: The agent rules predate Authentik, Technitium, dns-sync, and the
  DNS backend model.
- Where: `AGENTS.md:66-97` ("Existing Services" omits Authentik,
  Technitium, dns-sync; "DNS Integration" requires services to "be
  resolvable via Unbound"); `AGENTS.md:229` (seeding imports only
  `config/unbound.records`).
- Why it matters: Agents follow these rules literally; "resolvable via
  Unbound" contradicts the technitium backend, and the stable-services
  list understates what must not be broken.
- Suggested change: Add Authentik, Technitium, and dns-sync to the stable
  services list; reword DNS Integration to "resolvable via the selected
  DNS backend (generated built-in record list `provider_box_builtin_fqdns`)";
  mention `config/dns.seed` beside `config/unbound.records` in the seeding
  section.
- Blast radius: Documentation only.

## 14. PROJECT_CONTEXT.md is stale (not edited by this pass by instruction)

- What: Core components and the container image list predate Authentik,
  Technitium, and dns-sync.
- Where: `PROJECT_CONTEXT.md:67-97` (components), 140-153 (image list
  missing `AUTHENTIK_IMAGE`, `AUTHENTIK_POSTGRES_IMAGE`,
  `TECHNITIUM_IMAGE`, `DNS_SYNC_IMAGE`), depot table at 113-119 (missing
  the unauthenticated `/products/v1/bundles/lastupdatedtime` alias).
- Suggested change: Add the three services to the component list, extend
  the image list, add the missing depot path row, and describe the
  two-backend DNS model in the Runtime/Service model sections.
- Blast radius: Documentation only.

## 15. env drift check only detects missing variables, not stale ones

- What: `check_provider_env_is_current` flags example variables missing
  from the local env, but a variable removed from the example (like
  `TECHNITIUM_FORWARDER` was) lingers in operator envs forever with no
  signal.
- Where: `bootstrap/provider-box.sh:87-113`.
- Why it matters: Removed variables keep appearing to work (they are
  sourced and exported), masking the fact that nothing consumes them.
- Suggested fix: Emit a non-fatal notice listing local variables that no
  longer exist in the example.
- Blast radius: Small. One function; informational output only.

No dead variables were found in `config/provider-box.env.example`: every
active variable in the example is consumed by at least one module or
template, and every operator-supplied variable required by the modules
exists in the example.
