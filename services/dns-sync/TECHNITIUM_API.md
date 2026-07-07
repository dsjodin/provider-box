# Technitium API - verified shape

Verified against `docker.io/technitium/dns-server:13.4.2` on 2026-06-24 by
standing up a throwaway container and probing every endpoint dns-sync calls.
This is the authoritative reference; technitium-dns_design.md sec 3 was
written from memory and several details were wrong (see "Differences from
the design doc" at the bottom).

All endpoints return HTTP 200 even on errors. The body's `status` field is
the real signal: `"ok"`, `"error"`, or `"invalid-token"`. Successful calls
also include a `response` object.

## 1. Authentication

Token is passed as a **query parameter**: `?token=<TOKEN>`. Sending it as
`Authorization: Bearer ...` does NOT work - the server still reports
`Parameter 'token' missing`.

### Create a permanent API token

```
GET /api/user/createToken?user=<USER>&pass=<PASSWORD>&tokenName=<NAME>
```

Response:
```json
{
  "username": "admin",
  "tokenName": "dns-sync-test",
  "token": "c571ed9cab8846cccccfe4baeb4d03042d0600c966687dab074b23a64e25a0f5",
  "status": "ok"
}
```

The created token is permanent (no expiry) and is what dns-sync uses for all
subsequent calls. Sessions opened via `/api/user/login` work too but expire,
so prefer `createToken` for a service identity.

### Invalid token shape

```json
{ "status": "invalid-token", "errorMessage": "Invalid token or session expired." }
```

## 2. Zones

### List zones

```
GET /api/zones/list?token=<TOKEN>
```

Response:
```json
{
  "response": {
    "zones": [
      {
        "name": "0.0.10.in-addr.arpa",
        "type": "Primary",
        "internal": false,
        "disabled": false,
        ...
      },
      { "name": "0.in-addr.arpa", "internal": true, ... },
      { "name": "127.in-addr.arpa", "internal": true, ... }
    ]
  },
  "status": "ok"
}
```

Important: the listing includes Technitium's **built-in `internal: true`**
zones (`0.in-addr.arpa`, `127.in-addr.arpa`, `255.in-addr.arpa`, the v6
empty zone, `localhost`). dns-sync must filter `internal == true` out when
diffing against current state.

### Create zone

```
GET /api/zones/create?token=<TOKEN>&zone=<NAME_OR_CIDR>&type=Primary
```

- For forward zones: pass the FQDN, e.g. `zone=lab.test`.
- For reverse zones: pass an **IPv4 CIDR**, e.g. `zone=10.0.0.0/24`.
  Technitium derives the `in-addr.arpa` zone name from it. Passing the raw
  `0.0.10.in-addr.arpa` name also works.

**Not idempotent.** Calling create on an existing zone returns:
```json
{ "status": "error", "errorMessage": "Zone already exists: lab.test", ... }
```
Treat that specific error as success - it satisfies the EnsureZone contract.

### Delete zone

```
GET /api/zones/delete?token=<TOKEN>&zone=<NAME>
```

Idempotent; deleting a missing zone returns `status: ok`.

## 3. Records

### Add record

```
GET /api/zones/records/add?token=<TOKEN>&zone=<ZONE>&domain=<FQDN>&type=<TYPE>&<rdata>&ttl=<N>
```

Per record type:

| Type | Required rdata params           |
|------|---------------------------------|
| A    | `ipAddress=<IPv4>`              |
| AAAA | `ipAddress=<IPv6>`              |
| PTR  | `ptrName=<canonical FQDN>`      |

`zone` is the parent zone name (NOT the CIDR form). `domain` is the full
record name. For a PTR: `zone=0.0.10.in-addr.arpa`, `domain=10.0.0.10.in-addr.arpa`,
`ptrName=host.lab.test`.

**Idempotent for A records with identical rdata**: adding the same
(name, type, ipAddress) twice is a no-op.

**For PTR**: adding a second PTR for the same name **appends** - the zone
ends up with both PTRs co-existing. This is contrary to a common assumption;
the one-PTR-per-IP rule from technitium-dns_design.md sec 6 is enforced
by dns-sync at the diff layer (canonical name selection) and via an
explicit Delete of the old PTR alongside the Create of the new one.
TechnitiumTarget.Apply runs Create before Delete, so during the apply
window both PTRs are briefly present; the trailing Delete leaves exactly one.

**Fails if zone does not exist**:
```json
{ "status": "error", "errorMessage": "No such zone was found: foo.bar", ... }
```
So EnsureZone must run before adds.

### Update record

```
GET /api/zones/records/update?token=<TOKEN>&zone=<ZONE>&domain=<FQDN>&type=A&ipAddress=<OLD>&newIpAddress=<NEW>&ttl=<N>
```

Takes both old and new rdata; in-place edit of a single resource record.
dns-sync uses Create + Delete instead (simpler diff, same end state), so
this endpoint is documented but not called.

### Delete record

```
GET /api/zones/records/delete?token=<TOKEN>&zone=<ZONE>&domain=<FQDN>&type=<TYPE>&<rdata>
```

Idempotent; deleting a missing record returns `status: ok` with empty
response. The rdata identifier is required (the record set may contain
multiple values of the same type, e.g. several A records on one name).

### Get records in a zone

```
GET /api/zones/records/get?token=<TOKEN>&zone=<ZONE>&domain=<ZONE>&listZone=true
```

The `listZone=true` flag returns every record in the zone, not just the
ones matching the exact `domain`. Response shape:

```json
{
  "response": {
    "zone": { ... },
    "records": [
      {
        "name": "test1.lab.test",
        "type": "A",
        "ttl": 3600,
        "rData": { "ipAddress": "10.0.0.10" }
      },
      {
        "name": "lab.test",
        "type": "SOA",
        "rData": { "primaryNameServer": "lab.test", ... }
      },
      { "name": "lab.test", "type": "NS", "rData": { "nameServer": "lab.test" } }
    ]
  },
  "status": "ok"
}
```

Auto-generated NS and SOA records appear in the listing - dns-sync must
ignore record types it does not manage (anything other than A, AAAA, PTR).

## 4. Server settings

### Get all settings

```
GET /api/settings/get?token=<TOKEN>
```

Returns the full server config. Forwarder-related fields:

```json
{
  "forwarders": null,
  "forwarderProtocol": "Udp",
  "concurrentForwarding": true,
  "forwarderRetries": 3,
  "forwarderTimeout": 2000,
  "forwarderConcurrency": 2,
  "recursion": "AllowOnlyForPrivateNetworks",
  "proxy": "None"
}
```

`forwarders` is `null` until set, then an array of IP strings (`["8.8.8.8"]`).

### Set settings (partial update)

```
GET /api/settings/set?token=<TOKEN>&forwarders=<CSV>
```

Pass only the fields you want to change; everything else is preserved
(verified by writing only `forwarders` and confirming `dnssecValidation`
was untouched).

Multiple forwarders are comma-separated and URL-encoded:
`forwarders=8.8.8.8%2C1.1.1.1`.

**Naturally idempotent.** Re-sending the same value returns `status: ok`
with the same state. No "already set" error to special-case (unlike
`/api/zones/create`).

**Empty value is a no-op.** `forwarders=` does not clear the field; the
previous value is preserved. To explicitly clear forwarders, look up the
upstream's "clear" mechanism per Technitium docs (not needed by dns-sync,
which always sets a non-empty value).

