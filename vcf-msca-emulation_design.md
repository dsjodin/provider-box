# vcf-msca-emulation - Design Blueprint

Emulate a **Microsoft ADCS Certificate Authority Web Enrollment** endpoint (`certsrv`)
in front of **step-ca**, so **VCF / SDDC Manager** can automate certificate replacement
against this lab's CA the same way it does against a real Microsoft CA.

> Decision context: VCF supports two hands-off CA integrations - ACME and "Microsoft CA".
> The Microsoft path works by having SDDC Manager POST a CSR to a Windows `certsrv` web
> enrollment site and GET the issued cert back. This lab runs step-ca, which has no such
> interface, so VCF cert replacement here is manual today (paste a CSR into the dashboard
> `/csr` page, which calls `POST /api/csr/sign` -> `deploy.SignCSR`). This blueprint asks:
> can a shim speak the `certsrv` wire contract VCF expects and translate each enrollment
> into a step-ca signing? Answer: yes, and cheaply, because the signing seam already
> exists.

---

## 1. Feasibility conclusion

**Feasible, low-risk.** The `certsrv` "protocol" is not a real API - it is about five
stable HTTP endpoints returning HTML and binary blobs, driven over HTTPS with Basic Auth.
The server side is small, and the CSR -> cert step is already implemented and in
production use in this repo (`SignCSR`, `services/control-plane/internal/deploy/sign.go`).

No new signing mechanism, provisioner, or step-ca configuration change is required. The
shim is a thin HTTP front-end plus a small `ReqID -> cert` store.

The one thing this document cannot do is byte-verify the contract against a live SDDC
Manager (none is available here). See section 7.

---

## 2. Scope

**In scope**
- A `certsrv`-shaped HTTP surface served by the existing Go control-plane on a dedicated
  TLS listener with Basic Auth.
- Translation of each enrollment into `deploy.SignCSR` (reused verbatim).
- Serving the step-ca root/intermediate chain in the formats the client fetches.
- Config keys and validation entries.

**Out of scope**
- NTLM / Kerberos / Windows Integrated Auth. VCF supports Basic Auth for `certsrv`
  (Broadcom docs require enabling the Basic Authentication role on the real MS-CA), so
  Basic over TLS is sufficient.
- A separate container or nginx terminator. Chosen shape is in-process in the control
  plane (it already holds the docker socket, env, CA data, and readiness plumbing);
  isolation comes from a separate listener/port, not a separate process.
- Certificate revocation / renewal-count semantics beyond what the client parses. step-ca
  owns revocation (`/crl`); the shim reports `nRenewals=0`.
- Replacing the existing dashboard `/csr` path. That stays; this adds a second, VCF-shaped
  front door onto the same `SignCSR`.

---

## 3. Architecture

```
  VCF / SDDC Manager --HTTPS + Basic Auth--> /certsrv/* (dedicated listener)
                                                  |
                                                  |  parse CSR from certfnsh.asp POST
                                                  v
                                          deploy.SignCSR(ctx, env, csrPEM)
                                                  |  step ca sign (provisioner "admin")
                                                  v
                                              step-ca  ---> leaf + chain
                                                  |
                          ReqID -> leaf store <---+   (certnew.cer / certnew.p7b GET)
```

VCF drives the flow: probe credentials, POST a CSR, read back a `ReqID`, GET the issued
leaf, GET the CA chain. The shim maps that flow onto one `SignCSR` call and serves the
step-ca root/intermediate for the chain fetch.

---

## 4. The certsrv contract to emulate (server side)

Confirmed from the canonical ADCS client `magnuswatn/certsrv`, which mirrors how ADCS web
enrollment clients - including SDDC Manager - drive the site. The shim serves:

| Endpoint | Method | Request | Response the client parses |
|---|---|---|---|
| `/certsrv/` | GET | Basic Auth | 200 with valid creds, 401 without (credential probe) |
| `/certsrv/certfnsh.asp` | POST | form fields `Mode=newreq`, `CertRequest=<PEM CSR>`, `CertAttrib=CertificateTemplate:<tmpl>...`, `FriendlyType`, `TargetStoreFlags=0`, `SaveCert=yes` | HTML body containing `certnew.cer?ReqID=<n>&` (client regex `certnew.cer\?ReqID=(\d+)&`) |
| `/certsrv/certnew.cer?ReqID=<n>&Enc=b64` | GET | Basic Auth | issued leaf, header `Content-Type: application/pkix-cert` |
| `/certsrv/certnew.cer?ReqID=CACert&Enc=b64&Renewal=<k>` | GET | Basic Auth | CA cert, `application/pkix-cert` |
| `/certsrv/certnew.p7b?ReqID=CACert&Enc=b64&Renewal=<k>` | GET | Basic Auth | CA chain as PKCS#7, `Content-Type: application/x-pkcs7-certificates` |
| `/certsrv/certcarc.asp` | GET | Basic Auth | HTML containing `var nRenewals=<k>;` (client regex `var nRenewals=(\d+);`) |

