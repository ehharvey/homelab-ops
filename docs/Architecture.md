# Architecture — Homelab Ops App (v1)

Synthesized from the resolved decisions in `Open Questions.md`. This is the v1 shape only — anything multi-node or Operations-Center-dependent is explicitly deferred (see "Out of scope for v1").

## v1 framing

v1 does **not** depend on or wrap Operations Center. That was originally considered (§0 of Open Questions) but dropped because:
- Operations Center expects a trusted client cert in its own seed before it'll talk to anyone — for node #0 there's nothing yet to provide that, so it doesn't remove the bootstrap problem, it just relocates it.
- Multi-node clustering (Operations Center's main value-add) is explicitly out of scope for v1 anyway.

So for v1, this app talks to IncusOS nodes directly: it builds install seeds itself, drives `flasher-tool` itself, and issues certs trusted directly by Incus on the node — no intermediary management plane. Wrapping Operations Center is revisited once multi-node is actually on the table.

## Components

### 1. Bootstrap CLI (node #0 only)

A standalone command, not part of the always-on web app — it has to work before the app exists anywhere.

- Generates a self-signed cert/key pair offline (no network dependency)
- Reads one `Instance`/`Network` definition (see Data model) and renders an IncusOS seed bundle: `install.yaml` (disk target, TPM/Secure Boot flags), `network.yaml` (static IP or DHCP, plus a default route — required once an instance has a static IP, or the node can't reach anything off-subnet), `applications.yaml` (`incus` only — no `operations-center` for v1), `incus.yaml` (preseeds the generated client cert as a trusted Incus client cert, so Incus trusts it on first boot — see #5)
- Invokes `flasher-tool` to bake the seed into a `.img`
- Leaves the cert/key on local disk. This cert is the deployment's single break-glass client credential, not a one-off for node #0 alone — the web app is later pointed at the same cert's public half via its own deployment config (see "Cert sourcing" below), so it embeds it into every subsequently-provisioned node too. The private key never leaves the operator's local disk and the web app never reads it.

Once node #0 is up and reachable, this tool's job is done; steady-state provisioning moves to the web app.

### 2. Web app (Go)

No k8s dependency (per §9, decided in #18): dev runs it via Docker Compose; deployment targets are a Docker image and a plain binary. Migrating it to run inside the IncusOS-managed fleet itself is a later, explicit migration step, not a v1 concern.

Modules:

- **Config sync** — pulls one git repo (public, per environment; any `go-git`-supported transport, not GitHub-API-specific) on a poll/manual trigger, parses the k8s-style multi-doc YAML, diffs against last-known state, and **warns only** — no auto-apply, no rollback logic in v1.
- **Instance/network store** — holds the parsed `Instance` and `Network` objects, queryable by kind/name. Backed by sqlite (`modernc.org/sqlite`, pure Go, no cgo — keeps the single-binary/distroless deployment goal from #18), file-backed by default so IPAM-assigned IPs survive a restart (see `Open Questions.md` §12) — `:memory:` remains available for tests. Each sync fully replaces the prior snapshot rather than merging into it, matching config sync's full-`Config`-per-sync output (see #21); git stays authoritative for desired-state config, the store is the system of record for what's actually been assigned.
- **IPAM** — tracks `Network` definitions (CIDR + DHCP exclusion ranges), assigns static IPv4s to instances, basic duplicate detection. App is the sole source of truth; assignments persist in the durable store, no DHCP/DNS write-back in v1.
- **Seed/installer generation** — same seed-rendering logic as the bootstrap CLI, generalized to any instance: builds `install.yaml`/`network.yaml`/`applications.yaml`/`incus.yaml`, calls `flasher-tool`, and either serves the resulting `.img` for download or flashes it directly — whichever is easiest to implement first.
- **Cert sourcing** — the app never generates, mints, or stores a client cert itself. It reads one operator-supplied, self-signed cert (the same artifact the bootstrap CLI's `gen-cert` already produces for node #0) from local deployment config, and preseeds its public half into every subsequently-provisioned node's `incus.yaml`, so the whole fleet trusts a single break-glass credential identical to what node #0 trusts (not Operations Center auth — that question is shelved with the OC-wrapping decision). The private key is never read, transmitted, or persisted by the app — rotation/revocation remain deferred, but the key-storage risk that deferral originally implied is avoided entirely rather than just postponed.
- **Tailscale config** — takes an operator-supplied authkey and writes it into the node's seed for IncusOS's built-in [Tailscale service](https://linuxcontainers.org/incus-os/docs/main/reference/services/tailscale/). No per-node enrollment flow yet.
- **Logging config** — provisions an Alloy instance under Incus and points the node's remote syslog at it; Alloy forwards to Grafana Cloud. Tailscale-reachable Grafana endpoint is TBD later.

### 3. Managed node(s)

IncusOS host(s) running: Incus (always), Tailscale service (authkey-enrolled), and — once logging is wired up — an Alloy Incus instance receiving the node's syslog.

## Data model (sketch)

K8s-style: multiple YAML documents in one or more files, each discriminated by `kind:`, merged into one fleet definition.

```yaml
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1                                # required once any instance has a static_ip
dhcp_excluded_range: 192.168.1.200-192.168.1.250   # static IPs come from here
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: aa:bb:cc:dd:ee:ff
network: home-lan
static_ip: 192.168.1.201
disk: single          # v1 default; multi-disk override deferred
nic: single            # v1 default; multi-nic override deferred
security:
  tpm: false           # configurable per §6
  secure_boot: true     # configurable per §6
applications: [incus]
```

This is illustrative, not a finalized schema — exact field names are an implementation step, not an open question that needs more discussion first.

`static_ip` may be omitted; `internal/ipam` then auto-assigns the next free IPv4 from `dhcp_excluded_range` during sync, stably reusing the same instance's prior address across re-syncs (see Open Questions §5).

## Key flows

**Flow A — Node #0 bootstrap (manual, offline):**
Bootstrap CLI generates cert → renders seed from one `Instance`/`Network` doc → `flasher-tool` produces `.img` → operator flashes USB → node boots, installs IncusOS, Incus comes up trusting the generated cert.

**Flow B — Steady-state provisioning (web app, post node #0):**
GitHub push → config sync pulls + parses → diff shown (warn only) → operator/app triggers seed generation for a given instance → `.img` produced/downloaded → repeat Flow A's flash/install for that node, now trusting the same break-glass cert node #0 trusts.

**Flow C — Logging:**
IncusOS node syslog → Alloy Incus instance → Grafana Cloud.

**Flow D — Remote access:**
Operator supplies a Tailscale authkey per node → baked into seed → node joins tailnet via IncusOS's Tailscale service.

## HTTP API

The web app (`internal/server`) currently exposes:

| Route | Method | Purpose |
| --- | --- | --- |
| `/healthz` | GET | Liveness check |
| `/sync` | POST | Trigger a config-sync run; returns the synced commit, network/instance counts, and diff counts against the prior snapshot |
| `/status` | GET | Last-synced commit SHA and sync time, if any |
| `/networks` | GET | All stored `Network` objects |
| `/instances` | GET | All stored `Instance` objects |
| `/instances/{name}/seed` | POST | Renders the four IncusOS seed documents (`install.yaml`, `network.yaml`, `applications.yaml`, `incus.yaml`) for a synced instance, embedding the break-glass cert from `CLIENT_CERT_PATH` |

Full diff detail (human-readable added/changed/removed lines) is server-log-only, not part of the JSON response — keeps the API from committing to a long-form string contract before a UI exists to consume it. No OpenAPI spec yet (see `Out of Scope.md`).

Networking between IncusOS nodes and the web app itself (avoiding exposing nodes to the public internet) is unresolved — see `Open Questions.md` § Networking. Out of scope for Phase 1.

## Incus Node networking to Web App
We want to avoid putting Incus nodes on the public internet, so we need to figure out a way for nodes to interact with the Web app.

This is all out-of-scope for Phase 1. We need to visit after v1.

### Nodes connect to Web app.
Proposition: Incus nodes run a management container. This container polls the Web App for updates.

To make this more efficient, nodes could connect and then upgrade to a websocket connection for subscriptions.

### Tailscale on Web App
An alternative is if the WebApp also has Tailscale in order to connect to nodes.

### WireGuard on both Web App and Nodes
A 3rd alternative is if the Web app and Nodes connect via Wireguard.