# Architecture — Homelab Ops App (0.x)

> **Pivot (2026-11-02, `Decisions.md` §31):** the main line is now **bare-metal Kubernetes on Talos**, and the project becomes runtime-agnostic: a host-OS seam (`internal/hostos`: IncusOS, Talos, later Debian and an own mkosi distro) and a cluster-runtime seam (`internal/clusterruntime`: Incus, Kubernetes), Roadmap Phase 4. The web app appliance comes next (Phase 5): a dedicated laptop on the nodes' LAN that bootstraps the cluster, serves DHCP (Kea) and DNS from IPAM until the cluster takes them over, holds the Secure Boot signing key, and is the break-glass source after a full power cut. Then one Talos node (Phase 6), a 3-node cluster (Phase 7), the web app in the cluster as Talos static pods (Phase 8), and workloads through Flux (Phase 9). This document still describes what's **built today**: the IncusOS path, which is kept building as one backend but parked after Phase 3.

Synthesized from the resolved decisions in `Decisions.md`. This is the 0.x shape only — anything Operations-Center-dependent is explicitly deferred (see "Out of scope for 0.x"). Growing past one Incus cluster member is *not* out of scope for 0.x — it was Roadmap Phase 5 (`Decisions.md` §26), same-OS clusters joined by the agent (`Decisions.md` §30), and is parked since the pivot below — just not yet built. 0.x's App Manager leader-election design (`Decisions.md` § App Manager HA) needs Incus running as a cluster rather than a bare daemon, even at one member, so that a later migration to real multi-member clustering needs no rework; Phase 3 (Roadmap, `Decisions.md` §26 Tier A) is what actually sets that one-member cluster up — "single-node" below refers to the fleet 0.x provisions today, not to Incus running unclustered.

## 0.x framing

