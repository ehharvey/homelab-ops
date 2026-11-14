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

## Phase 3 — The agent and node connectivity

> **Pivot note (2026-11-02, #466, `docs/Decisions.md` §31):** the main line
> moved to bare-metal Kubernetes on Talos. This phase is unchanged and is
> finished first; after it, the agent and the rest of the Incus runtime are
> kept building but parked (see "Parked — Incus runtime" below).

> **Re-plan note (2026-10-26, #401, `docs/Decisions.md` §30):** the goal moved
> from App management to host support and management. The agent manages only
> itself and its cluster: it deploys, runs one per managed host (#203, moved
> here from the cluster phase) and upgrades itself (#109, #103, narrowed to
> the agent). Generic App reconciliation (#98) moved to Phase 8, #68 to
> Phase 5 and #67 to Phase 4. The two notes below are kept as history, in
> the phase numbering of their dates. Of their "paused" items, #98 is now
> Phase 8, while #109 and #103 are this phase's agent self-upgrade.

> **Rejig note (2026-07-14):** this phase used to be "Tailscale, logging +
> metrics." That content didn't disappear — it moved to what was then Phase 4
> below (Phase 5 from 2026-09-20, see #177; **Phase 7** since 2026-10-26,
> §30), displaced by two
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
  actually set up. The API listens on every address
  (`core.https_address: ":8443"`, so it answers over `wg0` too), and the
  member address advertised to future members is the instance's own static
  IP (`cluster.https_address`), which Incus requires at bootstrap; a
  DHCP-only Instance skips clustering rather than erroring. Verified against
  a real booted node (`node-boots-and-trusts-bootstrap-cert.sh`); growing
  past one member is parked (`docs/Decisions.md` §31): DONE; see #178, #315, `docs/Decisions.md` §26
- [x] Per-host app-manager agent with an operator-designated leader, managing
  only itself (`docs/Decisions.md` §30): one agent instance per managed host;
  a single primary per cluster, a standalone host's included, named in git
  (fenced by git commits and an acting handoff, so a stale checkout stands
  down and a new primary waits for the old one,
  `docs/Decisions.md` §25), proven by the agent's own blue-green self-upgrade.
  Reconciling other `kind: App` objects is parked (#92, `docs/Decisions.md` §31). Leader election is a
  pluggable interface, so the automated ranked-over-Incus election (specified
  in §25) can replace the designation later without touching the loop. Built
  in dependency order: DONE; see #109
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
  - [x] Preseed the `incus-socket` profile onto every node
    unconditionally, since an agent now runs everywhere. The socket lands at
    `/dev/incus-host.sock` rather than the issue's `/mnt/incus/unix.socket`,
    because Incus's proxy never creates the socket's parent directory, and an
    image without it would fail to start. Proven on a real booted node: a
    container there reaches the node's own Incus through the profile: DONE;
    see #99
  - [x] Deploy the agent's first instance — the web app route, plus the
    `bootstrap deploy-agent` CLI path, which needs no web app and is the
    recovery path if every agent is lost: DONE; see #100
  - [x] `cmd/agent` binary tying the `Elector` and the designation's git
    parsing to a loop; every node runs the same binary and the `Elector`
    decides which is active each tick. Its first loop is cluster membership
    (parked, §31), not App reconciliation — see `docs/Decisions.md` §27. Built
    minimal: `kind: Designation`, commit fencing and the acting handoff
    (§25), a persistent full clone, `user.*` keys on each
    agent's own instance, #187's sync-failure threshold and self-drain, and a
    heartbeat. When it leads, it only logs. Proven with three real agents
    against real Incus: DONE; see #101
  - [x] One agent on every managed host: on cluster members, the designated
    primary creates it with `target=<member>` (#98's zero-match branch for the
    agent App alone); a standalone host gets it from the web app's one-shot
    window or `bootstrap deploy-agent` (`docs/Decisions.md` §27, §30): DONE;
    see #203
  - [x] Agent self-upgrade: when the agent App's `image` in git changes, the
    primary's agent creates every host's successor, its own on its own host
    (#98's image-change handling, for the agent App alone). On the primary's
    host the old instance drains once its candidate is sustained-healthy, and
    the candidate takes over and retires it; the primary retires a follower's
    old instance. The primary never moves, and a standalone host leads only
    when a `kind: Designation` names it, like any one-member cluster
    (`docs/Decisions.md` §25, §30): DONE; see #109
  - [x] Publish the agent image to GHCR; local registry for
    dev/validation: DONE; see #102
  - [x] `scripts/validate/` script proving the agent's self-upgrade + the
    designation gate (a stale-checkout primary stands down; one acting
    instance per cluster through a self-upgrade) end-to-end, against this
    phase's done-when.
    `agent-self-upgrades-blue-green-without-gap.sh` bumps the agent App's
    image in git on a real standalone Incus host and shows the agent replaced
    blue-green with no operator step: the candidate is marked healthy, the old
    instance drains (`superseded`) only after the minimum healthy duration,
    the candidate takes over and retires it, at most one instance acts at any
    sample, and some agent's heartbeat stays fresh throughout. The
    stale-checkout stand-down is `agents-act-one-at-a-time-through-failover.sh`'s
    (#101, #212): DONE; see #103
- [x] Decide whether `wg0` keeps the IncusOS management role: DONE (dropped;
  eth0 is the only `management` interface); see #342

**Done when** (revised 2026-10-26, `docs/Decisions.md` §30): each managed
host has a persistent WireGuard tunnel to the web app and runs one agent,
deployed by the web app or `bootstrap deploy-agent`. Changing the agent image
in git replaces every host's agent, with one acting instance per cluster
throughout. A standalone host is a one-member cluster: when a
`kind: Designation` names it, its agent creates its successor on itself.
(At most one Designation per repo, so one cluster or standalone host per repo
upgrades unattended until #440.)

The fleet-wide done-when (blue-green for every App, not only the agent) is
#92's, and moved with it to Phase 8, now parked (`docs/Decisions.md` §31).

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
- [x] A fetchable, verified IncusOS base image, so the `[base-image]` checks
  stop skipping wherever nobody placed one by hand: `make incusos-base`
  fetches the version pinned in `scripts/incusos-base.version`, verifies it
  against the signed CDN manifest, and caches it once for every worktree; the
  harness defaults `INCUSOS_BASE_IMAGE` to that pinned image: DONE; see #296

**Done when:** the suite is honest about its own results — a missing tool
reports as a skip rather than a failure, a script that silently gains a
precondition fails rather than passing quietly, and every script can say
what it proves and what it needs without being read. (Reached. What
remains is *enforcement* — nothing runs the suite automatically yet; that
work is tracked separately and is not part of this section.)

## Phase 4 — Runtime-agnostic seams

> **New phase (2026-11-02, #466, `docs/Decisions.md` §31):** IncusOS and Incus
> go behind a host-OS seam and a cluster-runtime seam, behaviour-neutral,
> before Talos arrives. #398 moved here from the old Phase 6.

- [x] An `os` field on `Instance` and the `internal/hostos` seam (validate,
  render install media and config), with IncusOS behind it, wrapping `seed`
  and `flasher` (`docs/Decisions.md` §15 Prerequisite 1, §31): DONE; see #398
- [x] A cluster `runtime` (`incus | kubernetes`) on `Instance`, and the
  allowed-pairs table of `os` and `runtime`, checked in `config.Validate`
  along with one `os` and one `runtime` per fleet (`docs/Decisions.md` §30,
  §31): DONE; see #487
- [x] The `internal/clusterruntime` seam (bootstrap, join, upgrade, health),
  with Incus behind it, wrapping `agent` and `agentdeploy` in place;
  `leaderelection` stays in the agent loop around the seam
  (`docs/Decisions.md` §31): DONE; see #488
- [x] Validate scripts take the backend as a parameter (`VALIDATE_OS`), still
  named for behaviour (`docs/Decisions.md` §31): DONE; see #486
- [x] `internal/secureboot`: the project's PK, KEK and db, with Microsoft's
  UEFI CA enrolled by default as an operator choice (`docs/Decisions.md` §6,
  §31): DONE; see #489
- [x] `dhcp.mode` on `Network` (`off`, `proxy`, `reservations`, `full`) and
  IPAM's reservations as an output (`docs/Decisions.md` §5, §31): DONE; see
  #490

**Done when:** IncusOS provisioning and the agent work unchanged behind both
seams, proven by the existing validate scripts (e.g.
`scripts/validate/node-boots-and-trusts-bootstrap-cert.sh`) run with
`VALIDATE_OS=incusos`.

## Phase 5 — Web app appliance: bootstrap and break-glass

> **NixOS appliance (2026-11-08, #243, `docs/Decisions.md` §29):** the
> "Cloud-init Debian appliance" item became the NixOS appliance, and the
> `later` mkosi-built appliance image (#393) was removed from "Deferred —
> after the Talos main line": NixOS replaces it. The internet VM (#569),
> appliance CI (#570) and the web app's view of the host OS (#571) were
> added as their own items.

> **`later` items moved out (2026-11-04, #512):** the mkosi-built appliance
> image (#393), the web app as a bastion (#399) and pushing store snapshots to
> off-cluster S3 (#200) are `later` work. They moved to "Deferred — after the
> Talos main line" and have no phase yet. The notes below are kept as history.

> **Renumbered (2026-11-02, #466, `docs/Decisions.md` §31):** this was Phase
> 4, "Web app appliance and hardening", and moved after the seams and before
> the first Talos node, since Talos's bootstrap is what it's for. The LAN form,
> a dedicated laptop, is the bootstrap and break-glass appliance: DHCP and DNS
> from IPAM until the cluster takes them over (Phase 8), the signing key, and
> the boot source after a full power cut. `bootstrap deploy-web` (#386) and the
> IncusOS turnkey appliance (#246, #394) moved to Parked. The note below is
> kept as history.

> **New phase (2026-10-26, #401, `docs/Decisions.md` §30):** the former "Web
> app deployment" and "Web app hardening" sections, which sat alongside the
> cluster phase, folded into one phase. #67 moved here from Phase 3, because
> #241's config contract absorbs it. The web app runs on its own appliance
> machine, either an internet-reachable VM or a spare machine on the nodes'
> LAN (§29), and every deployment setting is an operator choice with a safe
> default. It is a provisioning and observation plane: nothing on a node or in
> the agent may call it synchronously, and the cluster must keep running while
> it's down or locked (§27). Proper auth and a JS client are deferred (§28);
> the hardening items are the interim posture.

Appliance (from "Web app deployment", added 2026-10-03, #247, §29):

- [x] Publish the web image to GHCR from `main`: DONE; see #240
- [x] NixOS appliance: a generic image built from this repo's flake
  (`deploy/nixos/`), set up at first boot from a NoCloud seed
  (`bootstrap render-appliance-seed`), running the production compose file
  (`deploy/compose.yaml`), with `system.autoUpgrade` from `main` that reboots
  only when the kernel, initrd or modules changed. The laptop runbook, a
  nixosTest and a validate script: DONE; see #243
- [ ] Install the NixOS appliance on an internet VM with nixos-anywhere:
  a disko layout, the config directory through `--extra-files`, and a
  runbook — see #569
- [ ] Appliance CI: `nix flake check` on PRs, publishing the generic image,
  and PRs that bump the NixOS release branch — see #570
- [ ] The appliance's NixOS release, last upgrade and end of life, shown in
  the web app — see #571
- [x] Deployment config contract: one directory, every setting with its
  default, the WireGuard endpoint as a DNS name. It absorbs #67's unified
  config (one place for config, failing fast on missing required values):
  DONE; see #241
- [ ] Build install media client-side: a local image build from a downloaded
  seed, or a `SEED_DATA` stick beside a stock IncusOS image; per OS behind
  Phase 4's `internal/hostos` — see #242
- [ ] Provision node0 from the web app by default; the bootstrap CLI as the
  alternative — see #244
- [ ] Migrate the web app between hosts without reflashing nodes, including
  the endpoint DNS re-resolution probe — see #245

Bootstrap and break-glass (added 2026-11-02, #466, §31):

- [ ] The LAN laptop as the bootstrap and break-glass appliance: it holds the
  Secure Boot db signing key and the operator credentials (`talosconfig`,
  `kubeconfig`) and signs install media; PK and KEK private keys stay offline
  on USB flash storage, later a YubiKey (`docs/Decisions.md` §31). To file.
- [ ] Kea 3.0 on the appliance, its config rendered from IPAM in
  `reservations` and `full` modes, leases in a local file
  (`docs/Decisions.md` §31). It carries two items #490 deferred: the seed
  rendering `full`-mode nodes as DHCP clients (until then it writes
  `static_ip` in every mode), and excluding the appliance's own address from
  allocation, added to `config.Network.Allocation`'s carve. To file.
- [ ] DNS records from IPAM, served by the appliance (`docs/Decisions.md`
  §31). To file.

Operator surface, keys, CA and IdM (added 2026-11-04, #505, §32):

- [x] Record the decision: a browser UI behind kanidm, TPM-sealed first-boot
  keys, step-ca and kanidm as sidecars (`docs/Decisions.md` §32): DONE; see #505
- [ ] step-ca as an app-managed sidecar: offline root (or generated on the
  appliance onto a YubiKey or a USB stick), online intermediate, TLS certs for
  the web app and kanidm — see #506
- [ ] kanidm as an app-managed sidecar, bootstrapped by the web app, with an
  OIDC client for the UI — see #507
- [ ] Browser UI behind OIDC login via kanidm, with the operator cert as
  break-glass — see #508
- [ ] *(operator choice)* First-boot WireGuard identity and store key, sealed
  to the TPM, with an operator recovery key — see #509
- [ ] First-boot setup without a console: one-time token, fingerprints, a
  setup page that closes after use — see #510

Hardening (from "Web app hardening", added 2026-09-28, #201, §28):

- [x] Serve the HTTP API over the web app's in-process WireGuard tunnel, to
  operator peers, by default; a plain-HTTP host listener only by explicit
  opt-in (`docs/Decisions.md` §28, §29). Closes the leak where the seed and image
  routes hand any caller a node's WireGuard private key (#63): DONE; see #195
- [x] Operator CLI for the API, over the tunnel: DONE; see #196
- [ ] Operator-held symmetric key: held only in memory, web app locked on
  restart, store encrypted at rest, and a wrong key on unlock never resets
  anything. The web app's WireGuard identity becomes operator-supplied
  deployment config — see #197
- [ ] Encrypted seed, image and snapshot downloads; the CLI decrypts
  straight to the install media — see #198
- [x] Network addressing from git (private repo) or web app state, exactly
  one source per network: DONE; see #199
- [ ] One operator client cert for both Incus and the web app's tunnel API;
  its custody is #397's to decide (`docs/Decisions.md` §30) — see #397

**Done when:** both former sections' done-whens hold, the laptop appliance
serves DHCP and DNS from IPAM and holds the signing key, and the operator logs
in to its browser UI through kanidm.

- *Appliance:* the web app runs from the published image on a dedicated
  appliance machine, either an internet VM or a LAN machine. The operator
  chooses each deployment setting, and it can be moved to another host without
  reflashing any node. HA, as a warm standby, is decided after that
  (`docs/Decisions.md` §29).
- *Bootstrap and break-glass:* on the nodes' LAN, the laptop hands out
  IPAM's reservations (and, in `full` mode, a pool) and answers DNS for every
  `Instance`, proven by a new `scripts/validate/appliance-serves-dhcp-and-dns-from-ipam.sh`.
- *Hardening:* by default, the web app's API is reachable only by operator
  peers over WireGuard; nothing it stores or serves exists in plaintext outside
  its own memory, apart from its WireGuard identity (operator-supplied
  deployment config, `docs/Decisions.md` §28), and in TPM mode not even that
  (§32); and its state survives the loss of its host.
- *Operator surface, CA and IdM:* the browser UI is served over TLS with a
  step-ca cert and admits only a kanidm login or the operator's client cert,
  proven by `scripts/validate/browser-ui-requires-kanidm-login.sh`; a
  TPM-sealed appliance comes back unlocked after a reboot, proven by
  `scripts/validate/web-unseals-from-tpm-after-reboot.sh` (`docs/Decisions.md`
  §32).

## Phase 6 — First Talos node

> **New phase (2026-11-02, #466, `docs/Decisions.md` §31, §15):** one Talos
> node, provisioned from the appliance, before any cluster.

- [ ] Talos `hostos` backend: machine config from `Instance` and `Network`
  via the `siderolabs/talos/pkg/machinery` Go module, IPAM's static IP,
  WireGuard to the web app in machine config, and install media built with
  Talos's `imager` locally (`docs/Decisions.md` §31). To file.
- [ ] Secure Boot: the node boots in Setup Mode and its first boot, from USB
  or disk media, enrolls the project's keys; images are signed on the
  appliance (`docs/Decisions.md` §6, §31). To file.
- [ ] Spike: TPM-bound disk encryption (PCR 7 plus the signed PCR 11 policy)
  across the disk and iPXE boot paths (`docs/Decisions.md` §31). To file.
- [ ] A validate script proving a Talos node, provisioned from the appliance
  with Secure Boot enrolled, reaches `Ready`. To file.

**Done when:** one Talos node, provisioned from the appliance with the
project's Secure Boot keys enrolled, reaches `Ready`.

## Phase 7 — Talos cluster

> **New phase (2026-11-02, #466, `docs/Decisions.md` §31):** three
> control-plane nodes on one disk each, bootstrapped once from the appliance.

- [ ] A cluster declaration with `runtime: kubernetes`: three control-plane
  nodes, workloads allowed on them (`docs/Decisions.md` §31). To file.
- [ ] The Talos bootstrap run exactly once, by the appliance through the
  Talos API (`docs/Decisions.md` §31). To file.
- [ ] Talos's shared VIP for the Kubernetes API. To file.
- [ ] Talos and Kubernetes upgrades driven from git. To file.
- [ ] A validate script proving the cluster survives losing one node. To file.

**Done when:** a 3-node Talos cluster survives losing any one node and
upgrades Talos and Kubernetes from git.

## Phase 8 — The web app in the cluster

> **New phase (2026-11-02, #466, `docs/Decisions.md` §31):** the web app,
> DHCP and DNS move from the laptop into the cluster, and leadership moves to
> a Kubernetes `Lease`.

- [ ] The web app as Talos static pods, declared in machine config
  (`docs/Decisions.md` §31). To file.
- [ ] Leadership: the in-cluster copies and the appliance compete for one
  `coordination.k8s.io` `Lease`; with the API unreachable, break-glass is
  operator-triggered, never automatic (`docs/Decisions.md` §25, §31). To file.
- [ ] Kea hot-standby pair on two nodes, Kea's HA choosing who answers, with
  the laptop as a backup peer while connected (`docs/Decisions.md` §31). To
  file.
- [ ] DHCP handoff v1, laptop ↔ cluster, both directions: stop, copy leases
  with `lease_cmds`, start (`docs/Decisions.md` §31). To file.
- [ ] DNS served in the cluster (`docs/Decisions.md` §31). To file.
- [ ] TLS between Kea's peers, with certs from the appliance's step-ca
  (`docs/Decisions.md` §31, §32; the CA itself is #506). To file.

**Done when:** the web app and DHCP hand off laptop → cluster → laptop with
no lease lost, proven by a new
`scripts/validate/dhcp-hands-off-without-losing-leases.sh`.

## Phase 9 — Workloads on Kubernetes

> **New phase (2026-11-02, #466, `docs/Decisions.md` §31, §19):** apps come
> from git through Flux; no app reconciler of our own. Observability (#77–#80)
> moved here from the old Phase 8, to be re-targeted from an Incus instance to
> Kubernetes.

- [ ] Flux, bootstrapped against the fleet repo. To file.
- [ ] Local volumes on Talos user volumes (`UserVolumeConfig`). To file.
- [ ] The single-replica `StatefulSet` pattern for stateful apps, as a Helm
  chart or Kustomize base (`docs/Decisions.md` §31). To file.
- [ ] A database through an operator (CloudNativePG, say;
  `docs/Decisions.md` §19). To file.
- [x] Add a local Grafana + Loki + Prometheus dev stack under `docker-compose.yml`, so log/metric forwarding can be validated without live Grafana Cloud credentials (see #82)
- [ ] Alloy collecting node logs, re-targeted to Kubernetes — see #77
- [ ] Alloy → Grafana forwarding confirmed end-to-end (local stack by default; Grafana Cloud is the real production destination, checked separately) — see #78
- [ ] Metrics scrape credentials, re-targeted from a `metrics`-typed Incus cert to Kubernetes — see #79
- [ ] Host and workload metrics scraped and remote-written to Grafana — see #80

**Done when:** a stateful single-replica app and an operator-managed database
run from git, and a node's logs and resource metrics show up in Grafana.

## Phase 10 — Tailscale

> **Renumbered (2026-11-02, #466, `docs/Decisions.md` §31):** this was Phase
> 7. On Talos, the authkey goes to Talos's Tailscale extension rather than an
> IncusOS seed service; #76 is re-scoped to match.

- [ ] Accept an operator-supplied Tailscale authkey per instance; bake it into Talos's Tailscale extension config (IncusOS's seed service is blocked upstream) — see #76

**Done when:** a freshly provisioned node is reachable over Tailscale.

## Parked — Incus runtime

> **New section (2026-11-02, #466, `docs/Decisions.md` §31):** the Incus code
> is kept building, not deleted: IncusOS is one host-OS backend and Incus one
> cluster runtime behind Phase 4's seams. What follows is kept, with its
> history, but nothing here is scheduled. It's also where a VM host for
> Windows guests would come from (§31). This heading deliberately doesn't
> start with "Phase", so `/next` never treats it as the current phase.

- [ ] *(parked)* `bootstrap deploy-web`: the web image as an Incus OCI
  container on any Incus host — see #386
- [ ] *(parked)* IncusOS turnkey appliance: an appliance seed and a hardware
  runbook. An IncusOS seed can't create instances, so it always needs
  `deploy-web` after boot (`docs/Decisions.md` §29) — see #246, #394

### Multi-node: same-OS Incus clusters (was Phase 5)

> **Re-plan note (2026-10-26, #401, `docs/Decisions.md` §30):** this was
> Phase 4; the web app appliance took that number. A cluster's members all run
> one host OS, since Incus rejects a joiner whose version differs at `Accept`
> (§27). #203 (an agent on every member) moved to Phase 3, which puts an agent
> on every managed host; #182 (placement) moved to Phase 8 with App
> management; #68 (the version pin) moved here from Phase 3. The notes below
> are kept as history; their "Phase 4" is this phase.

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
  the cluster vs. joins it, with no addresses required in git, and scope
  `kind: Designation` to a cluster (`docs/Decisions.md` §30). The same-OS
  check landed fleet-wide in #487; this narrows it to each cluster — see #181
- [ ] Tie `bootstrap`'s `flasher-tool` to the same pin as the web image, so
  the two cannot drift from one another: Incus rejects a joiner whose version
  differs from the cluster's. IncusOS's own updates are the larger source of
  that drift, so the join loop retries on a mismatch as well
  (`docs/Decisions.md` §27) — see #68
- [ ] Joiner seed variant (no cluster or profile preseed, `apply_defaults:
  false` set explicitly) and the operator-generated joiner cert, preseeded
  into joiners only — see #194
- [ ] Agent-driven join: the primary's agent discovers joiners on its own
  subnet with the joiner cert (trust on first use for now,
  `docs/Decisions.md` §27), waits out version mismatches, mints tokens over
  its unix socket, and joins missing members over the LAN — see #180
- [ ] Storage/network parity across members — largely the `member_config`
  and post-join volume fix-up the join step needs anyway — see #183
- [ ] A 3-VM validate script proving real node-loss fault tolerance, closing
  #92's own done-when caveat that 0.x never proved surviving an actual
  physical node's loss — see #184
- [ ] Member add/remove and quorum-loss recovery runbook — see #185

**Done when:** a fleet of ≥3 real Incus cluster members on one host OS,
joined by the agent rather than by hand and each running an agent, survives losing any one
member. Incus keeps quorum and the remaining agents keep running, with no
operator intervention beyond eventually replacing the member — plus, if it
was the designated primary, the operator's §25 failover (commit a new
`primary`, #212) before the replacement can be joined. (Placement of `kind: App`
workloads across members is parked too, with #182; see `docs/Decisions.md` §30, §31.)
`docs/Decisions.md` §25's `leaderelection.Designated` is re-weighed against the deferred
ranked-over-Incus election once real membership exists to make the automated
option's cost worth paying.



### Debian and NixOS with Incus (was Phase 6, "Multi-OS hosts")

> **New phase (2026-10-26, #401, `docs/Decisions.md` §30, §15):** multi-OS
> moved here from "direction, not yet built" and `docs/Out of Scope.md`. Order:
> IncusOS (done) → Debian → NixOS → Talos. Debian and NixOS are Incus hosts,
> so the agent runs there and joins same-OS clusters. Talos isn't an Incus
> host: the agent can't run there, and it needs its own management path.

- [ ] Debian + Incus host, by §15's declared-disk path first (the operator
  names the target disk; no helper-OS round-trip). To file.
- [ ] NixOS + Incus host. Needs its own install-mechanism decision first. To
  file.
- [ ] SSH-CA host access to Debian and NixOS hosts, through the bastion
  (#399, deferred), which also decides the SSH CA (`docs/Decisions.md` §32);
  instances keep Incus's own websocket exec. To file.

**Done when:** a Debian host and a NixOS host are each provisioned and run an
agent joined to a same-OS cluster. (Talos, first listed here, is the main line
since §31: Phases 6 and 7.)


### Incus App management (was Phase 8, "App management (deferred)")

> **New phase (2026-10-26, #401, `docs/Decisions.md` §30):** until this phase,
> the agent is the only App managed. Generic App reconciliation, blue/green for
> other Apps, placement and stateful apps (§19) moved here from Phases 3 and
> 4, where §27 had paused them. Logging and metrics moved here from the old
> Phase 5, as one of the supported Apps.

- [ ] Per-node agents reconcile every `kind: App` fleet-wide with blue/green
  upgrades, from git-declared config. #92 is the umbrella; its original
  done-when holds: a per-node agent fleet whose designated primary deploys and
  upgrades the fleet (blue-green, fleet-wide, including its own self-upgrade
  with no reconciliation gap) from git-declared config, with a stale-checkout
  primary fencing itself out once a peer is on a newer commit (§25) — see #92
  - [ ] App renderer registry + fleet-wide reconcile algorithm; fleet
    reconciliation only ever runs while the caller's `Elector` says it may.
    Its zero-match branch for the agent App alone is #203 (Phase 3) —
    see #98
  - [ ] Human approval before blue-green promotion (manual promote mode per
    App) — see #340
  - [ ] Cluster-group placement: a `Target` field on `kind: App`, since Incus's
    scheduler stops being a trivial decision once members > 1 — see #182

**Done when:** #92's done-when above holds.

## Deferred — after the Talos main line

> **New section (2026-11-02, #466, `docs/Decisions.md` §31).**

- [ ] *(later)* iPXE for every boot, signed with the project's db key, the
  laptop serving a cold start after a full power cut, and boot files, configs
  and IPAM state synced from the cluster to the laptop. To file.
- [ ] *(later)* Enrolling Secure Boot keys during a network boot. To file.
- [ ] *(later)* DHCP handoff v2, with no gap, through a temporary Kea HA pair.
  To file.
- [ ] *(later)* KubeVirt, together with replicated storage (Longhorn or
  Rook-Ceph), since live migration needs shared storage. To file.
- [ ] *(later)* An own mkosi-built distro replacing Talos, gated on a written
  list of what Talos can't do; Kubernetes bootstrapped with `kubeadm`. To file.
- [ ] *(later)* The web app as a bastion (`docs/Decisions.md` §11, §30). A
  decision first — see #399
- [ ] *(later, optional)* Push encrypted store snapshots to off-cluster S3 —
  see #200

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