Behavioral notes to encode:
- **Always issue synchronously.** step-ca auto-issues, so the shim never returns the
  "Certificate Pending" or "Certificate Denied" HTML that a real CA emits when a request
  needs manager approval. Every accepted CSR yields a `ReqID` immediately.
- **certfnsh.asp** parses the CSR out of the `CertRequest` form field, calls `SignCSR`,
  allocates a `ReqID`, stores `ReqID -> leaf`, and emits an HTML page whose body contains
  the `certnew.cer?ReqID=<n>&` link (that substring is all the client needs).
- **certnew.cer** returns the **leaf only** (the client validates the `application/pkix-cert`
  content type and treats the body as a single cert). `Enc=b64` returns PEM/base64;
  `Enc=bin` returns DER. The chain is fetched separately.
- **certnew.p7b** returns a degenerate PKCS#7 (`SignedData`, no content, certs-only)
  bundling the step-ca intermediate + root, so VCF can build trust.
- **CertAttrib template name** is accepted and checked against one configured allowed
  name (`VMSCA_TEMPLATE`, default `VMware`); a mismatch is rejected, otherwise it is
  ignored. step-ca's provisioner profile - not the MS template - governs the resulting
  certificate.

---

## 5. Integrated design (reuse, do not rebuild)

New package `services/control-plane/internal/msca`, wired into the control-plane server.

**Handlers** - `internal/msca/certsrv.go`, the six endpoints above.
- `certfnsh.asp` reuses `deploy.SignCSR(ctx, env, csrPEM)` verbatim: same provisioner
  (`admin` / `CA_PROVISIONER_NAME`), same password file, same full-chain guarantee. This
  is the single place issuance happens today, so VCF-issued certs are identical to
  dashboard-issued and `IssueCert`-issued ones.
- `SignCSR` returns a full-chain PEM. Split it: the first `CERTIFICATE` block is the leaf
  for `certnew.cer`; the CA chain for `certnew.p7b` is built from
  `${CA_DATA_DIR}/certs/intermediate_ca.crt` + `root_ca.crt` (files already read elsewhere
  in the tree - see `internal/certs` and `IssueCert`). Encode the chain as PKCS#7 via
  `crypto/x509` + `encoding/asn1` (degenerate SignedData) - no new dependency needed.

**State** - a `ReqID -> leaf` map guarded by a mutex.
- In-memory is sufficient for a single-node lab: VCF POSTs then GETs seconds apart.
- Trade-off to note: entries are lost on control-plane restart, so a VCF retry mid-flight
  would re-enroll (harmless - it just signs again). Optional hardening: persist issued
  leaves under `WORKDIR/msca` keyed by `ReqID`. Recommend starting in-memory.
- `ReqID` is a monotonic counter (per-process); `CACert` is a reserved sentinel handled
  before the numeric lookup.

**Auth + TLS** - a dedicated listener, separate from the admin control-plane port.
- Port `VMSCA_PORT` (default 8443), FQDN `VMSCA_FQDN` (e.g. `certsrv.sddc.lab`).
- Serve TLS with a step-ca leaf issued through `deploy.IssueCert`
  (`internal/deploy/stepca.go`) for `VMSCA_FQDN`, reusing the existing readiness gate
  (`requireCAReady`) and full-chain/ownership handling.
- HTTP Basic Auth against `VMSCA_USERNAME` / `VMSCA_PASSWORD`, compared with
  `crypto/subtle.ConstantTimeCompare`. A missing/invalid header returns 401 with
  `WWW-Authenticate: Basic realm="certsrv"` - which is exactly the credential-probe
  behavior the client keys on at `/certsrv/`.
- Keeping this off the admin control-plane port isolates the external-facing surface even
  though it runs in the same process.

**Startup** - gate on `VMSCA_ENABLE`. When true, `cmd/control-plane/main.go` starts the
second listener after the CA is ready; when false, nothing binds.

**Config** - add to `config/labprovider.env.example` and validate in
`internal/envfile/schema.go`:

