# Roadmap — Homelab Ops App (0.x)

Phased build order derived from `Architecture.md`. Each phase should produce something runnable/demonstrable before moving on.

> **Naming note (renamed 2026-06-24, see #58):** this milestone was originally called "v1" throughout the docs, code comments, and issue templates. It's now the **0.x line** — "0.x" signals pre-stable per semver, reserving a real `v1.0` tag for the first stable release. Older commits, issues, and PRs may still say "v1"; they mean the 0.x line.

## Phase 0 — Node #0 bootstrap tool

Goal: get one IncusOS machine up and trusted, with nothing else running yet.

- [x] Dev environment: DONE; see #6
- [x] Offline self-signed cert/key generation: DONE; see #1
- [x] Minimal `Instance`/`Network` YAML parsing: DONE; see #2
- [x] Seed renderer: `install.yaml` (disk target + TPM/Secure Boot flags) + `network.yaml` (static IP or DHCP) + `applications.yaml` (`incus` only): DONE; see #3
- [x] Shell out to `flasher-tool` to produce a `.img`: DONE; see #4
- [x] Manually flash + install; confirm Incus is reachable and trusts the generated cert: DONE; see #5

**Done when:** node #0 is running IncusOS + Incus, reachable, and the bootstrap cert authenticates against it.

## Phase 1 — Web app skeleton + config sync

- [x] Go service scaffold, deployed via Docker Compose in dev (no k8s — see #18); deployment targets: Docker image + plain binary: DONE; see #19
- [x] Git config sync: pull one public repo, parse k8s-style multi-doc YAML (`kind: Network`, `kind: Instance`): DONE; see #20
- [x] In-memory/local store for parsed objects: DONE; see #21
- [x] Diff against last-synced state; surface warnings (no auto-apply): DONE; see #22

**Done when:** pushing a YAML change to the repo produces a visible diff/warning in the app, with no side effects on real nodes yet.

## Phase 2 — IPAM + app-driven installer generation

- [x] IPAM: register `Network` CIDRs + DHCP-excluded ranges, assign static IPv4s to instances, duplicate-detect: DONE; see #35
- [x] Reuse Phase 0's seed-rendering logic inside the app, parameterized by any `Instance`: DONE; see #36
- [x] Wire an operator-supplied break-glass cert (read from local deployment config, never generated/stored by the app) into rendered seeds: DONE; see #36
- [x] Wire IPAM-assigned IP into the rendered `network.yaml`: DONE; wiring via #36, closed out with pull-through regression coverage in #38
- [x] Serve generated `.img` for download (download only; direct flashing deferred, see #34): DONE; see #39
- [x] `config.Validate`: reject duplicate `Network` names (silent last-wins data loss): DONE; see #52
- [x] Seed/image routes: distinguish 4xx (bad synced/render data) from 5xx (store/cert faults): DONE; see #57
- [x] `config.Validate`: reject `static_ip` colliding with its network's gateway/network/broadcast address: DONE; see #53

**Done when:** the app can take a new `Instance` entry from the synced repo and produce a working installer end-to-end, without the bootstrap CLI.

## Phase 3 — Local app-manager agent + node connectivity

> **Rejig note (2026-07-14):** this phase used to be "Tailscale, logging +
> metrics." That content didn't disappear — it moved to what was then Phase 4
> below (now **Phase 5**, since 2026-09-20 — see #177), displaced by two
> pieces of infrastructure everything after it now builds on: node↔app
> connectivity, and a self-managing app-manager agent. See `docs/Decisions.md`
> for the full rationale (cert custody, why a local agent instead of the
> always-on app driving Incus directly, the WireGuard vs. Tailscale/NetBird
> seed-hook comparison).

> **Pivot note (2026-09-28, `docs/Decisions.md` §27):** the project's next
> goal is a real 3-member Incus cluster (Phase 4). The agent still ships, but
> its first real job is joining cluster members, not deploying Apps. The app
> reconciliation items below (#98, #109, #103) are **paused**, except #98's
> zero-match branch for the agent App alone. That slice is split out as #203
> in Phase 4, so that every member runs an agent. The rest are still needed,
> because the agent is what drives the join. An agent that is deployed and
> does little else is an acceptable end state for this phase.

- [x] Seed WireGuard connectivity between nodes and the web app: web app
  generates its own identity + a per-node keypair at seed-render time,
  embedded into `network.yaml` (no live enrollment step, unlike
  Tailscale/NetBird); resolves `docs/Decisions.md` §11: DONE; see #91
- [x] Give a seeded node's `wg0` an address at the overlay's own prefix width,
  so the kernel installs the connected route the tunnel needs. The item above
  reported done since #91, but its second done-when — "the node's Incus API is
  reachable *through the tunnel specifically*" — had never actually held: a
  `/32` left the node handshaking fine and dropping every reply, with no route
  covering `10.100.0.0/24`. Not fixed the way #157 proposed (a `Routes` entry),
  because IncusOS's validator rejects the only route the vendored API can
  emit; resolves `docs/Decisions.md` §24: DONE; see #157
- [x] Seed Incus as a real one-member cluster (Tier A of `docs/Decisions.md`
  §26): `docs/AppManager.md`'s leader-election design and the agent below both
  assume one Incus API surface reachable fleet-wide, which the seed had never
  actually set up. `core.https_address` must be the instance's own static IP,
  not a wildcard bind — Incus rejects clustering outright otherwise, caught
  only by a real boot, not the initial source-read spike; a DHCP-only
  Instance skips clustering rather than erroring. Verified against a real
  booted node (`node-boots-and-trusts-bootstrap-cert.sh`, 12/0/0); growing
  past one member is Phase 4: DONE; see #178, `docs/Decisions.md` §26
- [ ] Per-node app-manager agent with an operator-designated leader: one agent
  instance per node; a single primary named in git (fenced by git commits and
  an acting handoff, so a stale checkout stands down and a new primary waits
  for the old one, `docs/Decisions.md` §25) is the only one that reconciles a new `kind: App`
  object across the whole fleet via a small renderer registry, proven by
  managing its own fleet (blue-green self-upgrade, driven fleet-wide by the
  primary) — see #92, `docs/Decisions.md` §25. Leader election is a pluggable
  interface, so the automated ranked-over-Incus election (specified in §25) can
  replace the designation later without touching the reconcile loop. Built in
  dependency order:
  - [x] `kind: App` schema + store/configdiff plumbing — no runtime
    behaviour, pure parse/validate/store, prerequisite for every step
    below; `App` carries a cardinality field rather than a separate
    `kind: AgentConfig` (see `docs/AppClasses.md`): DONE; see #97
  - [x] `internal/leaderelection` — the pluggable `Elector` interface and the
    `Designated` implementation (designated primary + epoch fencing +
    one-acting-instance-per-node through a self-upgrade). Replaces the Incus
    ETag-CAS lease #160's spike showed isn't a compare-and-swap; records every
    alternative weighed, and the deferred ranked-over-Incus protocol, in
    `docs/Decisions.md` §25: DONE; see #108. Its epoch is replaced by commit
    fencing and an acting handoff (#212), which #101 builds.
  - [x] `internal/incuslocal` — the unix-socket Incus client the reconciler and
    the `Registry` share. No conditional write: #160's own spike measured that
    Incus's `If-Match` is a lost-update guard rather than a compare-and-swap,
    so the epoch ratchet stays a single-writer read-modify-write above this
    layer (#101; since #212 there's no ratchet at all). `Exec` gave way to `ReadFile`, since `Healthy` is defined as
    the freshness of a heartbeat file: DONE; see #160
  - [ ] *(paused, §27)* App renderer registry + fleet-wide reconcile
    algorithm; fleet reconciliation only ever runs while the caller's
    `Elector` says it may. Its zero-match branch for the agent App alone is
    split out as #203 (Phase 4) — see #98
  - [x] Preseed the `incus-socket` profile onto every node
    unconditionally, since an agent now runs everywhere. The socket lands at
    `/dev/incus-host.sock` rather than the issue's `/mnt/incus/unix.socket`,
    because Incus's proxy never creates the socket's parent directory, and an
    image without it would fail to start. Proven on a real booted node: a
    container there reaches the node's own Incus through the profile: DONE;
    see #99
  - [ ] Deploy the agent's first instance — the web app route, plus the
    `bootstrap deploy-agent` CLI path, which needs no web app and is the
    recovery path if every agent is lost — see #100
  - [x] `cmd/agent` binary tying the `Elector` and the designation's git
    parsing to a loop; every node runs the same binary and the `Elector`
    decides which is active each tick. Its first loop is cluster membership
    (Phase 4), not App reconciliation — see `docs/Decisions.md` §27. Built
    minimal: `kind: Designation`, commit fencing and the acting handoff
    (§25), a persistent full clone, `user.*` keys on each
    agent's own instance, #187's sync-failure threshold and self-drain, and a
    heartbeat. When it leads, it only logs. Proven with three real agents
    against real Incus: DONE; see #101
  - [ ] *(paused, §27)* The leader marks itself draining on self version
    mismatch, once its candidate is sustained-healthy, so the candidate takes
    over and retires it during a self-upgrade — see #109
  - [x] Publish the agent image to GHCR; local registry for
    dev/validation: DONE; see #102
  - [ ] *(paused, §27)* `scripts/validate/` script proving fleet-wide blue-green + the
    designation gate (a stale-checkout primary stands down; one acting
    instance through a self-upgrade) end-to-end against #92's own done-when —
    see #103
- [ ] Tie `bootstrap`'s `flasher-tool` to the same pin as the web image, so
  the two cannot drift from one another. Matters more under Phase 4: Incus
  rejects a joiner whose version differs from the cluster's. IncusOS's own
  updates are the larger source of that drift, so the join loop retries on a
  mismatch as well (`docs/Decisions.md` §27) — see #68
- [ ] Unified config: consolidate config passing into one place and fail
  fast on missing required values, rather than resolving it just-in-time
  across the codebase — see #67

**Done when** (revised 2026-09-28, `docs/Decisions.md` §27): a managed node
has a persistent WireGuard tunnel to the web app, and the web app has deployed
the app-manager agent, published as an image, onto it. The agent runs under
`Designated` and otherwise does little until Phase 4 gives it membership work.

The original done-when, kept for when the paused items resume: a per-node
agent fleet whose designated primary deploys and upgrades the fleet
(blue-green, fleet-wide, including its own self-upgrade with no reconciliation
gap) from git-declared config, with a stale-checkout primary fencing itself out
once a peer is on a newer commit (§25; this originally said
"a higher epoch").

## Phase 3.5 — The validate suite made runnable

> **Detour note (2026-07-19, see #115):** this is not a phase in the sense
> the others are — it produces no runnable node or service. It is recorded
> as one because the work was large, it interrupted Phase 3 between #91 and
> #92, and a roadmap that omits it makes Phase 3 look stalled for no
> reason. It interleaves with the remaining #92 work rather than gating it.
>
> The trigger: #115 established that nothing ran `scripts/validate-*.sh` at
> all, that two scripts had been silently 503ing for weeks after #107 gated
> the seed and image routes on `WIREGUARD_ENDPOINT`, and that three
> assertions passed for the wrong reason. The sharpest was a `cmp -s` that
> could not distinguish a 40-byte 503 body from a 3.2 GB image — so it
> reported PASS throughout the window the route was broken, for precisely
> the failure it existed to catch. Rationale in `docs/Decisions.md` §20
> (bash vs. Go, CI transport, OVN) and §22 (the delivered contract).

- [x] Decide what the suite *is* before moving it: stays bash with an
  extracted `lib.sh` rather than becoming Go, CI reaches hardware over
  Tailscale rather than via a self-hosted runner, and no OVN; resolves
  `docs/Decisions.md` §20: DONE; see #127
- [x] Unbreak the two scripts failing since #107, and close the three
  assertions that passed for the wrong reason: DONE; see #129, #134
- [x] Parametrize the hardware scripts' remote/project/network
  (`VALIDATE_INCUS_REMOTE`/`_PROJECT`/`_NETWORK`) so they stop hardcoding
  one operator's dev host: DONE; see #132
- [x] Move the suite to `scripts/validate/`, each script named for the
  behaviour it proves rather than the issue that prompted it — an issue
  number ages into meaninglessness, and the originating issue is recorded
  in each file's header comment instead: DONE; see #138
- [x] Shared `lib.sh` harness and a real skip contract: an unmet
  prerequisite is a SKIP with its own exit code, never a FAIL, and
  `run.sh --describe` lets the scripts declare what they need rather than
  the docs claiming it; resolves `docs/Decisions.md` §22: DONE; see #140,
  #136
- [x] Make the tunnel script's own assertions honest — a per-run proof that
  the simulated NAT actually translates, a handshake check that reads the
  peer's state structurally instead of grepping the whole document, and the
  harness finally dialling node0 with node0's own key and endpoint. Its three
  headline assertions had never passed since #91 introduced them; records why
  the test cannot be made hermetic in `docs/Decisions.md` §23: DONE; see #137

**Done when:** the suite is honest about its own results — a missing tool
reports as a skip rather than a failure, a script that silently gains a
precondition fails rather than passing quietly, and every script can say
what it proves and what it needs without being read. (Reached. What
remains is *enforcement* — nothing runs the suite automatically yet; that
work is tracked separately and is not part of this section.)

## Phase 4 — Multi-member Incus clustering

> **New phase (2026-09-20, see #177, `docs/Decisions.md` §26):** displaces the
> old Phase 4 ("Tailscale, logging + metrics"), which renumbers to Phase 5
> below with no content change — same pattern as #58's v1→0.x rename. Phase 3
> grows Incus into a real *one-member* cluster (`docs/Decisions.md` §26's Tier
> A, tracked there, not here); this phase is Tier B, growing that to real
> multi-member clustering. Genuinely out of scope for 0.x until now — see the
> corrected framing in `docs/Architecture.md` and `docs/Out of Scope.md`.
> Tracked by #179, mirroring #92's role for Phase 3.

> **Pivot note (2026-09-28, `docs/Decisions.md` §27):** this is now the
> project's main line. Joining is driven by the designated primary's agent.
> It finds the fresh node on its own subnet (trust on first use, for now),
> mints the token over its own unix socket, fills `member_config` from live
> cluster state, and calls `PUT /1.0/cluster` on the fresh node. Seeds don't
> depend on live cluster state. A joining node can't run an agent itself,
> because the join wipes its database; once the node has joined, the primary
> puts an agent on it. The joiner trusts an operator-generated joiner cert,
> preseeded into joining nodes' seeds only. Everything below waits on a manual
> join spike.

- [x] Manual join spike: join a second VM to a Tier A cluster by hand
  (token minted on member 1, hand-filled `member_config`), both at seed time
  and after boot, and record what fails and what the new member is missing.
  Sets the scope of the items below — see `docs/Decisions.md` §27
  (its "Evidence" subsection): DONE; see #193
- [ ] Cluster membership config model: declare which `Instance` bootstraps
  the cluster vs. joins it, with no addresses required in git — see #181
- [ ] Joiner seed variant (no cluster or profile preseed, `apply_defaults:
  false` set explicitly) and the operator-generated joiner cert, preseeded
  into joiners only — see #194
- [ ] Agent-driven join: the primary's agent discovers joiners on its own
  subnet with the joiner cert (trust on first use for now,
  `docs/Decisions.md` §27), waits out version mismatches, mints tokens over
  its unix socket, and joins missing members over the LAN — see #180
- [ ] Every member runs an agent: #98's zero-match branch for the agent App
  alone, each agent created on its own member — see #203
- [ ] Storage/network parity across members — largely the `member_config`
  and post-join volume fix-up the join step needs anyway — see #183
- [ ] *(paused, §27)* Cluster-group placement: a `Target` field on
  `kind: App`, since Incus's scheduler stops being a trivial decision once
  members > 1. Nothing needs it while App reconciliation is paused — see #182
- [ ] A 3-VM validate script proving real node-loss fault tolerance, closing
  #92's own done-when caveat that 0.x never proved surviving an actual
  physical node's loss — see #184
- [ ] Member add/remove and quorum-loss recovery runbook — see #185

**Done when:** a fleet of ≥3 real Incus cluster members, joined by the agent
rather than by hand and each running an agent, survives losing any one
member. Incus keeps quorum and the remaining agents keep running, with no
operator intervention beyond eventually replacing the member — plus, if it
was the designated primary, the operator's §25 failover (commit a new
`primary`, #212) before the replacement can be joined. (Placement of `kind: App`
workloads across members is paused with #182; see `docs/Decisions.md` §27.)
`docs/Decisions.md` §25's `leaderelection.Designated` is re-weighed against the deferred
ranked-over-Incus election once real membership exists to make the automated
option's cost worth paying.

## Web app hardening (alongside Phase 4)

> **Added 2026-09-28 (#201, `docs/Decisions.md` §28).** Not a phase in the
> sense the others are — like Phase 3.5, it's recorded here so the work has a
> home. It is independent of Phase 4's critical path; #195 fixes a live leak
> and is worth doing early. Proper auth and a JS client are deferred (§28);
> this is the interim posture. The web app is a provisioning and observation
> plane: nothing on a node or in the agent may call it synchronously, and
> the cluster must keep running while it's down or locked (§27).

- [x] Serve the HTTP API over the web app's in-process WireGuard tunnel, to
  operator peers, by default; a plain-HTTP host listener only by explicit
  opt-in (`docs/Decisions.md` §28, §29). Closes the leak where the seed and image
  routes hand any caller a node's WireGuard private key (#63): DONE; see #195
- [ ] Operator CLI for the API, over the tunnel — see #196
- [ ] Operator-held symmetric key: held only in memory, web app locked on
  restart, store encrypted at rest, and a wrong key on unlock never resets
  anything. The web app's WireGuard identity becomes operator-supplied
  deployment config — see #197
- [ ] Encrypted seed, image and snapshot downloads; the CLI decrypts
  straight to the install media — see #198
- [ ] Network addressing from git (private repo) or web app state, exactly
  one source per network — see #199
- [ ] *(optional)* Push encrypted store snapshots to off-cluster S3 — see
  #200

**Done when:** by default, the web app's API is reachable only by operator
peers over WireGuard; nothing it stores or serves exists in plaintext outside
its own memory, apart from its WireGuard identity (operator-supplied
deployment config, `docs/Decisions.md` §28); and its state survives the loss
of its host.

## Web app deployment (alongside Phase 4)

> **Added 2026-10-03 (#247, `docs/Decisions.md` §29).** Like Web app
> hardening, this sits off Phase 4's critical path. The web app runs on its
> own appliance machine, either an internet-reachable VM or a spare machine on
> the nodes' LAN. Every deployment setting is an operator choice with a safe
> default. An internet-reachable deployment needs #195 first.

- [x] Publish the web image to GHCR from `main`: DONE; see #240
- [ ] Deployment config contract: one directory, every setting with its
  default, the WireGuard endpoint as a DNS name — see #241
- [ ] Build install media client-side: a local image build from a downloaded
  seed, or a `SEED_DATA` stick beside a stock IncusOS image — see #242
- [ ] Docker-box appliance: production compose, VM and LAN runbooks, and a
  validate script — see #243
- [ ] Provision node0 from the web app by default; the bootstrap CLI as the
  alternative — see #244
- [ ] Migrate the web app between hosts without reflashing nodes, including
  the endpoint DNS re-resolution probe — see #245
- [ ] *(later)* IncusOS turnkey appliance: appliance seed plus
  `bootstrap deploy-web`, which is also the path into the fleet — see #246

**Done when:** the web app runs from the published image on a dedicated
appliance machine, either an internet VM or a LAN machine. The operator
chooses each deployment setting, and it can be moved to another host without
reflashing any node. HA, as a warm standby, is decided after that
(`docs/Decisions.md` §29).

## Phase 5 — Tailscale, logging + metrics

- [ ] Accept an operator-supplied Tailscale authkey per instance; bake into seed via IncusOS's Tailscale service (blocked on upstream seed support — see #76, deferred)
- [x] Add a local Grafana + Loki + Prometheus dev stack under `docker-compose.yml`, so log/metric forwarding can be validated without live Grafana Cloud credentials (see #82)
- [ ] Stand up an Alloy Incus instance; point node syslog at it — now a renderer registered with Phase 3's app-manager agent (see #92) rather than built from scratch — see #77
- [ ] Alloy → Grafana forwarding confirmed end-to-end (local stack by default; Grafana Cloud is the real production destination, checked separately) — see #78
- [ ] Mint a per-instance `metrics`-typed Incus cert at seed-render time (shared `internal/cert`+`internal/seed` code, used by both the bootstrap CLI and the web app) and preseed it via `incus.yaml` for Alloy's local scrape — invisible to the operator, distinct from the break-glass client cert — see #79
- [ ] Extend the per-node Alloy instance to scrape Incus's native `/1.0/metrics` endpoint and remote_write to Grafana, alongside its existing syslog forwarding — see #80

**Done when:** a freshly provisioned node is reachable over Tailscale and both its logs and its resource metrics (host + instance, CPU/memory/disk/network) show up in Grafana Cloud.

## Deferred / post-1.0 — tooling and dev-host health

Not on the path to 1.0, but tracked here rather than nowhere: keeping the dev
host reproducible and the validation suite drivable in CI. The suite itself
was made runnable during the Phase 3.5 detour (#138, #140); what remains here
is the host it runs against and the CI that will drive it.

- [x] Dev Incus host cleanup — copy-on-write storage pool, pinned base images
  instead of a moving `images:alpine/edge` tag, `homelab-dev` deleted, and
  client/server version agreement asserted rather than hoped for: DONE; see
  #131 (folds in #96), `docs/Decisions.md` §21
- [ ] CI for the validate suite: a hosted runner joining the tailnet and
  driving throwaway Incus sandboxes on `homelab-host`, with `tofu/ci/`
  expressing the host state — see #115, `docs/Decisions.md` §20

**Done when:** the suite runs unattended in CI against a dev host whose state
is reproducible from the repo rather than from memory.
