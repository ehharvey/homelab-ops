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
> metrics." That content didn't disappear — it moved to the new Phase 4
> below, displaced by two pieces of infrastructure everything after it now
> builds on: node↔app connectivity, and a self-managing app-manager agent.
> See `docs/Decisions.md` for the full rationale (cert custody, why a local
> agent instead of the always-on app driving Incus directly, the WireGuard
> vs. Tailscale/NetBird seed-hook comparison).

- [x] Seed WireGuard connectivity between nodes and the web app: web app
  generates its own identity + a per-node keypair at seed-render time,
  embedded into `network.yaml` (no live enrollment step, unlike
  Tailscale/NetBird); resolves `docs/Decisions.md` §11: DONE; see #91
- [ ] Per-node app-manager agent, leader/follower HA: one agent instance
  per node, electing a single fleet-wide leader via an ETag-CAS lease
  stored in Incus itself (no new dependency) — the leader reconciles a new
  `kind: App` object across the whole fleet via a small renderer registry,
  proven by managing its own fleet (blue-green self-upgrade, driven
  fleet-wide by whichever agent is leader) — see #92,
  `docs/Decisions.md` § App Manager HA. Built in dependency order:
  - [ ] `kind: App` schema + store/configdiff plumbing — no runtime
    behaviour, pure parse/validate/store, prerequisite for every step
    below; `App` carries a cardinality field rather than a separate
    `kind: AgentConfig` (see `docs/AppClasses.md`) — see #97
  - [ ] `internal/leaderelection` — an ETag-conditional-write lease over a
    dedicated Incus project, giving the fleet one elected leader with no
    etcd/Consul/Raft — see #108
  - [ ] App renderer registry + fleet-wide reconcile algorithm; fleet
    reconciliation only ever runs while the caller holds the lease — see
    #98
  - [ ] Preseed the `incus-socket` profile onto every node
    unconditionally, since an agent now runs everywhere — see #99
  - [ ] Deploy the agent's first instance — web app route +
    `bootstrap deploy-agent` — see #100
  - [ ] `cmd/agent` binary tying election and the reconcile loop together;
    every node runs the same binary and election decides which is active
    each tick — see #101
  - [ ] Leader declines to renew its lease on self version mismatch, so a
    stale leader steps aside during a self-upgrade — see #109
  - [ ] Publish the agent image to GHCR; local registry for
    dev/validation — see #102
  - [ ] `scripts/validate/` script proving fleet-wide blue-green +
    leader failover end-to-end against #92's own done-when — see #103
- [ ] Tie `bootstrap`'s `flasher-tool` to the same pin as the web image, so
  the two cannot drift from one another — see #68
- [ ] Unified config: consolidate config passing into one place and fail
  fast on missing required values, rather than resolving it just-in-time
  across the codebase — see #67

**Done when:** a managed node has a persistent WireGuard tunnel to the web
app, and a per-node app-manager agent fleet that elects a single leader,
deploys and upgrades itself (blue-green, fleet-wide) from git-declared
config, and survives losing the specific agent instance currently holding
leadership — another already-running agent takes over within the lease
TTL with no reconciliation gap. (0.x provisions one physical node only,
so this proves the lease/election mechanism itself, not genuine
node-death fault tolerance — that needs real multi-member Incus
clustering, deferred; see `docs/Decisions.md` § App Manager HA.)

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

**Done when:** the suite is honest about its own results — a missing tool
reports as a skip rather than a failure, a script that silently gains a
precondition fails rather than passing quietly, and every script can say
what it proves and what it needs without being read. (Reached. What
remains is *enforcement* — nothing runs the suite automatically yet; that
work is tracked separately and is not part of this section.)

## Phase 4 — Tailscale, logging + metrics

- [ ] Accept an operator-supplied Tailscale authkey per instance; bake into seed via IncusOS's Tailscale service (blocked on upstream seed support — see #76, deferred)
- [x] Add a local Grafana + Loki + Prometheus dev stack under `docker-compose.yml`, so log/metric forwarding can be validated without live Grafana Cloud credentials (see #82)
- [ ] Stand up an Alloy Incus instance; point node syslog at it — now a renderer registered with Phase 3's app-manager agent (see #92) rather than built from scratch — see #77
- [ ] Alloy → Grafana forwarding confirmed end-to-end (local stack by default; Grafana Cloud is the real production destination, checked separately) — see #78
- [ ] Mint a per-instance `metrics`-typed Incus cert at seed-render time (shared `internal/cert`+`internal/seed` code, used by both the bootstrap CLI and the web app) and preseed it via `incus.yaml` for Alloy's local scrape — invisible to the operator, distinct from the break-glass client cert — see #79
- [ ] Extend the per-node Alloy instance to scrape Incus's native `/1.0/metrics` endpoint and remote_write to Grafana, alongside its existing syslog forwarding — see #80

**Done when:** a freshly provisioned node is reachable over Tailscale and both its logs and its resource metrics (host + instance, CPU/memory/disk/network) show up in Grafana Cloud.
