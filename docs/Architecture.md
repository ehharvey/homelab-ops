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
- Leaves the cert/key on local disk — this becomes the credential the web app adopts later to talk to this node's Incus API

Once node #0 is up and reachable, this tool's job is done; steady-state provisioning moves to the web app.

### 2. Web app (Go)

Runs on the local dev k8s cluster for now (per §9); migrating it to run inside the IncusOS-managed fleet itself is a later, explicit migration step, not a v1 concern.

Modules:

- **Config sync** — pulls one GitHub repo (public, per environment) on a poll/manual trigger, parses the k8s-style multi-doc YAML, diffs against last-known state, and **warns only** — no auto-apply, no rollback logic in v1.
- **Instance/network store** — holds the parsed `Instance` and `Network` objects (in-memory or a simple local DB; not yet specified — flag for the roadmap).
- **IPAM** — tracks `Network` definitions (CIDR + DHCP exclusion ranges), assigns static IPv4s to instances, basic duplicate detection. App is the sole source of truth; no DHCP/DNS write-back in v1.
- **Seed/installer generation** — same seed-rendering logic as the bootstrap CLI, generalized to any instance: builds `install.yaml`/`network.yaml`/`applications.yaml`/`incus.yaml`, calls `flasher-tool`, and either serves the resulting `.img` for download or flashes it directly — whichever is easiest to implement first.
- **Cert issuance** — self-signed cert per node, for direct Incus client authentication (not Operations Center auth — that question is shelved with the OC-wrapping decision), preseeded into the node's `incus.yaml` so Incus trusts it before first boot. Storage/rotation/revocation explicitly deferred.
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

## Key flows

**Flow A — Node #0 bootstrap (manual, offline):**
Bootstrap CLI generates cert → renders seed from one `Instance`/`Network` doc → `flasher-tool` produces `.img` → operator flashes USB → node boots, installs IncusOS, Incus comes up trusting the generated cert.

**Flow B — Steady-state provisioning (web app, post node #0):**
GitHub push → config sync pulls + parses → diff shown (warn only) → operator/app triggers seed generation for a given instance → `.img` produced/downloaded → repeat Flow A's flash/install for that node, now trusting a web-app-issued cert.

**Flow C — Logging:**
IncusOS node syslog → Alloy Incus instance → Grafana Cloud.

**Flow D — Remote access:**
Operator supplies a Tailscale authkey per node → baked into seed → node joins tailnet via IncusOS's Tailscale service.

## Networking
We want to avoid putting Incus nodes on the public internet, so we need to figure out a way to nodes to interact with the Web app.

This is all out-of-scope for Phase 1.

### Nodes connect to Web app.
Proposition: Incus nodes run a management container. This container polls the Web App for updates.

To make this more efficient, nodes could connect and then upgrade to a websocket connection for subscriptions.

### Tailscale on Web App
An alternative is if the WebApp also has Tailscale in order to connect to nodes.

### WireGuard on both Web App and Nodes
A 3rd alternative is if the Web app and Nodes connect via Wireguard.