| Key | Purpose | Default |
|---|---|---|
| `VMSCA_ENABLE` | turn the certsrv front-end on/off | `false` |
| `VMSCA_FQDN` | enrollment hostname (TLS SAN + VCF target) | `certsrv.sddc.lab` |
| `VMSCA_PORT` | listener port | `8443` |
| `VMSCA_USERNAME` | Basic Auth user VCF is configured with | (required when enabled) |
| `VMSCA_PASSWORD` | Basic Auth password | (required when enabled) |
| `VMSCA_TEMPLATE` | accepted CertificateTemplate name | `VMware` |

### Files
- **Reuse:** `internal/deploy/sign.go` (`SignCSR`), `internal/deploy/stepca.go`
  (`IssueCert`, `requireCAReady`, loopback-pinned client), `internal/certs/certs.go`
  (pattern for reading CA data from `CA_DATA_DIR`).
- **Extend:** `internal/server/controlplane.go` / `cmd/control-plane/main.go` (second
  listener wiring), `config/labprovider.env.example`, `internal/envfile/schema.go`,
  `README.md`.
- **New:** `internal/msca/certsrv.go` (+ `certsrv_test.go`).
- **Prior art:** `step-ca_api_design.md` (historical wrapper-service reasoning - Go vs
  Python, inventory, issuance path - much of it applies).

---

## 6. VCF-side configuration (operator steps)

In SDDC Manager -> Certificate Authority -> Edit:
- CA Type: **Microsoft**.
- Web Enrollment URL: `https://{VMSCA_FQDN}:{VMSCA_PORT}/certsrv`.
- Username / Password: `VMSCA_USERNAME` / `VMSCA_PASSWORD`.
- Template Name: value of `VMSCA_TEMPLATE` (default `VMware`).

The step-ca root must be trusted by SDDC Manager (already the lab's root); VCF also pulls
the chain via `certnew.p7b` during enrollment.

---

## 7. Risks / open items

- **Cannot byte-verify against a live VCF here.** SDDC Manager's exact request shape and
  which response substrings it keys on are not publicly pinned; the `magnuswatn/certsrv`
  contract is the best-documented proxy and matches the ADCS site behavior. A real-VCF
  enrollment run is the acceptance gate, and minor HTML/response-string tuning may be
  needed after the first live test.
- **Cert profile / EKU.** VCF expects server-auth (usually serverAuth + clientAuth) and a
  validity window matching its template expectation. `SignCSR` uses provisioner `admin`
  with `SERVICE_CERT_DURATION` (8760h). Verify the issued leaf carries the EKUs VCF wants
  and that 1y validity is acceptable (MS "Web Server" base is typically 1-2y); adjust the
  provisioner profile / duration if not.
- **SAN / subject passthrough.** `step ca sign` preserves CSR SANs subject to provisioner
  policy. Confirm step-ca's policy permits every SDDC component FQDN VCF submits (vCenter,
  NSX, SDDC Manager, etc.), or issuance will be rejected.
- **Basic Auth over TLS only.** No NTLM. Fine for VCF, but the endpoint must never be
  served plaintext - `VMSCA_ENABLE` implies a valid step-ca leaf is present first.
- **In-memory ReqID store** is lost on restart (section 5). Acceptable; document the
  persist-under-WORKDIR option if a longer-lived request record is ever wanted.

---

## 8. Validation

**Unit (`internal/msca/certsrv_test.go`)** - no live CA needed for most of it:
- `/certsrv/` returns 401 without creds and 200 with them.
- `certfnsh.asp` response body matches `certnew.cer\?ReqID=(\d+)&`.
- `certcarc.asp` response body matches `var nRenewals=(\d+);`.
- `certnew.cer` sets `Content-Type: application/pkix-cert` and returns exactly one cert;
  `certnew.p7b` sets `application/x-pkcs7-certificates` and parses as a PKCS#7 carrying
  intermediate + root. Stub `SignCSR` with a fixture cert so these run without step-ca.
- Template mismatch (`CertAttrib` != `VMSCA_TEMPLATE`) is rejected.

**End-to-end** - against a running lab:
1. Deploy step-ca and enable `VMSCA_*`; confirm the listener serves its step-ca leaf on
   `VMSCA_PORT` and `/certsrv/` prompts for Basic Auth.
2. Reproduce the client flow with `curl` (POST a test CSR to `certfnsh.asp`, extract the
   `ReqID`, GET `certnew.cer`, GET `certnew.p7b`) and verify the leaf validates against the
   step-ca root using the returned chain.
3. Point SDDC Manager's Certificate Authority config at the URL above and run a
   certificate replacement for a component; confirm issuance succeeds and the new cert
   chains to the step-ca root on the target.
