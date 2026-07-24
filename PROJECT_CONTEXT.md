# labprovider — Project Context

This document complements `AGENTS.md`, which defines implementation rules and constraints.

## Interaction Guidance (for AI assistants)

- Do not introduce abstraction layers unless explicitly requested
- Do not add migration logic unless explicitly requested
- Prefer explicit, service-scoped changes over generic frameworks
- Preserve the single-node, deterministic model

## Overview

labprovider is a small, opinionated infrastructure platform for lab and proof-of-concept environments.

It provides a single-node infrastructure services layer supporting VMware Cloud Foundation (VCF) and similar platforms, deployed and observed through a web control plane.

The focus is on:
- simplicity
- reproducibility
- clarity
- minimal dependencies

---

## Core Philosophy

labprovider is intentionally constrained:

- Single node only
- No orchestration (no Kubernetes)
- Explicit configuration (hidden magic is OK if its documented)
- Reproducible from scratch
- Minimal moving parts

This is an infrastructure services layer, not a full platform.

---

## Runtime Model (v2)

Everything is containerized:

| Component | Implementation |
|-----------|----------------|
| All services | Docker Compose stacks |
| Control plane | Go binary in a container (root, host network, docker socket + `/opt/labprovider` + `/host/etc` mounted) |
| Host footprint | Docker plus two one-time systemd tweaks done by `install.sh` |

No clustering or orchestration. There is no CLI path: the legacy bash bootstrap was the v1 approach and has been removed. chrony and rsyslog run containerized (images built locally, since neither has an official upstream container).

---

## Service Model

- `install.sh` starts the control plane; services are deployed and removed from the web UI (or its HTTP API)
- The deploy engine is a static registry with explicit dependencies, executed sequentially in dependency order, single-flight
- Removal stops containers and deletes runtime files; persistent data is always preserved

---

## Core Components

### Infrastructure

- Technitium (DNS; NetBox-driven via dns-sync)
- Chrony (NTP, containerized, SYS_TIME only)
- rsyslog (logging, containerized)

### Security & Identity

- step-ca (internal CA, dedicated PostgreSQL backend, CRL enabled)
- CSR signing and a Microsoft-CA web-enrollment emulator (`certsrv`) in the control plane, so VCF can automate certificate replacement against step-ca via its "Microsoft CA" integration
- Keycloak (OIDC identity provider; opinionated VCF realm import)
- Authentik (OIDC + outbound SCIM 2.0 for VCF 9 identity federation; runs in parallel with the other IdPs, federate against one)
- Zitadel (OIDC identity provider, optional multi-tenant orgs; v4 core + Login V2 + PostgreSQL + nginx terminator)

### Source of Truth

- NetBox (IPAM / DCIM); dns-sync reconciles it into Technitium continuously

### Storage & Transfer

- SeaweedFS (S3-compatible)
- SFTPGo (SFTP + admin UI)

### VCF Integration

- VCF Offline Depot (nginx, basic-auth protected paths)

### Control Plane

- `services/control-plane`: config wizard, deploy engine with SSE progress, read-only dashboard (certificates, DNS, IPAM, containers, recent errors), CSR signing, and the optional MSCA emulator
- No UI authentication in v1: trusted lab networks only

---

## Configuration Model

Single source of truth: `labprovider.env`, managed by the wizard at `/opt/labprovider/control-plane/labprovider.env`.

Principles:
- explicit values, no implicit defaults
- strict per-service validation from one schema table
- the shipped example is the completeness reference; deploys refuse an outdated config
- external DNS records in the managed `dns.seed` (optional)

## Container Image Model

All images are pinned centrally in `labprovider.env` (`*_IMAGE` variables). Locally built images (`labprovider/control-plane`, `labprovider/chrony`, `labprovider/rsyslog`, `labprovider/dns-sync`) build from the checkout or from sources baked into the control-plane image; no registry access is needed at deploy time.

---

## Certificate Model

- step-ca is the internal CA; all HTTPS services use step-ca certificates
- issuance goes through the shared `IssueCert` helper: identity-based reuse, full-chain guarantee, uid-1000 ownership
- every consumer pins `CA_FQDN` to `127.0.0.1`, so certificates can be issued before DNS exists (single-node assumption)
- the control plane issues its own leaf after the CA deploys and serves HTTPS after a restart

---

## Template Model

- Go text/template, embedded in the control-plane binary
- `missingkey=error`: a reference to an unset variable fails the render
- nginx runtime variables (`$uri`, `$host`) need no escaping
- golden render tests pin every template's output

---

## Directory Model

- Persistent service data: `/opt/labprovider/<service>`
- Runtime-generated files: `${WORKDIR}` (default `/opt/labprovider/runtime/<service>`)
- Control plane state: `/opt/labprovider/control-plane/` (managed config, dns.seed, state.json, certs, secrets)

---

## Removal Semantics

Remove (per service, from the UI):
- stops containers
- removes runtime files
- preserves persistent data, certificates, and operator secrets

---

## Security Model (Lab)

- basic auth for depot; per-service admin credentials from the env file
- scoped read-only tokens for the dashboard panels, auto-provisioned by the producing deployers; operator-placed (SOPS/age) tokens win
- pragmatic permissions; usability over production-grade hardening
- the control plane UI itself has no auth (v1) - trusted lab network only

---

## Operational Constraints

- Deploys are sequential and dependency-ordered; one deploy runs at a time
- Readiness checks probe externally exposed endpoints (a started container does not imply readiness)
- Docker is the source of truth for running state; state.json is advisory history
- The control plane never redeploys itself; upgrade by re-running `install.sh`

---

## Intended Use

- VCF labs
- PoCs
- isolated environments
- reproducible demos

---

## Non-Goals

- no HA
- no clustering
- no Kubernetes
- no advanced automation frameworks

---

## Development Principles

Changes should:

- be minimal
- follow existing patterns
- be explicit and readable
- avoid unnecessary abstraction
- avoid implicit behavior
- prefer external readiness checks over internal health probes

---

## Summary

labprovider is:

A minimal, reproducible, single-node infrastructure platform for lab environments — fully containerized, driven by a web control plane, prioritizing clarity, simplicity, and controlled dependencies.
