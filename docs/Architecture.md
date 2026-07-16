# Architecture — Homelab Ops App (0.x)

Synthesized from the resolved decisions in `Decisions.md`. This is the 0.x shape only — anything multi-node or Operations-Center-dependent is explicitly deferred (see "Out of scope for 0.x").

## 0.x framing

0.x does **not** depend on or wrap Operations Center. That was originally considered (§0 of Decisions) but dropped because:
- Operations Center expects a trusted client cert in its own seed before it'll talk to anyone — for node #0 there's nothing yet to provide that, so it doesn't remove the bootstrap problem, it just relocates it.
- Multi-node clustering (Operations Center's main value-add) is explicitly out of scope for 0.x anyway.

So for 0.x, this app talks to IncusOS nodes directly: it builds install seeds itself, drives `flasher-tool` itself, and issues certs trusted directly by Incus on the node — no intermediary management plane. Wrapping Operations Center is revisited once multi-node is actually on the table.

## Components

### 1. Bootstrap CLI (node #0 only)

A standalone command, not part of the always-on web app — it has to work before the app exists anywhere.

- Generates a self-signed cert/key pair offline (no network dependency)
- Reads one `Instance`/`Network` definition (see Data model) and renders an IncusOS seed bundle: `install.yaml` (disk target, TPM/Secure Boot flags), `network.yaml` (static IP or DHCP, plus a default route — required once an instance has a static IP, or the node can't reach anything off-subnet), `applications.yaml` (`incus` only — no `operations-center` for 0.x), `incus.yaml` (preseeds the generated client cert as a trusted Incus client cert, so Incus trusts it on first boot — see #5)
- Invokes `flasher-tool` to bake the seed into a `.img`
- Leaves the cert/key on local disk. This cert is the deployment's single break-glass client credential, not a one-off for node #0 alone — the web app is later pointed at the same cert's public half via its own deployment config (see "Cert sourcing" below), so it embeds it into every subsequently-provisioned node too. The private key never leaves the operator's local disk and the web app never reads it.

Once node #0 is up and reachable, this tool's job is done; steady-state provisioning moves to the web app.

### 2. Web app (Go)

No k8s dependency (per §9, decided in #18): dev runs it via Docker Compose; deployment targets are a Docker image and a plain binary. Migrating it to run inside the IncusOS-managed fleet itself is a later, explicit migration step, not a 0.x concern.

Modules:

- **Config sync** — pulls one git repo (public, per environment; any `go-git`-supported transport, not GitHub-API-specific) on a poll/manual trigger, parses the k8s-style multi-doc YAML, diffs against last-known state, and **warns only** — no auto-apply, no rollback logic in 0.x.
- **Instance/network store** — holds the parsed `Instance` and `Network` objects, queryable by kind/name. Backed by sqlite (`modernc.org/sqlite`, pure Go, no cgo — keeps the single-binary/distroless deployment goal from #18), file-backed by default so IPAM-assigned IPs survive a restart (see `Decisions.md` §12) — `:memory:` remains available for tests. Each sync fully replaces the prior snapshot rather than merging into it, matching config sync's full-`Config`-per-sync output (see #21); git stays authoritative for desired-state config, the store is the system of record for what's actually been assigned.
- **IPAM** — tracks `Network` definitions (CIDR + DHCP exclusion ranges), assigns static IPv4s to instances, basic duplicate detection. App is the sole source of truth; assignments persist in the durable store, no DHCP/DNS write-back in 0.x.
- **Seed/installer generation** — same seed-rendering logic as the bootstrap CLI, generalized to any instance: builds `install.yaml`/`network.yaml`/`applications.yaml`/`incus.yaml`, calls `flasher-tool`, and either serves the resulting `.img` for download or flashes it directly — whichever is easiest to implement first.
- **WireGuard tunnel** (`internal/wireguard`, resolves `Decisions.md` §11 — see #91) — the app terminates a persistent WireGuard tunnel to every managed node entirely in-process, over a userspace network stack (`golang.zx2c4.com/wireguard` + a gVisor netstack — no host TUN device, no `NET_ADMIN`, keeping the deployment distroless). The app's own identity is generated once and persisted in the durable store; each instance gets a keypair minted at seed-render time and embedded into its `network.yaml`, with no live enrollment step. `SyncOnce` assigns each instance a stable address on a fixed overlay CIDR and reconciles the live tunnel's trusted-peer table on every sync. `WIREGUARD_ENDPOINT` (operator-supplied, the app's externally-reachable `host:port`) gates this — see the HTTP API table below for what depends on it. `internal/nodeprovision` builds on the tunnel to let the app authenticate directly to a node's Incus API — mint a one-time bootstrap cert (the app never holds the break-glass cert's private key, see §4 below), dial over the tunnel, and revoke the cert after use — the mechanism #92's local app-manager agent deployment will use for real.
- **Cert sourcing** — the app never generates, mints, or stores a client cert itself. It reads one operator-supplied, self-signed cert (the same artifact the bootstrap CLI's `gen-cert` already produces for node #0) from local deployment config, and preseeds its public half into every subsequently-provisioned node's `incus.yaml`, so the whole fleet trusts a single break-glass credential identical to what node #0 trusts (not Operations Center auth — that question is shelved with the OC-wrapping decision). The private key is never read, transmitted, or persisted by the app — rotation/revocation remain deferred, but the key-storage risk that deferral originally implied is avoided entirely rather than just postponed.
- **Tailscale config** — takes an operator-supplied authkey and writes it into the node's seed for IncusOS's built-in [Tailscale service](https://linuxcontainers.org/incus-os/docs/main/reference/services/tailscale/). No per-node enrollment flow yet.
- **Observability config** — provisions an Alloy instance under Incus and points the node's remote syslog at it; Alloy forwards logs to one or more operator-configured Loki endpoints. The same Alloy instance also scrapes Incus's native `/1.0/metrics` endpoint (host + per-instance resource stats) and remote_writes it to one or more operator-configured Prometheus `remote_write` endpoints — one agent per node handles both logs and metrics rather than running a second exporter. The metrics scrape authenticates with a dedicated `metrics`-typed Incus cert (read-only, scoped to `/1.0/metrics` only), minted per-instance at seed-render time alongside the rest of that instance's seed — distinct from, and lower-privilege than, the break-glass client cert (see `Decisions.md` § Metrics). Grafana Cloud is the expected production destination, but destinations are just config, not hardcoded, and Alloy fans out to however many are configured — so a deployment can ship to Grafana Cloud, mirror to a local Grafana + Loki + Prometheus stack, or both at once (see `Decisions.md` § Metrics, "Destination"). Dev/test always includes the local stack (under `docker-compose.yml`) as at least one destination, needing no cloud credentials. Tailscale-reachable Grafana endpoint is TBD later.

### 3. Managed node(s)

IncusOS host(s) running: Incus (always), Tailscale service (authkey-enrolled), and — once observability is wired up — an Alloy Incus instance receiving the node's syslog and scraping its local Incus API for metrics.

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
disk: single          # 0.x default; multi-disk override deferred
nic: single            # 0.x default; multi-nic override deferred
security:
  tpm: false           # configurable per §6
  secure_boot: true     # configurable per §6
applications: [incus]
```

This is illustrative, not a finalized schema — exact field names are an implementation step, not an open question that needs more discussion first.

`static_ip` may be omitted; `internal/ipam` then auto-assigns the next free IPv4 from `dhcp_excluded_range` during sync, stably reusing the same instance's prior address across re-syncs (see Decisions §5).

## Key flows

**Flow A — Node #0 bootstrap (manual, offline):**
Bootstrap CLI generates cert → renders seed from one `Instance`/`Network` doc → `flasher-tool` produces `.img` → operator flashes USB → node boots, installs IncusOS, Incus comes up trusting the generated cert.

**Flow B — Steady-state provisioning (web app, post node #0):**
GitHub push → config sync pulls + parses → diff shown (warn only) → operator/app triggers seed generation for a given instance → `.img` produced/downloaded → repeat Flow A's flash/install for that node, now trusting the same break-glass cert node #0 trusts.

**Flow C — Observability (logs + metrics):**
IncusOS node syslog → Alloy Incus instance → Grafana Cloud (logs). The same Alloy instance also scrapes Incus's local `/1.0/metrics` endpoint → Grafana Cloud (metrics), authenticated via a per-instance `metrics`-typed cert minted at seed-render time.

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
| `/instances/{name}/seed` | POST | Renders the four IncusOS seed documents (`install.yaml`, `network.yaml`, `applications.yaml`, `incus.yaml`) for a synced instance, embedding the break-glass cert from `CLIENT_CERT_PATH` and, since #91, WireGuard tunnel config + a one-time bootstrap cert (see the WireGuard tunnel module above). Requires `WIREGUARD_ENDPOINT` to be set; the route reports 503 if it isn't |
| `/instances/{name}/image` | GET | Regenerates and streams a bootable `.img` for a synced instance, reflecting its current seed/cert/IP, generated fresh on each request (no caching). Requires `BASE_IMAGE_PATH` (an operator-supplied base IncusOS raw image), a `flasher-tool` matching that image's IncusOS release (the Docker image bakes one in), and — since #91, same as the seed route above — `WIREGUARD_ENDPOINT`; otherwise the route reports 503 |

Full diff detail (human-readable added/changed/removed lines) is server-log-only, not part of the JSON response — keeps the API from committing to a long-form string contract before a UI exists to consume it. No OpenAPI spec yet (see `Out of Scope.md`).

`GET /instances/{name}/image` regenerates on demand: each request copies the
multi-GB base image into a temp dir (`os.TempDir()`, i.e. `/tmp` in the
distroless image) and injects the seed, then streams and deletes it. That copy
is disk-backed, not RAM (the distroless image ships `/tmp` mode 1777 on its
writable overlay layer, no `tmpfs`), so a large image won't OOM the container —
but the deployment must give the container enough writable disk for at least
one full image copy (more if concurrent downloads are expected), or mount a
suitably-sized volume/`tmpfs` at `/tmp`.

Networking between IncusOS nodes and the web app itself (avoiding exposing nodes to the public internet) is resolved as of #91 — see § Web app's WireGuard tunnel module above and `Decisions.md` § Networking for the three options originally weighed and why WireGuard won.

## Incus Node networking to Web App

Resolved by #91 (`Decisions.md` §11): the web app and every managed node connect via a persistent WireGuard tunnel, seeded at install time with no live enrollment step — see § Web app's WireGuard tunnel module above for the mechanism, and `Decisions.md` §11 for the three options originally considered (a node-side polling management container, Tailscale on the web app, WireGuard) and why WireGuard won. `Decisions.md` §11 also sketches a not-yet-built follow-on direction: once the tunnel is proven, the web app could act as a transparent network relay between an operator and a node's Incus API, rather than just carrying the app's own traffic.