### Web service TLS (verified against 13.4.2)

The console/API HTTPS listener is disabled by default and is enabled via
the same `settings/set` endpoint:

```
GET /api/settings/set?token=<TOKEN>
      &webServiceEnableTls=true
      &webServiceTlsPort=53443
      &webServiceTlsCertificatePath=<container path to .pfx>
      &webServiceTlsCertificatePassword=<pfx password>
```

The certificate must be **PKCS#12** (`.pfx`); PEM is not accepted for
`webServiceTlsCertificatePath`. Convert with:

```
openssl pkcs12 -export -in chain.pem -inkey key.pem -out technitium.pfx -passout pass:<pw>
```

The TLS listener comes up within a few seconds of the `settings/set` call -
no restart needed. Related fields visible in `settings/get`:
`webServiceEnableTls`, `webServiceTlsPort` (default 53443),
`webServiceTlsCertificatePath`, `webServiceTlsCertificatePassword`
(masked as `************` on read), `webServiceUseSelfSignedTlsCertificate`,
`webServiceHttpToTlsRedirect`.

## 5. Differences from technitium-dns_design.md sec 3

The design doc's from-memory shape was close on path names but wrong on
several details. Captured here so the design can be updated:

1. **Token transport.** The design said "token-authenticated" without
   specifying transport. The real flow is `?token=` query string only,
   no Bearer header support.
2. **Endpoint paths.** The design wrote `/api/zones/records/add` etc.
   These match. But the design omitted `/api/zones/list` and
   `/api/zones/records/get?listZone=true`, both required for the diff
   read side.
3. **Zone create is not idempotent.** The design implied straightforward
   create; in fact a duplicate returns an error and must be treated as
   success.
4. **Reverse zones.** The design said "ensure the needed reverse zones
   exist in Technitium (e.g. per-/24 in-addr.arpa), creating them if
   absent." Worth noting: Technitium accepts a CIDR (`10.0.0.0/24`) on
   create and derives the zone name itself - lets dns-sync skip the
   octet-reversal arithmetic for create (still needed for record names).
5. **Built-in zones.** Not mentioned in the design - Technitium ships
   with `0.in-addr.arpa`, `127.in-addr.arpa`, `255.in-addr.arpa`, an
   IPv6 empty zone, and `localhost` marked `internal: true`. dns-sync
   must filter these out of "current state."
6. **PTR add semantics (corrected).** Initially this doc claimed PTR adds
   replace; verification proved otherwise - PTR adds APPEND, leaving the
   zone with multiple PTRs on the same name until one is explicitly
   deleted. The "one-PTR-per-IP rule" therefore must be enforced by
   dns-sync: pick a canonical name in the diff (lex-smallest), and emit
   Create(new) plus Delete(old) when it changes. TechnitiumTarget.Apply
   runs Creates before Deletes so the trailing Delete cleans up the
   stale PTR.
7. **Error shape.** Errors are HTTP 200 with `status: error` and a
   `stackTrace` field in the body. The design did not specify; callers
   must parse JSON, not rely on HTTP status.
8. **Token creation.** The design said "a login/token-create call to
   obtain the API token." Specifically: `/api/user/createToken?user=&pass=&tokenName=`
   for a permanent service token. `/api/user/login` exists but issues
   a session token that expires.