0.x does **not** depend on or wrap Operations Center. That was originally considered (§0 of Decisions) but dropped because:
- Operations Center expects a trusted client cert in its own seed before it'll talk to anyone — for node #0 there's nothing yet to provide that, so it doesn't remove the bootstrap problem, it just relocates it.
- Multi-*member* clustering (Operations Center's main value-add) wasn't what 0.x's Incus clustering was originally *for* — the cluster existed for the App Manager's leader-election design (`docs/AppManager.md`, `Decisions.md` § App Manager HA). That changed on 2026-09-28: a real 3-member cluster (then Roadmap Phase 5, parked since §31) became the project's main line, with the app-manager agent driving member joins (`Decisions.md` §27). Since 2026-10-26 the goal is host support and management (`Decisions.md` §30): the agent manages only itself and its cluster (deploy, one per host, self-upgrade, joins), more host OSes were to follow in Phase 6, and App management was deferred to Phase 8. Since 2026-11-02 Talos is the main line instead, and that Incus work is parked (`Decisions.md` §31). This reason for skipping Operations Center is therefore weaker than it was, and is revisited in §27.

So for 0.x, this app talks to IncusOS nodes directly: it builds install seeds itself, drives `flasher-tool` itself, and issues certs trusted directly by Incus on the node — no intermediary management plane. Real multi-member Incus clustering (formerly Phase 5) came on the table and is now parked (`Decisions.md` §31); `Decisions.md` §27 revisits Operations Center: §0's bootstrap objection still stands, but how it joins members is worth reading before building the agent-driven join.

## Components

### 1. Bootstrap CLI (node #0 only)

A standalone command, not part of the always-on web app — it has to work before the app exists anywhere.

- Generates a self-signed cert/key pair offline (no network dependency)
- Reads one `Instance`/`Network` definition (see Data model) and renders an IncusOS seed bundle: `install.yaml` (disk target, TPM/Secure Boot flags), `network.yaml` (static IP or DHCP, plus a default route — required once an instance has a static IP, or the node can't reach anything off-subnet), `applications.yaml` (`incus` only — no `operations-center` for 0.x), `incus.yaml` (preseeds the generated client cert as a trusted Incus client cert, so Incus trusts it on first boot — see #5)
- Invokes `flasher-tool` to bake the seed into a `.img`
- Leaves the cert/key on local disk. This cert is the deployment's single break-glass client credential, not a one-off for node #0 alone — the web app is later pointed at the same cert's public half via its own deployment config (see "Cert sourcing" below), so it embeds it into every subsequently-provisioned node too. The private key never leaves the operator's local disk and the web app never reads it.
- Owns the project's Secure Boot keys (`internal/secureboot`, `Decisions.md` §31), for the Talos and own-distro nodes that boot UKIs the project signs. `bootstrap gen-secureboot-keys` generates an RSA-2048 PK, a KEK issued by PK and a db key issued by KEK, offline. It writes the certs, the db key, an EFI signature list (`.esl`) per variable and the signed enrollment payloads (`PK.auth`, `KEK.auth`, `db.auth` and, with Microsoft's CA, `dbx.auth`, which systemd-boot auto-enrolls from `/loader/keys/<name>/` on install media while the machine is in Setup Mode) to `--output-dir` (default `bootstrap-output/secureboot/`). The **PK and KEK private keys go to a separate `--offline-dir`** (default `bootstrap-output/secureboot-offline/`), holding nothing else, so the operator can move that directory to offline storage whole. db includes Microsoft's third-party UEFI CA certs (2011 and 2023, vendored in `internal/third_party/msuefica`) by default, because GPU, NIC and storage option ROMs are signed under them. With them comes dbx: Microsoft's x64 list of revoked binaries (vendored in `internal/third_party/msdbx`), re-signed by KEK, because that CA has signed shims and boot loaders that were later revoked and clearing a machine's factory keys can empty its dbx. `--no-microsoft-ca` leaves out both. Microsoft's KEK and Windows PCA are never enrolled. Both commands write each file to a temporary file and rename it into place only once every write has succeeded, so a failed run leaves the previous files as they were. `bootstrap sign-efi <image>` Authenticode-signs a PE/EFI binary (a UKI) with the db key (`--key`/`--cert`, defaulting to `bootstrap-output/secureboot/db.{key,crt}`).

- Is also the operator's client for the web app's API, once the web app runs: `bootstrap api <verb>` joins the web app's WireGuard tunnel as an operator peer and calls one route per verb (see § Who can reach the API, "The operator CLI").

Once node #0 is up and reachable, this tool's bootstrap job is done; steady-state provisioning moves to the web app.

### 2. Web app (Go)

No k8s dependency (per §9, decided in #18): dev runs it via Docker Compose; deployment targets are a Docker image and a plain binary. The deployment artifact is the published OCI image `ghcr.io/ehharvey/homelab-ops/web`, which CI builds on every PR and pushes from `main` only, tagged with the commit SHA plus `latest`, after proving the image serves `/healthz` and runs its bundled `flasher-tool` (`docker-smoke` in `.github/workflows/ci.yml`, #240). In production it runs on its own appliance machine, outside the cluster: an internet-reachable VM or a spare machine on the nodes' LAN (`Decisions.md` §29). The first form is a stock Debian cloud image set up by cloud-init to run the published image under Docker, with unattended OS upgrades. An IncusOS turnkey host is parked, because an IncusOS seed can't create the web app's instance. An mkosi-built image is undecided. Both run the same published image from the same deployment config directory, and each deployment setting (API listener, where install media is built, how node0 is provisioned, and the rest) is an operator choice with a safe default. Moving it into the IncusOS-managed fleet itself is a later migration, the same path as moving between appliance hosts (§29).

Modules:

- **Config sync** — pulls one git repo (public, per environment; any `go-git`-supported transport, not GitHub-API-specific) on a poll/manual trigger, parses the k8s-style multi-doc YAML, diffs against last-known state, and **warns only** — no auto-apply, no rollback logic in 0.x.
- **Instance/network store** — holds the parsed `Instance` and `Network` objects, queryable by kind/name. Backed by sqlite (`modernc.org/sqlite`, pure Go, no cgo — keeps the single-binary/distroless deployment goal from #18), file-backed by default so IPAM-assigned IPs survive a restart (see `Decisions.md` §12) — `:memory:` remains available for tests. Each sync fully replaces the prior snapshot rather than merging into it, matching config sync's full-`Config`-per-sync output (see #21); git stays authoritative for desired-state config, the store is the system of record for what's actually been assigned.
- **IPAM** — tracks `Network` definitions (CIDR + DHCP exclusion ranges), assigns static IPv4s to instances, basic duplicate detection. App is the sole source of truth; assignments persist in the durable store. No DHCP/DNS write-back is built yet; per `Decisions.md` §31 IPAM will drive DHCP reservations (Kea) and DNS records, with the DHCP mode an operator choice per `Network`.
- **Seed/installer generation** — same seed-rendering logic as the bootstrap CLI, generalized to any instance: builds `install.yaml`/`network.yaml`/`applications.yaml`/`incus.yaml`, calls `flasher-tool`, and either serves the resulting `.img` for download or flashes it directly — whichever is easiest to implement first.
- **WireGuard tunnel** (`internal/wireguard`, resolves `Decisions.md` §11 — see #91) — the app terminates a persistent WireGuard tunnel to every managed node entirely in-process, over a userspace network stack (`golang.zx2c4.com/wireguard` + a gVisor netstack — no host TUN device, no `NET_ADMIN`, keeping the deployment distroless). The app's own identity is generated once and persisted in the durable store; each instance gets a keypair minted at seed-render time and embedded into its `network.yaml`, with no live enrollment step. `SyncOnce` assigns each instance a stable address on a fixed overlay CIDR and reconciles the live tunnel's trusted-peer table on every sync; the peer table lives only in the running tunnel, so startup rebuilds it from the stored instances and their credentials before serving, and a restarted app answers already-flashed nodes without waiting for a sync (#269). `WIREGUARD_ENDPOINT` (operator-supplied, the app's externally-reachable `host:port`) gates this — see the HTTP API table below for what depends on it. `internal/nodeprovision` builds on the tunnel to let the app authenticate directly to a node's Incus API — mint a one-time bootstrap cert (the app never holds the break-glass cert's private key, see §4 below), dial over the tunnel, and revoke the cert after use — the mechanism #92's local app-manager agent deployment will use for real.
- **Cert sourcing** — the app never generates, mints, or stores a client cert itself. It reads one operator-supplied, self-signed cert (the same artifact the bootstrap CLI's `gen-cert` already produces for node #0) from local deployment config (`client.crt` in the deployment config directory, or `CLIENT_CERT_PATH`; `docs/Deployment.md` documents every deployment setting, the directory's layout and which files in it are secret, #241), and preseeds its public half into every subsequently-provisioned node's `incus.yaml`, so the whole fleet trusts a single break-glass credential identical to what node #0 trusts (not Operations Center auth — that question is shelved with the OC-wrapping decision). The private key is never read, transmitted, or persisted by the app — rotation/revocation remain deferred, but the key-storage risk that deferral originally implied is avoided entirely rather than just postponed.
- **Tailscale config** — takes an operator-supplied authkey and writes it into the node's seed for IncusOS's built-in [Tailscale service](https://linuxcontainers.org/incus-os/docs/main/reference/services/tailscale/). No per-node enrollment flow yet.
- **Observability config** — provisions an Alloy instance under Incus and points the node's remote syslog at it; Alloy forwards logs to one or more operator-configured Loki endpoints. The same Alloy instance also scrapes Incus's native `/1.0/metrics` endpoint (host + per-instance resource stats) and remote_writes it to one or more operator-configured Prometheus `remote_write` endpoints — one agent per node handles both logs and metrics rather than running a second exporter. The metrics scrape authenticates with a dedicated `metrics`-typed Incus cert (read-only, scoped to `/1.0/metrics` only), minted per-instance at seed-render time alongside the rest of that instance's seed — distinct from, and lower-privilege than, the break-glass client cert (see `Decisions.md` § Metrics). Grafana Cloud is the expected production destination, but destinations are just config, not hardcoded, and Alloy fans out to however many are configured — so a deployment can ship to Grafana Cloud, mirror to a local Grafana + Loki + Prometheus stack, or both at once (see `Decisions.md` § Metrics, "Destination"). Dev/test always includes the local stack (under `docker-compose.yml`) as at least one destination, needing no cloud credentials. Tailscale-reachable Grafana endpoint is TBD later.

### 3. Managed node(s)

IncusOS host(s) running: Incus (always), Tailscale service (authkey-enrolled), and — once observability is wired up — an Alloy Incus instance receiving the node's syslog and scraping its local Incus API for metrics.

### 4. Runtime-agnostic seams (`Decisions.md` §31)

Two seams keep the callers independent of what a node runs. Each is an interface with a registry keyed by a config value, and a test that keeps the registry in step with what `config.Validate` accepts. Today each has one implementation, which wraps the existing packages in place rather than moving them, so it behaves exactly as before the seam existed. Neither interface guesses at a backend that doesn't exist yet: an operation is added when a real backend needs one.

- **Host OS** (`internal/hostos`, #398), chosen by `Instance.HostOS()` (`os`, default `incusos`): validate an Instance, render its install config, render its install media. IncusOS wraps `internal/seed` and `internal/flasher`. There's no upgrade operation, because IncusOS updates itself; Talos adds one in Phase 6.
- **Cluster runtime** (`internal/clusterruntime`, #488), chosen by `Instance.ClusterRuntime()` (`runtime`, default `incus`; one per fleet): bootstrap, join, upgrade and health. For Incus they are the app-manager agent's lifecycle, which is all the Incus runtime does today:

  | Operation | Incus wraps | Called from |
  |---|---|---|
  | Bootstrap | The first agent on a node: `agentdeploy.FindApp`, `IsAgentInstance` and `Desired` at `FirstGeneration`, created through `nodeprovision.CreateInstance` (the one-shot bootstrap credential, revoked after use) or `CreateInstanceNoRevoke` (the break-glass cert) | `bootstrap deploy-agent`, `POST /instances/{name}/deploy-agent` |
  | Join | The leader's create of the agent on each declared member that has none (`agent.ensureAgents`, #203) | `cmd/agent`, in the leader's tick |
  | Upgrade | The blue-green agent self-upgrade (`agent.upgradeAgents`, #109) | `cmd/agent`, in the leader's tick |
  | Health | A self-upgrade candidate's sustained-healthy check (`sustainedHealthy`/`unhealthy`, on its `incusregistry.KeyHeartbeat`) | Upgrade, for each candidate |

  `internal/incusregistry`, `incuslocal` and `nodeprovision` are the Incus runtime's internals, and stay direct. Outside the seam: the Incus host cluster's own formation (the seed renders the one-member Tier A bootstrap behind the host-OS seam, and member join, #180, goes behind Join when it lands), and the agent loop around Join and Upgrade. Leader election (`internal/leaderelection`, §25), the acting-flag gating, the order of Join then Upgrade and their shared pass timeout stay in `internal/agent`, whatever runtime is set. Incus's Join, Upgrade and Health run the agent's own passes, which `internal/agent` hands them (`clusterruntime.LeaderPass`, `Candidate`), so the dependency points one way: the web app and the CLI, which only Bootstrap, don't link the agent. `cmd/agent`, being the Incus runtime's agent, runs `clusterruntime.Incus` rather than dispatching on config.

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
applications: [incus]  # IncusOS seed applications — unrelated to `kind: App` below
---
kind: App
name: agent
type: agent            # renderer-registry key
replicas: per-node     # how many: a count, or `per-node`. Required. Never *where*
image:
  server: https://ghcr.io
  protocol: oci
  alias: ehharvey/homelab-ops/agent:latest
```

This is a sketch. The field-by-field reference, with the repo layout and what each reader checks, is `docs/Config Schema.md`, and working repos live in `examples/`.

`kind: App` is the app-manager agent's unit of work; the only App it manages is the agent itself (`Decisions.md` §30), and Incus App management is parked (`Decisions.md` §31) (see `docs/AppManager.md` for the schema, the renderer registry and the blue-green reconcile algorithm; `docs/AppClasses.md` for the classes of App a renderer implements). Note the collision to keep straight: `Instance.applications` is the IncusOS *seed* applications list (`applications.yaml`, `[incus]`), which has nothing to do with `kind: App`.

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
| `/instances/{name}/deploy-agent` | POST | Puts the first app-manager agent (`agent-<instance>-g0`, built by `internal/agentdeploy` from the declared `type: agent` App) on a synced instance's node, over its WireGuard tunnel, with the instance's one-shot bootstrap credential, which is revoked afterwards, whether the create succeeds or fails (#100). Reports 200 if this node's agent instance already exists there (revoking the credential then too), 409 once the node no longer trusts the credential (recover with `bootstrap deploy-agent`, which needs no web app), and 503 without `WIREGUARD_ENDPOINT` or `CONFIG_REPO_URL`, which the agent is given as its own config repo |

Full diff detail (human-readable added/changed/removed lines) is server-log-only, not part of the JSON response — keeps the API from committing to a long-form string contract before a UI exists to consume it. No OpenAPI spec yet (see `Out of Scope.md`).

`GET /instances/{name}/image` regenerates on demand: each request copies the
multi-GB base image into a temp dir (`os.TempDir()`, i.e. `/tmp` in the
distroless image) and injects the seed, then streams and deletes it. That copy
is disk-backed, not RAM (the distroless image ships `/tmp` mode 1777 on its
writable overlay layer, no `tmpfs`), so a large image won't OOM the container —
but the deployment must give the container enough writable disk for at least
one full image copy (more if concurrent downloads are expected), or mount a
suitably-sized volume/`tmpfs` at `/tmp`.

### Who can reach the API (#195)

The routes above are unauthenticated, and the seed and image routes hand out a
node's WireGuard private key. So by default the API is served only **inside
the WireGuard tunnel**, to operator peers (`Decisions.md` §28, §29). Which
listeners run is deployment config (`docs/Deployment.md` has every setting,
its default and where it goes in the deployment config directory):

| Setting | Default | Meaning |
| --- | --- | --- |
| `WIREGUARD_ENDPOINT` | unset (no tunnel) | Starts the tunnel. With it set, the API is served at `10.100.0.1:80` inside the tunnel, on the in-process network stack, so only the WireGuard UDP port is exposed on the host. |
| `WIREGUARD_OPERATOR_PEERS` | empty | The operator devices allowed to use the tunnel API: `<overlay-address>=<base64-public-key>` entries, separated by commas or whitespace; in the deployment config directory, the `operator-peers` file, one per line with `#` comments. Each address must be in `10.100.0.240`–`10.100.0.254` (`wireguard.OperatorCIDR`), a range instances are never assigned. Only the public key reaches the web app. Requires `WIREGUARD_ENDPOINT`. |
| `API_HOST_LISTEN_ADDR` | unset (off) | **Opt-in.** Also serves the whole API as plain HTTP on this host address, e.g. `127.0.0.1:8080` or one LAN interface, with no source filter. There is no default address. Startup logs a warning naming the exposure. |
| `HEALTH_LISTEN_ADDR` | `:8081` | Serves `GET /healthz` and nothing else, for container health checks, whatever the API setting. |

- **The source filter.** The tunnel listener admits a request only when its
  source is a listed operator address, and answers 403 otherwise. That address
  check is sound because WireGuard drops any packet whose source isn't in the
  sending peer's allowed IPs, so the address identifies the key. Nodes, and
  anyone holding a node's leaked key, are refused. The filter doesn't apply to
  the host listener, where a source address proves nothing.
- **Operator setup.** Generate a keypair (`wg genkey | tee op.key | wg pubkey`),
  pick an address in the operator range, and list it in
  `WIREGUARD_OPERATOR_PEERS`. The web app logs its own public key at startup
  (`wireguard public key …`). Use that key as the operator's peer, with
  `AllowedIPs = 10.100.0.1/32` and the web app's `WIREGUARD_ENDPOINT` as its
  endpoint. Then call `http://10.100.0.1/…` through the tunnel, or use the
  operator CLI below, which needs no OS WireGuard interface. Every sync
  re-registers the operator peers alongside the instances.
- **The operator CLI (#196).** `bootstrap api <verb>` brings up its own
  userspace WireGuard peer in-process (`wireguard.StartClient`, the netstack
  the web app uses), so it needs no OS interface and no root, and calls the
  API through it. It authenticates by being the listed operator peer; it is
  plain HTTP for now (TLS and server-cert pinning: #397). The HTTP client is
  `internal/apiclient`, separate from the tunnel dialer. Its inputs are flags,
  not a config file, so a script passes them inline:

  | Flag | Default | Meaning |
  | --- | --- | --- |
  | `--private-key-file` | required | File holding this peer's base64 private key (`wg genkey`). Never a flag value, which `ps` would show. |
  | `--local-addr` | required | This peer's address, as listed for its key in `WIREGUARD_OPERATOR_PEERS`. |
  | `--server-public-key` | required | The web app's public key, from its startup log. |
  | `--endpoint` | required | The web app's `WIREGUARD_ENDPOINT`, `host:port`. A host name is resolved once, and each address it resolves to (an A and an AAAA record, say) is tried in turn until one answers; the error names each address that didn't. |
  | `--server-addr` | `10.100.0.1` | The web app's tunnel address; the API is on port 80 there. |
  | `--listen-port` | `0` (OS picks) | Local UDP port for this peer. |
  | `--connect-timeout` | `30s` | How long to wait for the tunnel to answer, at each address `--endpoint` resolves to. Bounds connecting only, not a download. A key the web app doesn't list never gets an answer, so it fails here. |

  Verbs, one per route: `status`, `networks`, `instances`, `sync` and
  `deploy-agent <instance>` print the server's JSON on stdout.
  `seed <instance> --output <file>` saves the seed JSON, and
  `image <instance> --output <file|device>` streams the image as it arrives,
  never buffered; both create a new file mode 0600, `-` writes to stdout, and
  a download cut short exits non-zero and removes the partial file. Any 4xx or
  5xx (a failed sync is a 502) exits non-zero with the server's message.
  `scripts/validate/operator-cli-drives-api-over-tunnel.sh` drives every verb
  against the compose stack.
- **Nothing configured.** The API is served on no listener, and startup warns.
- **Dev.** `docker-compose.yml` opts in with `API_HOST_LISTEN_ADDR=:8080`,
  published on the host's `127.0.0.1` only, so `curl localhost:8080` works.
  `scripts/validate/api-serves-operator-peers-over-tunnel-by-default.sh` proves
  both postures against that stack. A TLS host listener is the expected third
  mode; it isn't built.

Networking between IncusOS nodes and the web app itself (avoiding exposing nodes to the public internet) is resolved as of #91 — see § Web app's WireGuard tunnel module above and `Decisions.md` § Networking for the three options originally weighed and why WireGuard won.

## Incus Node networking to Web App

Resolved by #91 (`Decisions.md` §11): the web app and every managed node connect via a persistent WireGuard tunnel, seeded at install time with no live enrollment step — see § Web app's WireGuard tunnel module above for the mechanism, and `Decisions.md` §11 for the three options originally considered (a node-side polling management container, Tailscale on the web app, WireGuard) and why WireGuard won. `Decisions.md` §11 also sketches a not-yet-built follow-on direction: once the tunnel is proven, the web app could act as a transparent network relay between an operator and a node's Incus API, rather than just carrying the app's own traffic.