# Decisions — Homelab Ops Web App

Gaps and decisions surfaced while reviewing `Rough Notes.md`, grounded against the current IncusOS docs/source. Decided so far: backend in **Go**; where the app itself runs is still open.

## 0. Biggest one first: does this duplicate Operations Center?

FuturFusion (the IncusOS team) already ships **[Operations Center](https://linuxcontainers.org/incus-os/docs/main/reference/applications/operations-center/)** ([source](https://github.com/FuturFusion/operations-center)), an official app that:
- Generates pre-seeded IncusOS ISO/raw images and deployment tokens for bootstrapping new nodes
- Clusters nodes together and shows a resource inventory across clusters
- Authenticates its web UI/API via trusted client certificates (seeded in)
- Pushes IncusOS updates to managed nodes
- Runs as an IncusOS "application" (listens on :8443 by default)

That overlaps directly with notes 1, 5, 6, and part of 4. Before designing anything:
- Have you run Operations Center yet? Does it cover enough that this project should be a **thin layer on top of it** (config-from-GitHub + IPAM + Tailscale + logging glue) rather than reimplementing ISO generation / cert trust / clustering from scratch?
- Or is the goal specifically to *not* depend on it (e.g., simpler, single-binary, no extra cluster-management concepts)?
- If building on top: integrate via its REST API, or just reuse its seed/ISO conventions and stay separate?

This decision reshapes most of the rest of the doc, so it's worth resolving first.

### Answer
~~Yes, we should use Operations Center and thinly wrap it since it provides an API.~~ Superseded: 0.x does not depend on or wrap Operations Center — see `Architecture.md` § 0.x framing. Operations Center expects a trusted client cert in its own seed before it'll talk to anyone, which for node #0 doesn't remove the bootstrap problem, it just relocates it; and multi-node clustering (its main value-add) is out of scope for 0.x anyway. Revisit wrapping it once multi-node is actually on the table.

## 1. GitHub-sourced config (note 2)

- One repo or one-per-environment? Public, private (needs a deploy key/PAT), or both?
- Pull model: poll on an interval, manual "sync" button, or webhook-triggered?
- What's authoritative if the repo and the running state disagree — does the app reconcile (GitOps-style, like Flux/ArgoCD) or just diff-and-warn?
- Validation/dry-run before applying a pulled config to real hardware?
- Rollback story if a pulled change breaks a node?

### Answer
One repo per environment, public only for now (private/auth deferred); a later iteration could share one repo across environments via different branches/commits. The app just diffs-and-warns against last-synced state — no reconciliation, validation/dry-run, or rollback story yet.

## 2. Bare-metal instance definitions via YAML (note 3)

- What identifies a physical machine in the YAML — MAC address, serial/asset tag, a manually assigned name? How does the app match a booting machine to its definition?
- Does one YAML file = one machine, or one file describing the whole fleet?
- Does this schema double as (or wrap) an IncusOS install seed (`install.yaml`/`network.yaml`/`applications.yaml`), or is it a separate higher-level format the app translates into seeds?
- Multi-node clusters vs. independent single-node hosts — in scope for 0.x?


### Answers
MAC address identifies a machine. Format is k8s-style — objects discriminated by a `kind:` key, merged together — as a simpler, higher-level format the app translates into install seeds, not a wrapper around them. 0.x focuses on single-node hosts; multi-node is evaluated later.

**Correction (2026-07-15, see § App Manager HA below):** "single-node" here means the *fleet* 0.x actually provisions is one host — it does not mean Incus itself runs as a bare, non-clustered daemon. Incus is initialized as a **one-member cluster** from the start, precisely so the App Manager's leader-election/fleet-reconciliation design (which needs one Incus API surface reachable from every node) doesn't need a later migration. Growing to N members — and the operator workflow for adding them — is the part that's still deferred, not clustering itself.

## 3. USB installer generation (note 5)

- Generated via the **flasher-tool** (Go CLI, `go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool`) invoked by the app, or via Operations Center's built-in image generation (see §0)?
- ISO vs. raw `.img`? Per the docs, the ISO is *not* hybrid — if these need to boot from a USB stick, you need the `img` format, not `iso`. Worth confirming since the original note says "ISOs to download."
- Per-instance image means baking that machine's seed (cert, network, install target) into the image at generation time — where do generated images live, and how does the user get them onto a USB stick (web download + `dd`/Rufus, or does the app drive that step too)?
- Storage/cleanup policy for generated images (regenerate on demand vs. cache)?

### Answer
flasher-tool, for now — node #0 has no other way in. Raw `.img`, not ISO, since it needs to boot from USB. Support both download-for-later-`dd` and the app driving the flash itself, whichever's easiest to implement. Regenerate on demand; caching is a later optimization.

## 4. Client certificates (note 6)

- What are these certs *for* — trusting the app's own management UI, trusting it against each node's Incus API, or both? (Operations Center uses client certs purely for its own web UI/API auth.)
- Does the app run its own CA and issue/sign certs per node, or generate self-signed certs per machine?
- Where are private keys stored, and what's the rotation/revocation story if a node is decommissioned or a USB stick is lost?

### Answers
Certs are for initial Incus client authentication against IncusOS (not Operations Center auth). Self-signed is fine for now. Key storage and rotation/revocation are deferred — current focus is just one node.

### Follow-up (resolved on #36/#37): one break-glass cert per deployment, supplied by the operator, never held by the app

Worked out across #36/#37's implementation discussion: the cert is a single break-glass credential for the whole deployment, not one per node — Incus's trusted-client-cert list is already cluster-wide by construction, and minting a unique keypair per node bought no real isolation anyway, since custody would still be centralized in one place. Taken one step further than that discussion's first conclusion: the web app doesn't generate or persist this cert in *any* form, not even just the public half. The operator runs the existing `bootstrap gen-cert` once — the same step node #0 already requires — and points the web app at the resulting public cert via local deployment config (e.g. a `CLIENT_CERT_PATH` file path, mirroring `STORE_PATH`'s pattern); the web app reads and embeds that public cert into every node's seed and never sees, generates, or stores the private key.

This resolves both questions left open on #37's review thread: key custody (moot — the app holds no key, full stop) and whether node #0's cert is the same credential as the cluster's break-glass cert (yes, by construction — it's literally the same file pointed at by config, not a separately generated or imported copy). `internal/cert` itself is unchanged by this — it's still exactly the CLI-only, offline generator it always was; the web app simply never calls it.

## 5. IPAM (note 7)

- Scope for 0.x: just "assign a static IP per node" bookkeeping, or real subnet/VLAN modeling with conflict detection?
- Does the app *configure* the node's network (writing `network.yaml` into its seed) or only *record* the assignment for a human to apply elsewhere (e.g., on the router/DHCP server)?
- IPv6 in scope, or IPv4-only for now?
- Any integration with an existing DHCP/DNS setup (e.g., reserve the address there too), or does this app become the sole source of truth?

### Answers
Just assignment bookkeeping: track `Network` CIDRs to help with IP generation, with basic duplicate detection, and account for existing DHCP by tracking usable static-IP ranges outside the DHCP range. The app configures the node's network by writing `network.yaml`, not just recording it for a human to apply elsewhere. IPv4-only for now; the app is the sole source of truth, no DHCP/DNS write-back.

Implemented in `internal/ipam` (#35): operator-supplied `static_ip` is honored and validated against the network's CIDR and `dhcp_excluded_range`; when omitted, the app auto-assigns the next free IPv4 from `dhcp_excluded_range`, reusing the same instance's prior persisted address across re-syncs so it doesn't churn. Duplicates and out-of-range values are rejected per network (two different networks may reuse the same address), and the sync that produced them is hard-failed — including an explicit `static_ip` that collides with a *different* instance's prior-assigned address, which is rejected rather than silently relocating that instance to a new one. Full policy: `docs/Ipam.md`.

## 6. TPM / Secure Boot (note 10) — flagging a likely misunderstanding

IncusOS binds **disk encryption** to measured boot via TPM PCRs, and Secure Boot is what makes those measurements trustworthy ([security model docs](https://linuxcontainers.org/incus-os/docs/main/reference/security/)). Concretely:
- With TPM present + Secure Boot on: disk encryption keys are bound to a verified boot chain.
- With TPM **missing** (`security.missing_tpm: true` in the install seed): falls back to a software TPM, which the docs call out explicitly as weakening security — encryption only protects against a stolen *powered-off* disk, not a booted/tampered system.
- With Secure Boot missing/disabled: falls back to a weaker PCR (4 instead of 7), losing trust in the boot chain.



"Disable TPM, but allow Secure Boot" is a real supported combination, but it mostly throws away the disk-encryption guarantee while keeping the (now less meaningful) Secure Boot check. Is that intentional — e.g., hardware in this homelab genuinely lacks a TPM and this is an accepted tradeoff — or was the intent closer to "don't *require* Secure Boot, but keep TPM" (i.e. `missing_secure_boot: true` instead)?

### Answers
Both TPM presence and Secure Boot are configurable per-instance, since homelab hardware genuinely may lack a TPM and that tradeoff is accepted.

## 7. Remote logging to Grafana / Tailscale (note 8)

- IncusOS's documented seed files (`install`, `network`, `applications`, `incus`, `kernel`, `migration-manager`, `operations-center`, `provider`, `update`) don't include a logging/syslog config — there's no obvious native "ship journald to X" knob on the minimal host OS itself.
- So: does log shipping happen from the **Incus host OS** (would need e.g. `systemd-journal-remote` or a syslog forwarder, if IncusOS even exposes that), or only from **workloads running inside Incus** (a Loki/Promtail/Alloy agent deployed as one of the managed instances)? These are very different integration points.
- Tailscale-accessible endpoint vs. directly reachable Grafana — does that mean Grafana itself sits on the tailnet, or is this app generating a Tailscale Funnel/serve config for it?

### Answers
Run Alloy as an Incus instance; configure IncusOS's remote syslog to point at it, and Alloy forwards to Grafana Cloud. Whether Grafana itself sits on the tailnet is TBD later.

## 8. Tailscale (note 11)

- Installed on every IncusOS node, only on whatever runs the web app, or both?
- As an IncusOS "application" container/instance, or some other mechanism (IncusOS being a minimal/immutable host limits options here)?
- Auth key provisioning: pre-generated reusable key baked into seeds (simpler, but a standing credential in every image) vs. some per-node enrollment flow?

### Answers
Use IncusOS's built-in [Tailscale service](https://linuxcontainers.org/incus-os/docs/main/reference/services/tailscale/). For now the app just takes an operator-supplied authkey per node; a real enrollment flow is a later expansion.

### Addendum (2026-07-03)
As of this writing, IncusOS's built-in Tailscale service has no seed-time
configuration hook: none of the 10 documented seed files (`install`,
`network`, `applications`, `incus`, `kernel`, `migration-manager`,
`operations-center`, `provider`, `security`, `update` — checked against the
vendored schema, the pinned `third_party/incus-os` submodule commit, and
upstream `main`) carry Tailscale config; `ServiceTailscaleConfig.AuthKey` is
only settable via a post-boot REST call to the node's incus-osd API. No
upstream issue/PR proposes adding a seed hook; the closest thread is
[lxc/incus-os#497](https://github.com/lxc/incus-os/issues/497), where this is
an open question. Implementation is deferred pending upstream clarity —
tracked as issue #76 with the `later` label rather than built against a
seed file incus-osd won't yet read.

## 9. Where does the web app itself run?

**Current rule: on its own appliance machine, outside the cluster: a Docker box first, an IncusOS turnkey host later. See §29.** The options and first answer below are kept as the record. (Originally flagged as open.)

- Inside the homelab it manages (a container/VM on one Incus host) — needs a bootstrap path for node #1 before the app exists to generate its installer.
- Or a separate always-on box outside the cluster — avoids the chicken-and-egg problem, adds "one more thing to maintain" outside the managed fleet.
- This also interacts with §0: if Operations Center is the bootstrapper, does *this* app only need to exist after the first node is already up?

### Answers
~~Dev environment on local K8s cluster~~ — superseded by #18: no k8s dependency. Dev uses Docker Compose; deployment targets are a Docker image and a plain binary. Later, a migration path to running inside the IncusOS-managed fleet itself is wanted (see `Architecture.md` § Web app).

### History

- **2026-10-03 (#247).** Answered by §29: the web app runs on an appliance machine (an internet VM or a LAN machine), from one published image. Moving into the fleet is §29's migration path (#245, #246).

## 10. Single disk / single NIC default, configurable (note 9)

- Confirms this maps to `install.yaml`'s `target` (disk selection) and `network.yaml` (interface config) per machine — does the per-instance YAML (§2) need to support arbitrary multi-disk/multi-NIC overrides in 0.x, or just the single/single default for now with multi as a documented future case?

### Answers
Maps to `install.yaml`'s disk `target` and `network.yaml`'s interface config. Single disk/single NIC is the 0.x default; multi-NIC is a documented future case, not built now. `Network` config needs to cover DHCP on/off, DNS source, and static IPs.

## 11. Networking between nodes and the web app

We want to avoid putting Incus nodes on the public internet, so nodes need some way to reach the web app — out of scope for Phase 1, unresolved beyond brainstorming three options:
1. Nodes run a management container that polls the web app, optionally upgrading to a websocket for subscriptions.
2. The web app also runs Tailscale, so it connects to nodes directly.
3. Both web app and nodes connect via WireGuard.

### Answer
**Resolved 2026-07 by #91: option 3, WireGuard.** The web app generates and
persists its own WireGuard identity on first run; `internal/seed.Render`
mints a fresh per-node keypair at seed-render time and embeds it (plus a
peer entry pointing at the web app) into that node's `network.yaml` — no
live enrollment step, unlike option 2 (Tailscale), whose seed-time
configuration hook doesn't exist yet (§8's addendum). Option 1 (a polling
management container) is superseded rather than built — see § Web app's
WireGuard tunnel module in `Architecture.md` for the resulting mechanism,
and the "Future direction" subsection immediately below for what this
tunnel unlocks next.



#### Considered and rejected for 0.x: an end-to-end-encrypted command bus

A more ambitious version of option 1 came up: make the internet-facing web app a *zero-knowledge relay* — nodes poll it, browser clients push encrypted commands, the server only ever stores ciphertext (per-node keypairs, signed payloads, replay windows, browser-held keys, ciphertext in Postgres). Rejected for 0.x:

- **It's a control plane; 0.x deliberately isn't one.** Config sync is diff-and-warn only — no apply, no reconciliation (§1). An encrypted command queue is the opposite of that decision.
- **E2EE's value proposition is moot here.** 0.x is single-user with no auth (see `Out of Scope.md`). Zero-knowledge encryption protects data from an untrusted server operator across multiple clients; with one user running their own server, it encrypts data from yourself.
- **The transport story already covers it.** The decided posture is keeping nodes off the public internet and reaching them over Tailscale/WireGuard (§8, options 2–3 above), which already gives confidentiality + node identity without a bespoke crypto protocol.
- **Wrong stack / against conventions.** It assumes Postgres and a custom Ed25519/AES-GCM protocol; the app is pure-Go sqlite, single-binary, distroless (§9), and conventions favour stdlib over a bespoke security-critical subsystem.
- **The real "command a node" path already exists.** The app issues each node a client cert and talks to its Incus API directly (§4) — authenticated by Incus itself. A second, parallel encrypted command bus would duplicate that and reintroduce the trusted control plane 0.x avoids.

Worth keeping from the discussion: prefer node-initiated outbound (option 1), and if a node ever does send actions, send *structured actions*, not shell commands. Revisit the whole idea only if multi-user or an untrusted-relay requirement ever materialises.

#### Future direction (not designed or built): web app as a zero-knowledge network proxy

Raised 2026-07-14 alongside the WireGuard phone-home work (see the Phase 3
rejig in `docs/Roadmap.md`): once the web app has a persistent WireGuard
tunnel to every managed node, it could also act as a transparent network
*relay* between an operator and a node's Incus API — letting an operator
reach a node without exposing it directly to the internet, without the web
app itself needing to understand or terminate the traffic it's relaying.

This is related to, but distinct from, the "end-to-end-encrypted command
bus" addendum immediately above, and doesn't reopen that rejection:
- That addendum was about a *command queue* — a control plane storing and
  later delivering structured commands, which conflicts with 0.x's
  diff-and-warn-only posture (§1).
- This idea is a *transparent relay* — packets in, packets out, no storage,
  no command semantics — sitting on top of the WireGuard tunnel once it
  exists, closer to a bastion/jump host than a control plane.

**Expected sequencing:** after Phase 4 (`docs/Roadmap.md`), once WireGuard
node connectivity (Phase 3) and logging/metrics (Phase 4) are both in
place. Not designed or built now — captured here so the shape of the
options survives until then.

**Options considered so far (2026-07-14 discussion), none designed/built:**

1. **Single shared WireGuard mesh** — web app is the hub, operator's
   browser embeds a WireGuard client (e.g. a WASM implementation, since
   browsers have no raw-socket access — a real, non-trivial dependency) and
   joins the same mesh as the nodes. Operator authenticates to the node's
   Incus API with their own client cert; the web app never gets that
   private key. But the web app *is* a WireGuard endpoint on both hops, so
   it necessarily decrypts to plaintext IP packets in the middle to
   re-route between them — "zero-knowledge" here holds only because of the
   inner Incus mTLS, not because of WireGuard. Worth being explicit that
   this is what "zero-knowledge" would mean in this design, rather than
   assuming the WireGuard layer itself hides anything from the app.

2. **Nested/second WireGuard mesh** — operator and node share an inner
   WireGuard network that the web app has no keys for and is never a peer
   in; the web app just forwards opaque ciphertext (UDP) between the two,
   blind to its contents. This is genuine network-layer zero-knowledge
   (not just mTLS-dependent, as in option 1), and is the right shape *if*
   the ambition is general operator access to arbitrary node-side services/
   ports, not just the Incus API — an app-layer relay (option 3) doesn't
   generalize to that. More moving parts than option 3: a second keypair/
   config per node, and the web app still needs to broker addresses since
   both sides likely still route through it as a relay for the
   ciphertext-forwarding itself.

3. **Plain TCP/L4 relay over the existing Phase 3 tunnel (recommended if
   scope stays "reach one node's Incus API")** — no WireGuard in the
   browser at all. The operator already has an authenticated HTTPS session
   to the web app; the app does a CONNECT-style relay of that byte stream
   to the node's Incus port over the WireGuard tunnel it already holds from
   Phase 3, passing the operator's client-cert TLS handshake through
   untouched rather than terminating it. Gets the property that actually
   matters (web app never holds/needs the operator's private key, never
   sees decrypted Incus traffic) with the least new infrastructure — no
   browser-side WireGuard stack, no nested mesh, no NAT-traversal problem.

4. **Web app as a Tailscale-style coordinator, operator and node connect
   directly (rejected as a default approach)** — the web app hands each
   side the other's WireGuard public key + candidate endpoints and steps
   out of the data path once a direct UDP path forms, the way Tailscale's
   control plane (or DERP as fallback) works. Rejected as the thing to
   build ourselves: the entire reason nodes phone home to the web app
   instead of being dialed directly is that a homelab node sits behind a
   NAT/firewall with no public IP (`Architecture.md` § Incus Node
   networking to Web App) — coordinating a "direct" connection doesn't
   remove that constraint, it just attempts to hole-punch through the same
   NAT, which fails outright for symmetric NATs (common on corporate
   networks/some mobile carriers — exactly where a mobile operator is
   likely to be connecting from). A robust version still needs a relay
   fallback for when hole-punching fails, i.e. still needs option 3's relay
   *plus* new STUN-like endpoint discovery, NAT-type detection, and
   keepalive/session machinery on top — strictly more engineering than
   option 3 alone, for a benefit (web app off the data path) that's
   opportunistic rather than guaranteed. This is also a substantial
   reimplementation of what Tailscale/headscale already do, cutting against
   this project's established bias against rebuilding things Tailscale
   already solved (§0, §8).

   Worth reconsidering later, not as a hand-rolled coordinator but as
   **live Tailscale enrollment for this one hop**: §8's rejection of
   Tailscale for node connectivity was specifically that its authkey has no
   *seed-time* config hook (§8 addendum, upstream issue #76) — a
   bootstrapping constraint, not a runtime one. Phase 3's local app-manager
   agent (`docs/Roadmap.md`) reconciles config live, post-boot, and could
   enroll a node into Tailscale after the fact with no seed hook needed —
   getting Tailscale's real, battle-tested coordination + DERP relay
   fallback for operator access specifically, while the WireGuard seed
   tunnel stays as the install-time bootstrap mechanism it actually solves.
   Two tunnels for two different lifecycles, rather than one mechanism
   trying to do both.

**Current lean:** option 3 (plain relay) if scope stays Incus-API-only;
revisit option 4's Tailscale-enrollment variant if broader operator access
to arbitrary node services becomes a real requirement. Avoid option 4's
hand-rolled-coordinator form and option 2 unless option 3 demonstrably
doesn't cover the need.


## 12. Persisting IPAM state

Since the app is the source of truth for IP assignments, it needs to persist that state somewhere. Options:
1. Make the store durable. `store.Open()` already takes a path (not just `:memory:`) — point it at a real file and treat the store as the system of record for what's actually been handed out, while git stays the source of truth for desired config. Simplest change, but it means the store now needs a backup/migration story it doesn't have today (`docs/Architecture.md`'s "Instance/network store" paragraph currently frames it as a disposable, `:memory:`-by-default cache rebuilt every sync).
2. Write auto-assigned IPs back into the synced git repo (commit the resolved `static_ip` once assigned) so git alone remains authoritative and the store can stay disposable. More faithful to the existing "app is sole source of truth via git" framing, but adds a write-back-to-git capability the app doesn't have yet, and raises its own questions (commit as the app's own bot identity? race if two requests assign concurrently?).

### Answer
Make the store durable — point `store.Open()` at a real file instead of `:memory:`, and treat it as the system of record for assigned IPs while git stays authoritative for desired config. Git write-back is deferred to a later iteration.

## 13. Validation approach

Surfaced by #46 (no `Network`-level validation) and tracked as the decision #47. Work on #35 (IPAM) left validation scattered and stringly-typed: `config.Network`/`config.Instance` hold every field (`cidr`, `gateway`, `static_ip`, `dhcp_excluded_range`, `dns`, `mac`) as a `string`; `config.Parse` does structural validation only (strict unknown-field detection), no semantics. The semantic checks that exist are duplicated and re-parsed — `internal/ipam` builds a throwaway validated representation (`*net.IPNet`/`net.IP` bounds) out of the strings, and `internal/seed` re-parses the same strings to re-check static-IP-in-CIDR and the gateway again — while `Network` has no validation entrypoint at all (gateway-is-an-IP, gateway-∈-CIDR, name-non-empty, DNS-are-IPs, range-∈-CIDR all go unchecked).

### Options considered

1. **`go-playground/validator`** (combined parse/validate, struct tags). Rejected: the rules that matter here are cross-field (gateway-∈-CIDR, range-∈-CIDR, static-ip-∈-CIDR-and-∈-range), which its tags can't express — you register custom validators and write the `ParseCIDR().Contains()` logic by hand anyway, losing the declarative payoff. It also leaves fields as `string`, so the re-parsing duplication survives, and it adds a reflection-heavy dependency tree against the repo's stdlib-first convention.
2. **`zog`** (separate parse/validate, schema DSL). Right instinct (the parse/validate split), wrong dependency: it's a pre-1.0, single-maintainer library whose API churns — exactly what the "don't depend on unstable public APIs" note in `Development Conventions.md` warns against (cf. the cert decision citing incus's own v6→v7 bump). It's built for HTTP form/JSON request validation, still doesn't know "IP ∈ CIDR" (you write custom refinements regardless), and you hand-wire the typed target struct yourself — so the library mostly donates an issue-list datatype we can write in ~15 lines.

### Answer

**Roll our own, as a stdlib `net/netip` parse/validate split.** This keeps the good idea from each rejected option without the cost: zog's parse/validate separation and validator's centralized rules, using `net/netip`'s built-in `encoding.TextUnmarshaler` for the syntactic layer and a small hand-rolled validator for the cross-field semantics no library does for free here anyway. Consistent with every prior dependency decision in the repo (cert, YAML, sqlite, go-git) — no new dependency.

- `config` fields become typed (`cidr → netip.Prefix`, `gateway`/`static_ip` → `netip.Addr`, `dns → []netip.Addr`, `dhcp_excluded_range →` a small range type; `mac` stays a string). Verified that `yaml.v3` honors `encoding.TextUnmarshaler`, so these parse straight from the synced YAML, fail on malformed input at parse time, and model optional fields cleanly via the zero value (`IsValid()==false`, no error on empty or omitted).
- The validated representation then *is* the parsed representation: `internal/ipam` and `internal/seed` stop re-parsing strings, and #46's rules become one-line `Prefix.Contains(...)` checks in a single `config.Validate` pass.
- **Leaving room without paying for it:** validation returns a stable `Issue{Path, Message}` value (the same shape zog's `issue.Path`/`issue.Message` produces). If an untrusted-input surface ever appears, a validation library can sit at *that* boundary producing the same `Issue` shape, without touching the git-sync path. No `Validator` abstraction/registry is built now — the value-returning function is the entire seam.
- **When this gets revisited:** the trigger is the first write/create API endpoint that accepts a *request body* to author a `Network`/`Instance` (vs. the name-keyed action routes planned for Phase 2). The roadmap (Phases 0–3) and `Out of Scope.md` put no such surface in 0.x — single-user, no auth, diff-and-warn from git, schema count stays at 2. And per `Out of Scope.md` §13 the likely growth path is JSON Schema *generated from* `internal/config`'s structs, which the typed structs serve directly — so the more probable future is codegen-from-structs, not adopting zog.

Implementation (the `netip` type rework + `config.Validate` pass, which also closes #46) lands under #46; dev-facing conventions for it are in `Development Conventions.md` § Config validation.

## 14. Metrics (Phase 3 scope addition)

Phase 3 was originally scoped as Tailscale + logging only (§7 above). Folding in node/infra resource metrics (host + Incus-instance CPU/memory/disk/network) alongside logging raised three questions: where do metrics come from, how do they get shipped, and how does the shipping agent authenticate.

### Source

Incus already exposes a native Prometheus-format metrics endpoint (`/1.0/metrics`, host + per-instance stats) — confirmed in the vendored `lxc/incus/v7` module this repo already depends on. No separate exporter (e.g. `node_exporter`) is needed: IncusOS's minimal/immutable host has no natural home for a second agent, and Incus already exposes this data for free. Host stats Incus doesn't expose (e.g. disk usage outside its storage pools) are out of scope for now; revisit only if a concrete gap shows up.

### Delivery

The same per-node Alloy instance already planned for syslog forwarding (§7) also handles metrics — one Alloy config with both a syslog receiver and a `prometheus.scrape` + `prometheus.remote_write` pair, rather than a second agent/instance per node.

### Cert custody for Alloy's scrape

Alloy needs to *present* a client cert to Incus's API when scraping `/1.0/metrics`, unlike the break-glass cert (§4), which the app only ever embeds the public half of. Incus supports a dedicated `metrics` certificate type (`api.CertificateTypeMetrics`) that's restricted to read-only `/1.0/metrics` access — nothing else on the Incus API — preseeded via the same `incus.yaml`/`InitPreseed` mechanism `internal/seed` already uses for the break-glass cert.

**Answer:** this cert is minted invisibly, per instance, at seed-render time — the operator never sees or manages it. Whichever tool renders the seed (bootstrap CLI for node #0, web app for every later node) mints a fresh keypair via the existing `internal/cert.Generate` (already offline/no-network, so this works identically before or after any node exists), embeds the public half into that instance's `incus.yaml` as a `metrics`-typed trusted cert, and bakes the private half into the same instance's Alloy provisioning data. `internal/seed`'s `renderIncusPreseed` already builds `InitPreseed.Certificates` as a slice, so adding this second entry is additive.

This is a deliberate, narrow exception to the break-glass cert's "the app never mints certs" precedent (§4): that rule exists to avoid centralizing custody of a *standing, full-access* credential. A per-instance, read-only-metrics keypair minted and immediately handed off to the same instance carries none of that risk, so there's no reason to push a manual step onto the operator for it. The operator's only remaining observability-related config surface is *where to ship logs/metrics* (destination endpoint + auth, see below), not certificate handling.

### Destination: fan-out to multiple operator-configured endpoints

Alloy's log/metric shipping is not a single hardcoded destination. Both of Alloy's relevant components support this natively — `loki.write` and `prometheus.remote_write` each accept multiple `endpoint { url = ... }` blocks in one component instance — so Alloy fans out to however many destinations are configured, each with its own URL + auth, the same way `CLIENT_CERT_PATH`/`STORE_PATH` already are operator-configured. Grafana Cloud is hosted Loki + Mimir behind exactly those two APIs, so it's simply the expected default destination, not a special-cased one; the same mechanism works against any Loki-/remote_write-compatible endpoint, including a self-hosted Grafana stack — and against more than one at a time.

This means "local Grafana instance" and "Grafana Cloud" aren't an either/or choice: an operator can run both simultaneously (e.g. always mirror to a local Grafana instance for fast local queries/debugging, while also shipping to Grafana Cloud as the durable production destination), point at multiple cloud accounts/environments, or configure just one — whatever the deployment needs. The exact shape of the "list of destinations" config (repeated env vars vs. a small typed list) is left to whichever mechanism #67 (Unified config / viper) lands on, rather than inventing a bespoke env-var scheme now that config consolidation would immediately have to replace.

**Local dev/test stack:** requiring live Grafana Cloud credentials for every `scripts/validate/` run (or every dev loop) is exactly the friction the repo's other dev fixtures avoid (`dev/git-fixture` for config sync). So a local stack — Grafana + Loki + Prometheus (with `--web.enable-remote-write-receiver`) under `docker-compose.yml`, mirroring the existing `web`/`config-repo` services — is always at least one of the configured destinations for `scripts/validate/`'s Phase 3 checks: no cloud credentials needed for that destination, fully automatable, torn down after each run. A real Grafana Cloud check (with the local stack configured as an *additional* destination, or on its own) remains available as a separate, credential-gated step (same gate-and-skip pattern as `INCUSOS_BASE_IMAGE`/`BASE_IMAGE_PATH` for real-VM tests) rather than the thing CI/every contributor has to run.

## 15. Multi-OS support (Debian, Talos) — direction, not yet built

Explored 2026-07-03: the long-term plan is to manage more than IncusOS
nodes. Two targets came up; neither is built, and both are blocked on
prerequisite work below. Captured here so the reasoning survives even
though `Out of Scope.md` is where this currently lives.

### Debian + Incus

Same managed workload as IncusOS (Incus), so `config.Instance`/
`config.Network`, IPAM, config sync, the store, and the break-glass-cert
model (`internal/seed`'s `renderIncusPreseed` → `InitPreseed`) all carry
over unchanged — `incus admin init --preseed` on Debian accepts the
identical struct. The open question was the install mechanism:

- **cloud-init/NoCloud**: works fine on bare metal (NoCloud just needs a
  `cidata`-labeled volume; Debian's `generic` cloud image + `dd` +
  `growpart` handles the rest) — but gives a plain unencrypted root
  filesystem. Since `config.Instance.Security.{TPM,SecureBoot}` is a real,
  decided-on-purpose feature for IncusOS instances (§6), silently ignoring
  those same fields for Debian instances would be a correctness gap, not a
  smaller feature set.
- **systemd-repart** (chosen direction): a small `mkosi`-built helper Linux
  environment boots off USB, runs `systemd-repart` against the target disk
  from a rendered `repart.d/*.conf` (the Debian analog of `install.yaml`),
  which supports `Encrypt=tpm2` for real LUKS2+TPM2-bound unlock at install
  time and `CopyFiles=` to populate the partition from a
  `debootstrap`/squashfs rootfs source. Secure Boot is handled by Debian's
  `shim-signed` package (Microsoft-signed shim + signed GRUB via normal
  `apt`), not a from-scratch signing problem. This mirrors IncusOS's own
  build tooling — confirmed via IncusOS's docs that it's built with
  `mkosi`/`ukify` with TPM PCR binding via `systemd-measure` — so it's the
  same tooling family, not a detour.
- **mkosi-built target image**: rejected as the *target* rootfs mechanism
  (most work, least reuse of Debian's official images) — but mkosi is
  still the right tool for building the *helper* boot environment itself.

### Helper OS: pre-install hardware registration/confirmation

Rather than only reporting hardware post-install (see `Out of Scope.md`'s
existing phone-home/manifest item, which is a one-shot post-install ping),
the systemd-repart helper OS is also the natural place to solve bare-metal
identification: it boots pre-install, reports disk inventory / machine-id /
NICs to the web app, and the operator confirms (which `Instance`
definition, which target disk) before the helper OS proceeds to run
`systemd-repart`. This is an interactive, blocking round-trip, not a
fire-and-forget ping — distinct enough from the existing phone-home item to
warrant its own line in `Out of Scope.md`. It also relaxes the current
MAC-only identification model (§2): useful once a machine has more than one
candidate disk or MAC isn't known in advance.

### Talos

Talos nodes aren't Incus hosts — they're Kubernetes nodes — so
`applications: [incus]`, the break-glass-cert-into-Incus trust model, and
Alloy-as-an-Incus-instance for observability don't transfer. Talos has its
own machine-config format (one YAML, a reasonable analog to today's seed)
and its own PKI bootstrap (`talosctl gen config`), implemented in parallel
rather than reused. Phased the same way Incus multi-node itself is (§0,
`Architecture.md`'s 0.x framing): single-node install first; full cluster
lifecycle (control-plane/worker roles, `talosctl bootstrap`, etcd quorum)
deferred further behind that, mirroring the existing Operations Center
deferral.

### Prerequisites (why this isn't buildable yet)

1. **OS-target abstraction.** `internal/seed.Render` and `config.Instance`
   currently hard-assume IncusOS's exact 4-file seed bundle
   (`supportedApplications = map[string]bool{"incus": true}`,
   single-disk/single-NIC-only checks). Before any second OS backend
   lands, this needs a discriminator (e.g. an `os`/`target` field on
   `Instance`) and a renderer-selection seam — one `Render`/image-build
   implementation per OS behind a shared interface.
2. **Node → web app networking**, currently fully unresolved
   (`Architecture.md` § "Incus Node networking to Web App", `Decisions.md`
   §11). The helper-OS registration/confirmation flow *requires* this — a
   helper OS reporting hardware back to the app pre-install is exactly the
   "node talks back to the app" problem §11 defers. This has to be decided
   before the helper-OS flow is buildable at all, not just nice-to-have.
   Phase 3's Tailscale work may end up supplying the answer — §11 already
   lists "web app also runs Tailscale" as a live option, and it's the same
   transport the existing phone-home-over-Tailscale idea (Other notes #2)
   would use.
3. Phase 3 (Tailscale/logging/metrics) is already in progress and likely
   lands first regardless of this direction.
4. A smaller intermediate milestone is worth considering before the full
   confirmation-flow UX: a "declared-disk" Debian+Incus path (operator
   pre-specifies the target disk in config, no helper-OS round-trip) as a
   stepping stone — proves systemd-repart + cert/network seed rendering
   end-to-end before adding the networking-dependent confirmation flow on
   top.

## 16. `internal/incuslocal` vs. `internal/nodeprovision`: accepted duplication (#92)

#92's local app-manager agent needs an Incus API client over its own host's
forwarded unix socket (`internal/incuslocal`) — conceptually the same
create-instance/wait-for-operation/decode-response shape `internal/
nodeprovision` already implements for the web app's TLS-over-WireGuard-tunnel
path. Should these share a common low-level helper now, or duplicate?

### Options considered

1. **Extract a shared low-level helper now** (e.g. `internal/incusapi`) that
   both `nodeprovision` and `incuslocal` wrap, parameterized only by
   transport (`*http.Client` construction) and auth (TLS client cert vs.
   none). `nodeprovision`'s `createInstance`/`waitOperation`/`do` are already
   three standalone, non-tangled unexported functions, so this would be a
   small extraction, not a rewrite.
2. **Accept the duplication for v1.** The two clients have genuinely
   different transports (TLS-over-tunnel-dial vs. plain-unix-socket, no TLS
   at all) and there are only two consumers today.

### Answer

Accept the duplication for v1 (option 2) — consistent with this repo's
general anti-premature-abstraction bias (cf. §13's rejection of a validation
library before a second consumer existed). Two consumers with genuinely
different transports don't yet justify a shared abstraction; the
create/wait/decode logic is small enough (a few dozen lines) that copying it
once costs less than the wrong abstraction would.

**When this gets revisited:** a third Incus-API-consuming package appears —
the concrete candidate is #77's future Alloy renderer wanting to scrape
`/1.0/metrics`. At that point, extract the shared create/wait/response-
envelope-decode logic (already three standalone functions in
`nodeprovision`) into a common low-level helper both `nodeprovision` and
`incuslocal` wrap, rather than adding a third copy.

## 17. App Manager HA — leader/follower via Incus-native lease (#92)

> **Superseded in part — see §25.** The Incus ETag-CAS lease below (the
> coordination project, the lease object, renewal at 1/3 TTL, "stop renewing
> on self version mismatch") was replaced after #160 measured that Incus's
> `If-Match` is not a compare-and-swap. Leader election is now a pluggable
> interface, shipping operator-designated leadership first. Everything else
> here — one leader, leader-only fleet reconciliation, self-recognition,
> blue-green — stands.

#98's original design ran exactly one app-manager agent for the whole
0.x fleet (trivially "the one node" today), reconciling only that node's
own Apps, with no leader concept — safety came from partitioning
(`App.Node == nodeName`), not coordination. That leaves no HA story: if
the one node running the agent goes down, nothing takes over, because
Incus has no mechanism to relocate a stateless instance onto a healthy
node without shared storage (ceph) backing it.

### Decision

Run an agent on **every** node; exactly one is ever active (the leader),
elected via an atomically-updated object stored in Incus itself, not a new
dependency (etcd/Consul/etc.) or a new datastore.

**The agent is not declared as a `kind: App` at all.** An earlier revision
of this decision had the operator author one `App{type: agent, node: <n>}`
entry per node — wrong on two counts: it puts a per-node bookkeeping burden
on the operator for something that should just always be true of every
node, and it overloads `App.Node` (meant to mean "the operator's placement
choice") with something that's really "wherever the fleet's nodes are."
Instead: a new singleton `kind: AgentConfig` document declares the agent's
desired image once, fleet-wide; the leader synthesizes one `App`-shaped
value per known `Instance` from it at reconcile time (never parsed from
git as a literal document) and feeds those through the exact same
`Renderer`/blue-green machinery as any real App. Adding a node to the
fleet is enough to get an agent on it — nothing agent-specific needs
authoring per node.

> **Superseded — see "Follow-up: cardinality replaces `kind: AgentConfig`"
> at the end of this section.** The reasoning above stands, but its
> conclusion doesn't: the per-node shape isn't agent-specific, so it became
> `replicas: per-node` on an ordinary `kind: App` and `AgentConfig` was
> dropped before either was built. The rest of this section is unaffected —
> read "the agent's synthesized App" for "`AgentConfig`" throughout.

**This also removes `Node` from `kind: App` entirely** (not just for the
agent): 0.x has no real placement logic yet, and pretending otherwise with
a name-the-node field was premature. `Desired()` now builds every App's
`InstancesPost` with no `Target`, so Incus's own scheduler decides — moot
today (single-member cluster), and cheaper than building placement now
just to throw it away later. The anticipated real mechanism, when
placement actually matters (heterogeneous multi-member clusters), is
Incus's own **cluster groups** — tag members with a capability, target
creation at the group — not an operator naming individual nodes. See
`docs/Out of Scope.md`.

- **Coordination project.** A dedicated Incus project (e.g.
  `homelab-ops-meta`) holds a single never-started, config-bearing
  instance — the lease below, and nothing else (see "no desired-version
  object" further down) — the same "tag an Incus object, don't keep a
  separate store" philosophy #98 already uses for App generations,
  extended to fleet-wide coordination state.
- **Leader lease.** One object holds `owner`, `expiry`, and a monotonic
  `term` (fencing counter, bumped on each new acquisition). Every agent
  attempts to acquire-or-renew it every tick via Incus's ETag/`If-Match`
  conditional-write support — a CAS write either lands or it doesn't, so
  "am I leader" must always be re-derived from the last successful write,
  never from a locally cached flag (protects against clock skew or a
  GC-style pause making two processes briefly believe they're leader).
  The leader renews well before expiry (e.g. at 1/3 of the lease TTL)
  specifically to make losing the lease to a false expiry a non-event
  under normal operation.
- **Exactly one leader, deliberately** — no multi-leader/sharded variant
  was considered worth the complexity here; partitioning work across
  multiple simultaneous leaders reintroduces the coordination problem this
  design exists to avoid, for a scale this project doesn't operate at.
- **Fleet-wide reconciliation is leader-only.** `ReconcileNode` (per #98)
  generalizes to `ReconcileFleet`: the leader iterates every declared App
  (no `Node` filter — see above) plus every synthesized per-node agent
  App, running the same per-App blue-green state machine #98 already
  designed, using cluster-wide Incus reach instead of a single node-local
  socket. Followers do not reconcile anything — they only compete for the
  lease and keep their own instance's heartbeat alive (so `Healthy()`
  still works whenever the leader evaluates that instance during a
  blue-green transition). This is not a new liveness/watchdog concept:
  ordinary instance disappearance is already covered by `ReconcileFleet`'s
  existing zero-match → recreate branch, and Incus's own restart policy is
  trusted for day-to-day process liveness inside an already-converged
  instance.
- **Incus state stays authoritative; the coordination project is a hint,
  and it's just the lease.** Consistent with #98's existing invariant ("no
  distributed lock, always re-derivable from `incus list` alone") — the
  lease exists to make election possible, not to become a second source of
  truth competing with live Incus state or git. There is deliberately no
  second "desired version" object (see next bullet): once only the leader
  ever acts, nothing else needs a mirror of git's declared version to read.
- **Fleet-wide blue-green upgrade, unified with normal reconciliation —
  no separate object needed.** The leader detects its own staleness the
  *same generic way* it detects any App's version bump: its own
  synthesized agent App's live image tag vs. `AgentConfig.Image`, freshly
  read from its own git sync each tick. (An earlier revision of this
  decision had a leader-written `desired-version` object in the
  coordination project for this — dropped once "every agent watches it and
  self-replaces" was already rejected in favor of leader-driven creation
  fleet-wide: with no other reader left, the object had no purpose beyond
  what the leader's own git-synced config already gives it directly.) The
  leader creates every generation transition fleet-wide, including its own
  replacement, as part of one ordinary `ReconcileFleet` pass; a follower
  that crashes without a replacement is still covered, since nothing
  depends on a follower noticing its own staleness. Once the leader sees
  its own image is stale, it **stops renewing** the lease rather than
  voluntarily stepping down (self-recognition-rule-safe: it still never
  deletes itself). Only an already-upgraded agent should attempt the next
  acquisition; the new leader's first `ReconcileFleet` pass is what cleans
  up old-generation agents fleet-wide — no separate cleanup mechanism.

### Scope correction this forces: 0.x runs Incus as a single-member cluster

The mechanism above needs one Incus API surface reachable from every
node's agent — which only real Incus clustering provides (any member can
target any other member; the lease/version objects live in the cluster's
shared control-plane, reachable identically from wherever the leader
happens to be running). §2 and `Architecture.md`/`Out of Scope.md`
previously framed multi-node as wholesale deferred; that's corrected to:
0.x initializes Incus as a **one-member cluster** from the start (not a
bare daemon), so this design needs no later migration. Growing to N
members — and the operator workflow for joining them — remains the
deferred part.

### Trade-offs accepted, not solved now

- **0.x proves the lease/election mechanism, not genuine node-death
  fault tolerance.** 0.x provisions one physical node only (§2) — so
  today's "leader failover" is necessarily multiple agent *instances*
  contesting the lease on that single Incus member (proving the
  ETag-CAS/renewal/handoff logic is correct), not survival of an actual
  node's loss. Real node-level HA additionally needs Incus's own dqlite
  fault-tolerance, which needs an odd member count ≥3; a 1-2 member
  cluster gets none of that (the control-plane itself is a single point
  of failure until ≥3 members exist). Not a blocker for 0.x (one member
  anyway, and the mechanism is what's being validated at this stage) —
  revisit both the done-when criteria and the ≥3-member requirement once
  a real multi-member cluster is on the table. See #92's done-when and
  #103's validation script — which lands in `scripts/validate/` named for the
  behaviour it proves, not its issue number (#138).
- **Leader-to-node partitions.** If the leader loses reach to a specific,
  still-alive node, that node's Apps go unreconciled until the partition
  heals or the lease moves to an agent that can reach it. Accepted,
  consistent with this project's existing minutes-scale-RTO tolerance
  (`docs/AppManager.md`'s Prior-art section already frames this project's
  HA bar that way).
- **A bad agent version can cause leadership churn, not just a bad
  rollout.** The pre-promotion health gate now requires *sustained*
  health (`docs/AppManager.md`'s `healthy-since` tag / `MinHealthyDuration`
  — Nomad's `min_healthy_time`), which catches a candidate that's flaky
  enough to flap during the health-poll window. It doesn't catch a
  candidate that's stable for longer than `MinHealthyDuration` and only
  starts failing *after* promotion (no post-promotion monitoring or
  auto-rollback exists — see "Automatic rollback after a successful App
  promotion" in `docs/Out of Scope.md`). For the agent's own self-upgrade
  specifically, a version bad enough to pass the gate but crash-loop once
  leading could cause real churn: it wins the lease, crash-loops, fails to
  renew, an old-version follower (if any still exist, un-promoted) or
  another already-upgraded-but-different instance re-contests the lease,
  and so on — until an operator reverts the agent App's `image` in git. Not
  unsafe (no single instance ever gets stuck broken; the self-recognition
  rule still holds throughout) but not silent either. Accepted rather than
  building full post-promotion auto-rollback (which would need old
  generations retained post-promotion, continuous monitoring, and a
  per-renderer reversibility story — a materially bigger feature than this
  project's scale warrants). Recovery is already available with no new
  mechanism: reverting any App's `image` in git — the agent's included — is
  itself just another forward version bump through the same blue-green
  algorithm, not a distinct "rollback" capability.

### Follow-up: cardinality replaces `kind: AgentConfig`

The decision above introduced a singleton `kind: AgentConfig` to declare the
agent's image fleet-wide, on the reasoning that the agent's placement (every
node, always) and authorship (one fleet-wide value) "don't fit `App`'s
per-instance-declared shape at all." That reasoning is correct — and it is
**equally true of Alloy** (#77), which is explicitly per-node, is already
slated to be an app-manager renderer (`type: alloy`), and would otherwise
need a `kind: AlloyConfig` of its own. The shape isn't agent-specific; it's a
DaemonSet, and it already has two consumers.

So `AgentConfig` is dropped and its job moves onto `kind: App` as a
cardinality field:

```yaml
kind: App
name: agent
type: agent
replicas: per-node   # or a fixed count; required
image: {...}
```

The leader synthesizes one App-shaped value per known `Instance` for every
App declaring `replicas: per-node` — the same synthesis the earlier revision
did, no longer hardcoded to `Type: "agent"`. The agent becomes an ordinary
`App` again, `#92`'s "the agent is not a `kind: App`" invariant is retired,
and the fleet-definition schema is **three** kinds rather than four (§13's
"schema count stays at 2" was already stale; this makes it stale by one
less, not two). `replicas` is parsed by a small `encoding.TextUnmarshaler`
type in `internal/config`, exactly as `Range` already is for
`dhcp_excluded_range` (§ Validation approach) — `"per-node"` and `"3"` both
land as a typed value at parse time. The field is **required**, settled when
#97 implemented it: an earlier revision of this section had an omitted field
be a valid zero meaning 1, but yaml.v3 never calls `UnmarshalText` for an
absent key, so an omitted `replicas:` and a typo'd `replicas: 0` reach
`Validate` as the same zero value — indistinguishable. Defaulting would have
made `0` silently mean 1; requiring it makes both one error, and writing
`replicas: 1` costs an operator nothing.

**Three things that have already been conflated once, kept apart here:**

- **Cardinality** — *how many*. `replicas`. In scope now.
- **Placement** — *where*. Deferred. When it lands, the mechanism is Incus's
  own cluster groups via a **separate** field, invalid in combination with
  `replicas: per-node`, not an operator naming individual nodes.
- **`App.Node`** — deleted in the pass above precisely because it was both at
  once ("the operator's placement choice", overloaded with "wherever the
  fleet's nodes are"). That deletion is not reopened here; it's what reserves
  the placement field's shape.

`replicas` must not inherit `Node`'s conflation, which is why the placement
field is named as separate now rather than discovered later as an overload.

**One consequence for the reconcile algorithm**, and the reason this is
settled before #97/#98 are built rather than after: the per-App state machine
becomes keyed by `(App, member)` rather than by `App`. The existing
zero/one/two-match logic is unchanged — it just becomes the inner loop — plus
a `max_parallel: 1` rule (never act on two members of one App in a tick).
That's a cheap change now and a migration once the code exists; everything
else multi-instance workloads need is genuinely additive (see § Stateful app
support).

## 18. App classes, and why the renderer registry stays curated (#92)

`Renderer` is the seam for per-App-type cutover behavior, but nothing in the
design said which cutover shapes actually exist, or which one a new renderer
should be implementing. `docs/AppClasses.md` now names six (stateless;
lease-guarded singleton; asymmetric handover; symmetric scale-out; quorum
member; exclusive), classified by what blue/green *overlap* costs rather than
by write topology. This section records what follows from that taxonomy: who
gets to add an App type, and how.

The obvious generalization is to make the class declarative — an operator
writes `class: symmetric-scaleout`, brings any image, and gets the cutover
choreography for free without writing Go.

### Decision

**The registry stays curated: a new App type is a Go renderer, explicitly
registered, compiled into the agent image.** Not a declarative `class:` field.

- **The taxonomy itself says a generic system wouldn't cover the classes that
  would justify one.** The line is whether cutover requires handing over
  authoritative state. Classes 1/2/4/6 don't — the only app-specific
  questions are "is it healthy?" and "how do you stop it gracefully?", both
  probe-shaped and genuinely declarative. Classes 3 and 5 do, and that's
  irreducible choreography (who's the writer, is the candidate caught up, is
  a switchover safe now, does quorum survive the member add). So "generic"
  would cover 1/2/4/6 — exactly the four where a Go renderer is a handful of
  lines anyway.
- **Kubernetes already ran this experiment.** Probes and lifecycle hooks are
  declarative and cover Deployments/DaemonSets/StatefulSets; the ecosystem
  then produced CloudNativePG, Zalando's postgres-operator and etcd-operator
  — code, per app — for precisely classes 3 and 5. The declarative primitives
  were not enough, and the convergence was on operators.
- **"Fixed list" costs ~nothing here, for two project-specific reasons.**
  0.x is single-user with no auth (`docs/Out of Scope.md`), so the operator
  *is* the developer — "pick from a fixed list" means "write a small Go file
  in a repo you own". And § App Manager HA's self-upgrade mechanism already
  makes shipping a renderer routine: write it, build the agent image, bump
  the agent App's `image` in git. That last step is an ordinary blue-green
  agent upgrade — the exact flow the leader/follower design exists to make
  safe and validated (#103). The marginal cost of a renderer is low *because
  of* the design already committed to.
- **A declarative `class:` field is a lie-vector the curated model doesn't
  have.** It lets an operator declare class 4 for a class 3 app — two writers,
  data loss, reconciler cheerfully cooperating — and nothing in the schema can
  detect the mismatch. A renderer author can't make that mistake by accident;
  the class is implicit in the code they wrote.
- **The taxonomy still earns its keep as shared Go, not as a config enum** —
  an embeddable no-op `Promote` for classes 1/2, a destructive-update helper
  for class 6, a drain hook for class 4. Reuse without a DSL. This also keeps
  `App.Params` typed per-renderer rather than degenerating into a config
  language.

**Nothing is foreclosed: the declarative model is a strict subset of the
curated one.** If bringing your own stateless app without touching Go is ever
wanted, it arrives as *one more registered renderer* — `type:
generic-stateless`, reading its probe config from `params` — not a redesign
and not a second execution model. The option is free until exercised;
choosing generic now would instead commit to a framework serving about six
callers, against §13/§16's standing anti-premature-abstraction bias.

**When this gets revisited:** the first time someone who *can't* build and
push the agent image needs to add an App type — i.e. multi-user
(`docs/Out of Scope.md`), or a published/shared distribution of this project.
Until then the fixed list has a cost of roughly zero.

## 19. Stateful app support (databases, Kubernetes) — direction, not yet built

Raised while checking whether the App Manager design is *tolerant* of the
workloads this project eventually wants — a SQL database (ideally a stateless
proxy directing to a writer and readers) and Kubernetes. Neither is being
built now. This records the shape they'd take and what actually blocks them,
so the design isn't accidentally foreclosed. See `docs/AppClasses.md` for the
class vocabulary used throughout.

### Direction

**Three layers, and the boundary between them is the whole design.**

```
app manager   →  places instances, gates image upgrades       (tick: minutes)
DB-native HA  →  replication, write-leader election, failover  (seconds)
proxy         →  routing, health-checked against the DB layer  (seconds)
```

- **The app manager elects a reconciler. It must never elect the write
  leader.** Those are different leases with different failure semantics, and
  conflating them is how you get split-brain and lost writes. The corollary
  is just as load-bearing: the proxy discovers the current writer by
  health-checking the DB layer directly, never by reading app-manager state.
  This project's minutes-scale RTO (§ App Manager HA) is fine for "an
  instance died, recreate it" and catastrophic for "the writer died, route
  around it" — so the data plane must not sit on the reconcile tick.
- **The proxy+replicas topology is what makes a database tractable at all.**
  With replication, a replica's data lives in the *cluster*, not the
  instance, so a new generation rebuilds its state from its peers and
  create-candidate/health-check/retire becomes safe again. At N=1 there's no
  blue-green at all (class 6, destructive in-place). Multi-instance isn't a
  nice-to-have here — it's the precondition for a safe upgrade path.
- **Where consensus lives decides the App count.** Postgres + Patroni needs
  an external DCS — a 4th App, etcd, the exact dependency § App Manager HA
  went out of its way to avoid for the agent's own lease. MariaDB + Galera
  and CockroachDB own consensus internally and need no DCS. That's a real
  trade against Patroni's maturity, not a settled call. **Do not** back
  Patroni's DCS with the agent's Incus-CAS lease: technically possible,
  and a bad idea for a correctness-critical component.
- **Routing shape**: prefer HAProxy's two-frontend pattern (one port to the
  primary backend, one to the replicas, health-checked against the DB layer's
  own API, client picks its port) over query-parsing read/write splitters.
  Stateless, no query-parsing failure mode, standard.
- **The proxy is a separate `kind: App` that *references* the database — not
  a role inside some grouped "deployment" unit.** This is what the prior art
  does: CloudNativePG models `Cluster` and `Pooler` as two separate CRDs, with
  the Pooler naming its cluster; Patroni + HAProxy is the same split. Neither
  groups the roles into one object. What the topology actually needs from this
  project, then, isn't composition — it's the two smaller things in
  `docs/Out of Scope.md`: a **stable endpoint** for the proxy (clients have to
  reach it somewhere), and a **cross-App reference** (the proxy has to know
  which instances to health-check). Both are deferred; neither is a grouping
  primitive. See also the composition entry there for why `kind: Deployment`
  isn't the answer even when those land.
- **Blue-green sits at the *member* level here, not the cluster level.** Two
  colors of a stateful cluster means two clusters, which means forked data —
  blue kept taking writes while green was being health-checked. That's why
  class 3 is one long-lived cluster with replicas rolled one at a time and the
  writer switched over last: the cluster itself never has a version color, its
  members do. The exception is a **major-version** upgrade, where physical
  replication can't cross the boundary at all (16 → 17): there you really do
  stand up a second cluster, logically replicate into it, and swap the proxy —
  cluster-level blue-green, with the proxy as the swap point. That's a
  deliberate, planned operation with a stop-writes moment, not something a
  reconcile loop should ever infer from an image-tag diff.
- **Kubernetes is two classes, not one.** Workers are class 4 (green joins,
  blue drains — easy). The control plane and etcd are class 5, where adding a
  candidate member is itself a quorum event and version skew, not health, is
  what makes a member unsafe to admit.

### Prerequisites (why this isn't buildable yet)

- **Real multi-member Incus clustering.** Three replicas on one physical node
  is theater — the same critique § App Manager HA already levels at 0.x's own
  leader failover. The value only lands at ≥3 members, which is also what
  Incus's own dqlite quorum needs. So this is gated behind work already
  deferred, not behind anything about the App Manager. Note **ceph is not
  required**: local storage per replica plus app-level replication is the
  entire point, and it sidesteps the shared-storage dependency § App Manager
  HA cites as why the agent can't be relocated.
- **Persistent volumes on `kind: App`.** The schema has no volume field, and
  a class-3 renderer needs one (readers get a fresh volume per generation and
  re-replicate; the writer is switched over, never replaced). Out of scope.
- **Version-skew classification.** "The image string differs" is too coarse:
  Postgres physical replication requires matching major versions, so 16 → 17
  can't blue-green at all. Out of scope.
- **Backup.** Replication is not backup — a `DROP TABLE` replicates
  perfectly. Orthogonal to all of the above (a sidecar plus `params`), but it
  has to exist before anything real depends on a managed database.

## 20. The validation suite: bash, tailnet CI, and no OVN (#115, #127)

#115 established that nothing runs `scripts/validate-*.sh`, and that two of
them had been silently broken for weeks after #107 gated the seed and image
routes on `WIREGUARD_ENDPOINT`. Putting them in CI forces three coupled
questions: are these scripts Go or bash, how does CI reach real hardware, and
does that need OVN on the Incus host.

One fact reframes all three. The #107 gate is a *config* gate, not a hardware
gate: `internal/wireguard` runs the tunnel over `netstack.CreateNetTUN` +
`conn.NewDefaultBind()` — userspace, no `NET_ADMIN`, an unprivileged
`net.ListenUDP` — and nothing dials a real node. So the docker-compose family
(7 scripts) runs on `ubuntu-latest` today, and with the two that need only Go,
9 of the 14 need no hardware at all; only the Incus/VM family needs the host.

### Options considered

**Go vs. bash.** ~2,800 lines across 14 files, with 22 near-identical copies
of `check`/`check_json`/`check_log`/`wait_log`. Go would bring `t.Skip`, typed
assertions against `internal/config`, and real `go test` reporting.

**Self-hosted runner vs. hosted + tailnet.** The repo is public, so a
self-hosted runner would execute fork-authored code on the dev workstation.
The alternative is a hosted runner that joins the tailnet and shells into the
Incus host.

**OVN vs. plain bridges.** Incus only supports OVN-type networks inside
non-default projects (#96) — measured on 6.0.4, and re-measured on 7.0.1 after
#131's upgrade, so this is a property of Incus rather than of one version. OVN
would allow per-project network ACLs to keep CI workloads off the real LAN.

### Answer

**1. Stay bash; extract `lib.sh`** (plus `lib-compose.sh`, `lib-incus.sh`).
The strongest pro-Go argument — typed assertions against the repo's own
structs instead of `jq` string-matching — is on inspection an argument
*against*: `curl` + `jq` is the **independent observer** that makes these
scripts worth their runtime, and asserting through the app's own types means a
wrong struct satisfies the app and its validation simultaneously. That is
exactly the issue-#5 failure `CLAUDE.md` records — a passing test suite that
still shipped a silently dropped seed file. The real defects (a skip printed
as `FAIL`, and a `cmp -s` assertion that passes on a 503 error body) are
missing-harness defects, not bash defects; `t.Skip` is a six-line bash
function. Consistent with §16's anti-premature-abstraction bias, and with
`lint-mermaid.sh`'s precedent that a well-shaped bash script wired to a Make
target and CI is an accepted artifact here.

Counterweight, recorded rather than buried: `node-tunnel-survives-nat-and-provisions.sh` at 544
lines is already past comfortable bash.

**`bats-core` was considered for the harness and declined.** It would supply a
correct `skip`, a `run` helper, TAP output, and `setup_file`/`teardown_file`
for the expensive compose bring-up — genuinely useful, but roughly what
`lib.sh` costs to write, against a new dependency in a repo whose §13/§16 bias
is stdlib-and-vendoring. The apparent win, `bats --jobs`, does not survive
contact: it parallelises test *cases within* a file, whereas these scripts are
strictly ordered narratives (#38 asserts an IPAM address, then re-syncs and
asserts that address is *stable* — meaningless out of order), and the
contention that actually matters is *between* scripts fighting over host ports
and `home-lan`. `run.sh --jobs` addresses that; bats does not. `lib.sh` should
still copy bats' `skip` semantics rather than invent worse ones.

**When this gets revisited:** the first validation script that needs a real
data structure — a map or list it must build, sort, and assert over — gets
rewritten in Go, alone, and only then. Not the suite. Separately, `bats-core`
becomes worth reopening if the suite is ever split into small, genuinely
*independent* checks, since that is the shape its model and its parallelism
assume.

**2. Hosted runner + Tailscale to the Incus host, not a self-hosted runner.**
The security objection to self-hosted is real but not decisive on its own:
fork PRs cannot fire `push` on the base repo, and `workflow_dispatch` requires
write access. What decides it is operational. The workstation is not always
on, so a job targeting it queues amber until GitHub's 24-hour timeout — checks
that hang until someone boots a desktop train you to ignore checks, which is
the same disease this suite already suffers from, one layer up. And the runner
*is* the developer's machine: `app-produces-working-installer-e2e.sh` already warns against
running concurrently with `node-boots-and-trusts-bootstrap-cert.sh` (both drive the live `home-lan`
bridge, and #5 hardcodes the address IPAM hands node1), while #91 avoided
per-network NAT because it would tune the shared host's netfilter state. A
GitHub-dispatched job cannot take a lock against a human; a host-side script
under `flock` can.

Inverting the direction keeps untrusted code on GitHub's disposable VM and
makes the host reachable only through a credential GitHub **withholds from
fork PRs entirely**. The SSH target is a persistent `ci-orchestrator` Incus
container holding a project-restricted TLS cert, which launches throwaway VM
sandboxes and deletes them — so nothing CI-related lives on the desktop
filesystem, and the whole apparatus is one deletable container. A VM sandbox
rather than a nesting container because any container engine inside an Incus
container needs `security.nesting=true` (podman included), whereas a VM has
its own kernel and runs stock Docker with no privilege grant.

**3. No OVN.** The strongest case for it was network ACLs keeping CI sandboxes
off the real LAN — a legitimate goal, since `home-lan` is `ipv4.nat: "true"`
with no firewall override, so today they can reach it. But the running server
already advertises `network_bridge_acl`, so that control works on plain
bridges; and per-run network isolation already works via uniquely-named
bridges, which `node-tunnel-survives-nat-and-provisions.sh`'s `valwan-$$` does today. Against that,
OVN would add OVS, `ovn-northd`, `ovn-controller` and OVSDB as an always-on
failure domain to the machine every PR now depends on. Note also that
`features.networks=false` *inherits* the default project's networks rather
than losing them — measured on `homelab-host`, `user-1000` sees `home-lan`
while `homelab-dev` (`features.networks=true`) saw nothing, which was #96's
trap — so project scoping needs no OVN either. (`homelab-dev` was deleted in
#131; the constraint it demonstrated was re-confirmed on 7.0.1 against a
throwaway project, so the evidence outlived the project. See §21.)

**When this gets revisited:** OVN earns its place when parallel runs need
overlapping IP ranges or genuinely isolated L2 — simulating two separate
homelabs at once. Unique bridge names per run do not need it; identical
subnets per run do. It also flips if the bridge-ACL control cannot be made to
hold, since that protection is *structural* (the ACL and its network live in a
project the CI cert cannot reach) rather than enforced by a dedicated
"may not edit ACLs" restriction, which does not appear to exist.

## 21. The dev Incus host: btrfs, pinned base images, one project (#131, #96)

`homelab-host` is the dev workstation's Incus, and the substrate the whole
`scripts/validate/` suite runs against. Nothing recorded its storage, image or
version state, so it drifted — and #115's CI work depends on it. #131 audited
it live and fixed four things. #96 (the stuck `homelab-dev` project) is folded
in here rather than tracked separately, since both touch the same project.

### Storage: `dir` → `btrfs`

The pool was whatever `incus admin init` produces by accepting defaults, which
is driver `dir` — no copy-on-write, so every `incus launch` copies the entire
rootfs rather than cloning it. That is the operation #115 performs per CI run,
launching a throwaway sandbox from a baked base image.

Measured with a representative ~1.4 GB incompressible image (an Alpine launch
measures nothing — its rootfs is 3.29 MiB; incompressible so btrfs cannot
flatter itself by compressing zeroes):

| operation | `dir` | `btrfs` | |
|---|---|---|---|
| launch from image | 3,188 ms | 1,288 ms | 2.5× |
| copy instance (rootfs duplication) | 13,675 ms | 531 ms | 26× |

**Read the two rows differently.** Copy-instance is pure rootfs duplication —
the exact operation copy-on-write replaces — and 26× is COW doing what it
says. Launch-from-image also unpacks and starts an instance, so the driver is
only part of the cost, and 2.5× is the honest ceiling on what a CI run saves.

**Caveat, stated because the numbers invite over-reading:** the `dir` figures
were taken on server 6.0.4 and the btrfs figures on 7.0.1, so the comparison
is confounded by the version upgrade. The plan called for re-measuring `dir`
on 7.0.1 to isolate the driver; the migration ran straight through and the
`dir` pool no longer exists, so that control is unrecoverable. A 26× copy
delta is far too large to be a version effect, but the 2.5× launch delta is
not cleanly attributable.

**The measurement also corrected the premise.** #131 argued a `dir` launch
might cost "more than the image pulls it exists to avoid"; at ~3.2 s that is
plainly false. The honest case for btrfs is that it removes a few seconds per
run, collapses instance cloning to near-free, and costs nothing to adopt —
not that `dir` was catastrophic. Recorded this way deliberately: the issue's
framing would have justified the change on a number that turned out not to
exist.

btrfs over zfs because it needs no out-of-tree kernel module. Loop-file backed
rather than a partition, so no repartitioning of a working workstation.

A pool's driver cannot be changed in place, so the migration deletes and
recreates the pool — destroying every instance, volume and image on it.
`scripts/host-setup/incus-storage-migrate.sh` does this behind an explicit
`--yes-destroy-everything` flag. It lives outside `.devcontainer/host-setup/`
on purpose: everything there runs unattended via `initializeCommand`, and a
step that destroys the host's instances must never be on an automatic path.

### `homelab-dev` deleted (this is #96)

The project got `features.networks=true` while exploring project-scoped
networks. Incus only supports OVN-type networks inside non-default projects,
so `home-lan` became invisible from it, and the flag would not unset because
Incus considered the project non-empty. #132 had already repointed the suite
at `default`; #131 deleted the project (delete its one cached image, then the
project — an empty default profile does not block deletion).

Repaired rather than deleted was never seriously on the table: nothing needed
a second project, and the OVN constraint that broke it still holds. Confirmed
empirically on 7.0.1 rather than assumed — a throwaway
`features.networks=true` project still refuses a bridge, failing on
`/run/openvswitch/db.sock`. §20's "no OVN" conclusion is unaffected.

### The base image is pinned

Nine call sites launched `images:alpine/edge` directly. `edge` is a moving
tag, and the local copy expires on the default 10-day
`images.remote_cache_expiry` — so the suite both re-downloaded periodically
and silently ran against a drifting base image, a flakiness source nobody
would attribute correctly.

Not hypothetical: #131 recorded fingerprint `19237dd97601` (`20260716_13:00`)
and three days later the host held `20260718_13:00` under a different
fingerprint.

Now pinned to local aliases `validate-alpine` and `validate-alpine-vm`, created
by `.devcontainer/scripts/3-pin-validate-images.sh` and asserted by
`require_incus_image`. **Two aliases, not one**, because a container image and
a VM image are separate images with separate fingerprints and an alias resolves
to exactly one — `incus launch <alias> --vm` needs its own.

Deliberately no `--auto-update`: that reintroduces exactly the drift being
fixed. Refreshing is a conscious act — delete the alias, re-run the script.
Explicitly copied aliased images are not *cached* images, so
`images.remote_cache_expiry` cannot reap them (verified: `expires_at` is zero).

### Client/server version skew, and enforcing it

The host ran server 6.0.4 against a 7.1 client. Nothing detected it; it
surfaced only as a stray `Can't specify column L when not clustered`. Resolved
by upgrading the host to 7.0.1 via Debian backports — keeping the server in the
distro's packaging rather than adding a third-party line.

Fixing it once only resets a clock, so it is now asserted:
`devcontainer-reaches-host-incus.sh` fails if client and server disagree, and
`.devcontainer/scripts/1-setup-incus-remote.sh` warns at devcontainer start
(warning only — it runs under `set -e` on `postStartCommand`, and refusing to
start a devcontainer because the host is mid-upgrade is a bad trade).

**The contract is major-version agreement, not exact match.** The client comes
from zabbly `stable` and rebuilds independently of the host, so client 7.2
against server 7.1 is normal. A check that fails on that is one people learn to
ignore, which is worse than no check. The defect worth catching is a
major-line split, which is what 7.1-vs-6.0.4 was. Known consequence: when
zabbly `stable` reaches 8.x it will outrun a backports 7 LTS server and this
will go red — that is the check working, and the fix then is to pin the
Dockerfile to a 7.x line.

### What was deliberately not done

**No reaper.** Script-created objects follow `validate-<slug>-<role>-$$` for
instances and volumes, and `valwan-$$` for networks (short because bridge names
become real `ip link` interfaces, capped at 15 characters). That convention is
now written down in `scripts/validate/README.md`, which is what #131 asked
for. A reaper to collect leftovers automatically was considered and declined —
the convention makes leftovers identifiable by hand, and the suite showed no
evidence of actually leaking. Revisit if leftovers start accumulating in
practice.

**No `tofu/ci/`.** #131 asked for the resulting state to be expressed in
OpenTofu. Deferred to #115, which is the issue that actually stands up
`tofu/ci/`; designing that layout here would mean #115 reworking it.
Reproducibility in the meantime comes from the two shell scripts above, which
is the same pattern `.devcontainer/host-setup/setup-incus-trust.sh` already
uses.

## 22. The validate suite's delivered contract: skips, exit codes, self-description (#140, #136)

§20 decided *to* extract `lib.sh`; it predates the harness actually landing,
and so records none of what the harness turned out to need. This entry is that
record. Numbered 22 rather than 21 because §21 (the dev Incus host) was in
flight on another branch when this was written.

**The finding that reframed the work.** #115 was opened on the premise that the
suite had rotted. It had not: 12 of the 13 runnable scripts were green. **The
skip contract was the dominant defect** — every failure in the suite that
wasn't #137 turned out to be an unmet prerequisite reported as a failure. A
suite that cries FAIL when `flasher-tool` is merely absent (#136) trains its
operator to ignore it, which is the same disease as a suite nobody runs, one
layer up. Recorded because the premise was wrong in an instructive direction:
the scripts were fine and the *reporting* was broken, so a rewrite — the
obvious response to "rotted" — would have preserved the actual defect.

### Answer

**Four exit codes, because "didn't run" is not "failed".** `0` pass, `1` a
genuine assertion failure, `2` a hard prerequisite missing (the script cannot
meaningfully start), `3` a skip. The distinction that earns its keep is 2 vs.
3 vs. 1: "you didn't install a tool", "this route needs hardware you don't
have", and "the thing under test is broken" are three different facts, and
collapsing them is precisely what made the suite ignorable. Semantics copied
from `bats`' `skip` rather than invented, per §20's note.

**`make validate` treats 3 as success, and this is not a fudge.** Under
`--strict --allow-skip <tag>` the only skips that survive are ones the command
explicitly blessed, so treating 3 as a build failure would make a correct run
red. `run.sh` still *reports* 3 rather than 0, because "not everything ran" is
worth saying out loud — the Makefile decides what that means for a gate, the
harness only reports it. Keeping that judgement in the caller rather than the
harness is what lets CI and a developer's laptop apply different policies to
the same run.

**`--strict --allow-skip` is the anti-rot mechanism.** A skip is the failure
mode that hides: #107 gated the seed and image routes on `WIREGUARD_ENDPOINT`
and four scripts went quiet for weeks. An allow-list inverts that — a route
that silently gains a precondition is no longer on the list, so it goes red
instead of quiet. The list is the point; `--strict` alone would only forbid
skipping.

**Scripts declare, docs don't.** Each script carries `VALIDATE_PROVES`,
`VALIDATE_GROUP`, `VALIDATE_NEEDS` and `VALIDATE_DURATION`, and
`run.sh --describe` reads them back. The alternative — a table in a README —
is a second source of truth that drifts from the scripts the moment anyone
edits one, and this repo has already paid for exactly that (`README.md`
described `scripts/validate-issue-N.sh` for weeks after #138 deleted them).
Asking the scripts what they need cannot drift.

**When this gets revisited:** the exit-code vocabulary grows only if a real
outcome appears that none of the four describes — a flake distinct from a
failure would be the plausible one, and only once flakes are observed rather
than anticipated. Separately, §20's Go trigger still stands and is unaffected
by any of this: the harness being pleasant to use is not an argument for
staying bash if a script ever needs a real data structure.

Implementation landed under #140 (harness, `run.sh`, the three libraries) and
#136 (the missing-`flasher-tool` skips that motivated it); operator-facing
detail lives in `scripts/validate/README.md`, and the day-to-day rules in
`CLAUDE.md`. Enforcement — a workflow that actually runs any of this — is
**not** part of it and does not yet exist.

### Follow-up: the compose group's serial-teardown barrier (#153)

#153 reported `make validate` failing nondeterministically on clean `main`,
always in the `compose` group, with the hypothesis that a script's
`docker compose down` returned before releasing the shared host ports/network,
so the next script's `compose up` raced it. Characterising before fixing
(the suite's whole doctrine) **disproved that mechanism on the current
toolchain**: on Docker 29.6.1 / Compose v5.3.1, `down` releases the published
ports *and* the project network before it returns, measured directly, and 13
consecutive suite runs (5 group + 8 full `make validate`, the latter under CPU/IO
load) stayed green. The bug was real when filed — the most likely cause is a
Compose upgrade since, older versions tearing networks/ports down more loosely.

The fix is kept anyway, as a `compose_down` barrier in `lib-compose.sh` that
every compose script's cleanup trap calls: it blocks until the containers and
published ports are actually gone. It is a no-op where `down` is already
synchronous, and defends the group's determinism on an older or CI-provided
Compose whose version the repo does not control — which matters precisely
because enforcement (#146, a *required* validate check) is the next step, and a
required gate cannot be allowed to flake on a runner we didn't measure.

## 23. node0 needs the internet, so the tunnel test can't be hermetic (#137)

#137 reported three failing assertions in
`node-tunnel-survives-nat-and-provisions.sh` — the WireGuard handshake, the
Incus API over the tunnel, and the temp-cert provisioning mechanism — and
diagnosed them as the node auto-updating IncusOS during the 3-minute handshake
window. The suggested fix, in preference order, was to stop node0 reaching the
internet.

**That diagnosis was wrong, and the suggested fix is not available.** Recorded
because both halves are things a future reader would otherwise retry.

### The tunnel was never broken

Queried live against a running node, mid-window:

```
"latest_handshake": "54 seconds ago",
"stats": {"rx_bytes": 92, "tx_bytes": 244}
```

The tunnel handshakes, through the simulated NAT, on a node that *had* already
auto-updated. Six separate defects were found in the script and its harness —
five of them causing the three failures, plus the assertion-quality defect the
issue was actually filed over. Each masked the next, which is why they came
off one full run at a time:

1. **`APP_PUBLIC_KEY` was always empty — on every run this script has ever
   had.** `POST /instances/{name}/seed` returns compact JSON whose *values* are
   YAML documents, so the web app's key appears as `public_key: <base64>`. The
   extraction grepped for JSON syntax (`"public_key": "…"`) and matched
   nothing. Nothing noticed, because the handshake check then grepped for that
   empty string and its pattern became vacuous.
2. **The endpoint moved.** IncusOS serves system network state at
   `/os/1.0/system/network`; the script asked for `/1.0/system/network`, which
   404s on a node that has updated. This is the only respect in which the
   auto-update mattered — and the durable answer is an assertion that tries
   both paths, not a frozen OS version.
3. **The old NAT check asserted configuration, not function.** It ran four
   setup commands and asserted they exited 0, so a broken NAT simulation and a
   broken tunnel presented identically. This was the defect #137 was actually
   filed over, and the one worth fixing regardless of the others.
4. **The harness binary could not execute at all.** It was built without
   `CGO_ENABLED=0`, so it requested `/lib64/ld-linux-x86-64.so.2` — which
   musl-based Alpine does not have. `incus exec` reports that as `Error:
   Command not found`, for a file that is present and executable, which is
   why it read as a tunnel failure rather than a build one. The web app
   binary two lines above had always set `CGO_ENABLED=0`; the harness never
   did.
5. **The harness container was used before it had a DHCP lease.** It dials
   node0 the moment it starts, and an interface without a lease has no route
   to home-lan — which surfaces from inside WireGuard's own handshake as
   `sendmmsg: network is unreachable`, reading like a tunnel fault rather
   than a container that isn't up yet. Every other container in the script
   already waited; this one never did, and nothing noticed while (4) meant
   the binary could not run at all.
6. **The harness was also dialling with the wrong identity, and had nowhere to
   send it.** It was passed the *web app's* public key while its peer is node0,
   whose `wg0` key is a different one; and neither its own peer entry for
   node0 nor node0's seeded entry for it carried an endpoint, so neither end
   could send a first packet. `UpsertPeer` cannot express an endpoint — that
   is deliberate, because the web app genuinely never knows a node's address
   in advance. The harness does know, having just read it off the node, so it
   now uses `UpsertPeerWithEndpoint`, and the script reads node0's real `wg0`
   public key and `listening_port` from the node's own API. (The port has to
   be read: the rendered seed sets no `port` for `wg0`, so IncusOS binds a
   random one.)

### What the fixed assertions then found: a real product gap

With those six defects cleared, the handshake assertion passes and the run goes
from *30 passed, 3 failed* to *39 passed, 2 failed*. The two that remain are no
longer test bugs — they are the suite doing its job, and they are left failing
deliberately rather than papered over. node0's own view of the harness peer, at
the moment the HTTP dial times out:

```
"latest_handshake": "1 minute, 3 seconds ago",
"endpoint": "192.168.1.58:37856",
"stats": {"rx_bytes": 724, "tx_bytes": 188}
```

The WireGuard layer works completely: handshake done, endpoint learned,
traffic in both directions. What does not work is IP on top of it. `wg0` is
rendered with a `/32` address and no routes:

```
wg0  addresses: ["10.100.0.2"]   routes: <none>
eth0 addresses: [...]            routes: [default via …, 10.200.0.0/24 via …]
```

So node0 has no route covering `10.100.0.0/24` and cannot originate or reply
to overlay traffic — the TCP SYN-ACK has nowhere to go. Incus itself is bound
correctly (`core.https_address: ":8443"`, and `wg0` carries the `management`
role), so this is not the binding assumption `internal/seed/seed.go` flags as
unverified; that assumption survives.

`internal/seed` renders the peer's `AllowedIPs` but nothing on
`SystemNetworkWireguard.Routes`, which the vendored API does expose. `wg-quick`
would install allowed-IPs as routes; IncusOS evidently does not. Tracked as
#157 rather than fixed here: it is a change to production seed rendering, needs
its own validation, and #137 is about the assertions, not the seed.

**Note this means #91's second done-when criterion — "the node's Incus API is
reachable through the tunnel" — has never actually held.** It was reported as
passing by an assertion that could not have detected otherwise.

**Resolved by #157 — but not by adding a route.** The paragraph above assumed
`SystemNetworkWireguard.Routes` was the answer. It is not: IncusOS's own
validator rejects the only route this could emit. The fix is a wider address on
`wg0`; §24 records why the vendored API cannot express the route this section
reached for, and why node0's network state could never have proven it either
way. The first of the two failures described here is fixed and now passes on
real hardware. The second turned out not to be this bug at all: with the route
in place, `create-instance` reached Incus and failed on the harness asking for a
storage pool named `default` when IncusOS names its only pool `local`, fixed
separately in #161. With both landed the script's contract is 41 passed,
0 failed.

**None of these three assertions had ever passed.** The extraction in (1), the
cgo-linked harness in (4), the missing DHCP wait in (5), and the wrong key in
(6) are all present in `f94fa44`, the commit that introduced the script — so
they failed from the day it landed. #137's premise that "the
tunnel path is byte-identical to when this script last passed" was mistaken;
there was no such run. Worth stating because it changes what the suite's green
history means: a script can be merged, be re-run for weeks, and never once have
proven its headline claim.

### Blocking node0's egress breaks the node outright

An Incus network ACL on node0's NIC (`security.acls` plus
`security.acls.default.egress.action=reject`) works exactly as documented, and
was verified enforcing: DHCP still leases, the gateway is still reachable,
`1.1.1.1` is dropped, and the node stays on the installed build instead of
updating. It also makes the test useless. From node0's console under that ACL:

```
ERROR Failed to check for Secure Boot key updates err=http request timed out
      after five seconds: Get "https://images.linuxcontainers.org…
failed to perform NTP synchronization, system time may be incorrect
refused (provider: images)
```

**IncusOS fetches the Incus application itself from
`images.linuxcontainers.org` during first boot.** With egress blocked, Incus
never installs, port 8443 never opens, and all three tunnel assertions fail —
for a new reason, having replaced one environmental dependency with another.
Narrowing the ACL doesn't help: the OS update and the application download come
from the same host, so no rule separates them.

The seed offers no lever either. The vendored IncusOS seed types
(`internal/third_party/incusos/api/seed/`) cover install, network,
applications, and incus — none carries an update channel or auto-update
toggle, so #137's second suggestion would need new upstream surface vendored
first.

**So the test is deliberately not hermetic.** node0 reaches the internet
through `home-lan`'s own `ipv4.nat` and updates itself, and the defence against
a moving upstream is that the assertions no longer care which build answers.
That is a weaker guarantee than isolation and worth stating plainly — but it is
the one available, and it is strictly better than the previous state, where the
same drift produced three red assertions naming the wrong subsystem.
1. Track the commit hash nodes are running
2. Some phone home functionality could be a nice-to-have if this has low development cost. I.e., could the node phone the dev instance over tailscale to indicate success (and provide a manifest of it's hardware)?

## 24. `wg0` takes an overlay-width address, because the seed API cannot express an on-link route (#157)

§23 left two assertions failing deliberately: a node completed a WireGuard
handshake but could not carry IP traffic, because `internal/seed` gave `wg0` a
`/32` and nothing covering `10.100.0.0/24`. It also proposed the obvious
remedy — populate `SystemNetworkWireguard.Routes` — which is what #157 was
filed to do, hedged with "the exact shape needs checking against IncusOS's
semantics first."

Checking it resolves the hedge in the negative. **The remedy is not available,
and attempting it is worse than the bug.** Recorded here because the issue text
still says otherwise and will outlive it.

Evidence is from upstream `lxc/incus-os` at `2ad9069b90b4`, not from this repo:
`scripts/vendor-incusos.sh` vendors only `incus-osd/api/`, so the rendering and
validation code below cannot be read from the submodule.

### Nothing else creates the route, so the address has to

IncusOS configures WireGuard through systemd-networkd. The generated wireguard
`.network` file is `processAddresses(wg.Addresses)` plus
`processRoutes(wg.Routes)` and nothing more — `RouteTable=` appears **nowhere**
in `internal/systemd/networkd.go`. That setting is what asks systemd to turn a
peer's `AllowedIPs` into routes, and `systemd.netdev(5)` says it "Defaults to
false", where "off" means "the routes to the addresses specified in the
`AllowedIPs=` setting will not be configured". So they install no routes at
all. `wg-quick` installs them as a side effect; systemd-networkd does not. That
single difference is the whole bug.

`processAddresses` emits `Address=<addr>` verbatim, and `AddPrefixRoute=`
"Defaults to true" (`systemd.network(5)`), so `10.100.0.2/24` makes the kernel
install `10.100.0.0/24 dev wg0 proto kernel scope link` — covering both the web app
(`.1`) and the validate harness (`.254`). `validateAddressWithCIDR`'s regex is
`^[.:[:xdigit:]]+/\d+$`, so `/24` is exactly as valid a seed value as `/32`.

Peers' `AllowedIPs` stay `/32`. That is crypto-routing — which peer may carry
which addresses — a different concern from IP routing, and widening it would let
one peer claim every other node's overlay address.

### The `Routes` field cannot express what this needs

`SystemNetworkRoute.Via` has no `omitempty`, so a gateway-less route marshals as
`via: ""`. IncusOS's own validator rejects that before systemd ever sees it —
`validateWireguard` in `internal/systemd/networkd_validate.go`:

```go
for routeIndex, route := range wg.Routes {
    err := validateAddressWithCIDR(route.To)
    ...
    err = validateAddress(route.Via)   // unconditional
    if err != nil {
        return fmt.Errorf("wireguard %d route %d 'Via' %s", index, routeIndex, err.Error())
    }
}
```
```go
func validateAddress(address string) error {
    if address == "" {
        return errors.New("has empty address")
    }
```

The **whole network configuration** fails validation, not just the route. Had
#157's suggested fix been implemented literally, it would have replaced a node
with a missing route by a node whose network config IncusOS refuses outright.

No other `via` helps. The only non-empty candidate on the overlay is
`10.100.0.1`, the web app — which is unreachable from a `/32`, so the kernel
rejects the route, and which sits inside the very prefix being routed. `via` the
node's own tunnel address is gateway-is-self nonsense. There is no way to emit
`Scope=link` or `GatewayOnLink=` through the vendored type.

### The node's own network state cannot prove this fix

Worth stating plainly, because it is exactly the assertion someone will reach
for next. IncusOS builds `state.interfaces.<n>.routes` by running `ip route show
dev <n>` and regex-matching `` `(.+) via (.+) proto` ``, so it reports only
routes with an explicit gateway. A connected route has none and **never
appears — before or after the fix**. §23's `wg0 … routes: <none>` evidence was
therefore consistent with both the broken and the fixed node, and it is why
eth0's own `192.168.1.0/24` connected route is missing from that same listing.

Addresses are no better: `GetIPAddresses` matches `` `inet6? (.+)/\d+ ` `` and
keeps only group 1, discarding the prefix length — which is the entire content
of this fix. `10.100.0.2/32` and `10.100.0.2/24` are indistinguishable there.

So `scripts/validate/node-tunnel-survives-nat-and-provisions.sh` adds no
structural check for the route. What it proves is the behaviour — TCP over the
overlay, in both directions, from a real second WireGuard endpoint — which is
strictly stronger than a route's presence, and is what #91's done-when actually
asks for. A `.routes` assertion would have been permanently red on a *working*
node: the #129/#137 false-signal failure mode, inverted. The seeded address is
printed in the failure text instead, as a diagnostic rather than an assertion.

### #91's done-when now holds for the first time

"The node's Incus API is reachable through the tunnel specifically" has been
reported green since #91 landed and was never true (§23). It is true as of #157,
proven on real hardware: the assertion that had never once passed since #91
introduced it now does.

#157's own run was 40 passed, 1 failed rather than the 41/0 it predicted, and
the difference is worth recording. #157 assumed *both* remaining failures were
the missing route. Only one was. With the route in place the second got all the
way to Incus and failed on its own merits — the validate harness requested a
root disk on a pool named `default`, while IncusOS names its only pool `local`.
That check had never passed either, so a wrong pool name sat behind a network
failure from #91 onward, invisible until the network was fixed. Fixed in #161,
which is what takes the script to 41/0.

Twice now — #137 → #157, and #157 → #161 — fixing the outermost layer of this
path has exposed the next latent defect underneath. Worth expecting a third:
until an assertion has actually passed, nothing downstream of it has been
exercised, and "the suite is green" says only that no one has looked yet.

## 25. Leader election: a designated primary fenced by git commits, pluggable, ranked-over-Incus later (#108, #160, #212)

§17 chose an Incus ETag-conditional-write lease. #160's spike found the primitive doesn't do what that design needs, and this section replaces it. §17's lease bullets are superseded; the rest of §17 (one leader, leader-only fleet reconciliation, self-recognition, blue-green) stands.

### What the spike found (Incus 7.3, non-clustered; `scripts/validate/incus-etag-write-guards-lost-updates-not-races.sh`)

§17 claimed Incus's conflict rejection "is the entire concurrency mechanism". It isn't:

| Probe | Result |
|---|---|
| Matching `If-Match` on instance `PATCH`/`PUT` | lands (config-only `PATCH` on a never-started instance is synchronous, 200) |
| Stale `If-Match` | 412, nothing changes |
| A one-key `user.*` change | moves the ETag, so a renewal is visible |
| 16 concurrent writers, one shared ETag, on an **instance** | usually exactly one wins, but ~15% of rounds admit **two** |
| Same race on a **project** | 4–15 of 16 writers win per round |
| 16 concurrent `POST`s of one **profile** name | exactly one 201 in 8/8 (then 10/10) rounds; losers get a raw 500 |
| 16 concurrent `POST`s of one **instance** name | 9–16 accepted (202): creation is async, the conflict surfaces later |

The Incus REST docs promise only this: send the ETag as `If-Match` "to avoid race conditions … This will cause Incus to fail the request if the object was modified between GET and PUT." That is lost-update protection for one client's GET-then-PUT, not exactly-one-winner under contention, and `PATCH` accepting it is observed behaviour, not documented. The check and the write are evidently not atomic (cause inferred, not confirmed against `incusd`'s source). Two winners both believe they lead; the fencing `term` can't help, since both read the same state and write the same `term`+1.

Unverified: the clustered path. All numbers are from a non-clustered host, and 0.x runs a one-member cluster.

### The design space, and why each was or wasn't chosen

Requirement: **at most one agent performs leader-only writes at a time.** Idle periods of minutes are acceptable (0.x's HA bar is minutes-scale RTO, §17); zero leaders is safe, two is not.

1. **Incus instance lease, ETag CAS + read-after** (§17's design, with a token-and-settle-delay workaround). *Rejected.* Correct only if the settle delay exceeds a race window that can't be bounded or documented.
2. **Incus profile-create arbiter** (`lease-<term>` profiles in the coordination project; a database uniqueness constraint picks exactly one creator). *Viable fallback, not chosen.* Real arbitration with no new dependency, and the term is the name so fencing is free. Costs: relies on an undocumented property; losers see a 500 that must be confirmed by re-reading; old terms must be pruned. Profile names cap at **64 characters** (measured: 64 accepted, 65 rejected); `lease-` plus an int64 is at most 25, so the limit never binds and the counter never wraps in practice, but the term must be parsed as a number (never sorted as a string) and reaching int64 max must be a hard stop, not a wrap. Unclustered-only evidence.
3. **External lease service (etcd, Consul).** *Rejected.* Real HA needs three members, which on one physical node is no better than today; and it is a class-5 quorum App (`docs/AppClasses.md`) deployed by the manager that depends on it — a bootstrap circularity. §17 had already ruled out a new coordination dependency.
4. **Embedded `hashicorp/raft` in the agent.** *Rejected.* It makes the agent stateful (a log and stable store that must survive blue-green replacement, and two instances can't share one identity), makes it a class-5 quorum member (whose upgrades need `max_parallel: 1`, contradicting fleet-wide concurrent agent upgrades), turns every upgrade into a Raft membership change run by the reconcile loop, and adds a peer network. Incus is already a dqlite/Raft cluster, so this duplicates a consensus group that exists. Gains nothing in 0.x, which has one node.
5. **`hashicorp/memberlist` (SWIM gossip).** *Rejected.* It provides membership and failure detection, not election, so a rule (highest rank) is still needed. Its failure detector runs over a different path than the actions: agents that can't gossip but can all reach Incus each see the others as dead and elect themselves, producing extra leaders. Heartbeating *through Incus* makes liveness and the ability to act one channel.
6. **Kubernetes leases / leaderless deterministic apply.** *Deferred, and not a substitute.* Workloads inside a cluster can be applied by several managers as no-ops (server-side apply, pure rendering, a recorded applied revision to stop flip-flop between managers on different git commits, ownership-labelled pruning), or better handed to an in-cluster controller that uses Kubernetes Leases. But the cluster's own lifecycle (bootstrap, control-plane join, drain, upgrade, etcd membership) is choreographed and single-actor, and it can't use the cluster's leases to decide who builds the cluster. Kubernetes would sit above election, not replace it. See §19.
7. **Temporal.** *Deferred.* It removes the leader entirely — a fixed workflow ID allows one reconcile per App, workers compete for tasks, a Schedule with overlap policy `SKIP` replaces the tick — moving the single point of coordination to the Temporal server. Hosting it needs a database (a stateful App the manager can't safely upgrade with itself) or a cloud VM. `Renderer.Promote` is the seam: the first class-3 renderer can start a workflow there without Temporal becoming a tier-0 dependency of everything. If Temporal is adopted, this section's election machinery is moot.
8. **Web app as arbiter** (grants leases from a cloud VM). *Rejected by the operator.* It would make the web app a hard availability dependency for management.
9. **Ranked election over Incus heartbeats.** *Specified below; deferred.*
10. **Operator-designated primary, fenced by git commits, with an acting handoff.** ***Chosen for 0.x.*** (It was first chosen with an operator-raised epoch as the fence; see History.)

### Decision

Leader election is an interface, `internal/leaderelection.Elector`, answering `MayAct(ctx) (Decision, error)`. Callers ask before every leader-only action and treat an error as "not leader". 0.x ships one implementation, `Designated`; a second (ranked) can replace it with no change to the reconcile loop.

**Designated.** Git config names a `primary` node, and nothing else (`docs/Config Schema.md`). The designation names a node, not an instance, so a blue-green self-upgrade needs no designation change: the old leader steps aside (`draining`, #109) once it sees its candidate sustained-healthy, the candidate leads and retires it, and the old instance never has to delete itself.

Each agent publishes two `user.*` keys on its own instance, each written by that agent alone (no CAS, no contention):

- **its commit:** the `HEAD` of its last successful sync, which its designation came from;
- **`acting`:** whether it is acting.

Its **peers** are the running instances of its own App: they carry the same `user.homelab-ops.app` value as its own instance, which it reads from its own listing rather than from config. Without that, every agent on one Incus would elect together, and a validate script's throwaway agents would fence real ones with commits from a history they don't share.

#### Commits fence

**An agent may act only if every running, non-draining peer's published commit is in its own history.** A peer on a commit it doesn't have is ahead of it. The agent then stands down and re-syncs at once, instead of waiting for its next poll.

- **One-sided.** A peer that is behind never blocks anyone; only evidence that *I* am behind stops me. Being ahead never makes an agent leader either: leadership is still exactly `primary`. It only makes agents that are behind pause until they catch up.
- **"In my history" means reachable from the commit of my last successful sync,** not "the object is in my clone". A clone keeps abandoned commits after a force-push, so an object-presence check would let the agent that fetched the rewrite trust a peer still on the old history.
- **Agents keep the full repo history** in a persistent clone, and fetch into it, rather than a fresh depth-1 clone per sync (a config repo is tiny). That makes the ancestry check a local go-git lookup. It's also why the published commit alone is enough, rather than the last *N* commits: with only *N*, an agent more than *N* commits behind finds no evidence it's stale, and carries on.
- **The fetch must force-update** (`+refs/heads/<ref>:refs/remotes/origin/<ref>`). Without the `+`, go-git v5.19's fetch *succeeds* after a force-push and leaves the tracking ref on the abandoned commit, so an agent would sit on a stale checkout indefinitely with no error. `TestCloneFollowsAForcePush` pins it.
- **No ratchet.** An agent publishes whatever commit it is on, and that may go *backwards*, e.g. after a force-push rollback.
- **Draining peers are skipped.** A peer drained for sync failure can't republish, so after a force-push its abandoned commit would otherwise hold every agent behind until it reached git again. A draining peer that is still acting still fences, through the acting handoff. The cost: if the only peers ahead of a stale primary are drained, it isn't fenced by commit; the handoff still keeps it from overlapping anyone.

A resurrected old primary on a stale checkout therefore sees a peer on a commit it lacks, and stands down. The fence comes from git itself, and moves on every commit.

#### The acting handoff

Commits tell an agent it's stale. They don't stop a new primary from starting before the old one has finished its in-flight work. So a new primary waits for the old one's flag.

**Before acting,** an agent:

1. publishes `acting: true`;
2. *re-reads* its peers;
3. acts only if every check below still passes, and the re-read shows its own flag set. Otherwise it clears the flag and does nothing this tick. A claim whose re-read *fails* is withdrawn the same way.

An agent already acting does the same on every tick: it re-publishes the flag before reading, so every read it acts on was taken after publishing. Publishing and then re-reading means that of two agents claiming at the same moment, at least one sees the other (Dekker ordering). Both backing off is safe. The next tick settles it, because the one that's behind stays out.

**When it stops,** because a check fails or it is draining, it finishes or abandons its in-flight work and *only then* publishes `acting: false`.

**On startup,** an agent clears its own `acting` before anything else. A flag left by its previous run is its own, and it isn't acting.

Failing over from node0 to node1:

1. node1 syncs the change, publishes it, and waits, because node0 shows `acting: true`.
2. On node0's next tick, it sees node1's commit, which it doesn't have. It stops, publishes `acting: false`, and re-syncs.
3. node1 takes over.

There's no overlap and no timer. The handoff takes as long as node0's next tick. A same-node self-upgrade gets the same guarantee: the candidate waits for the old generation's `acting: false`, which the old one publishes once it drains (#109).

#### The full rule

An agent may act iff, all at once:

1. its designation, from its own last successful sync (within the sync-failure threshold, below), names its node as primary;
2. every running, non-draining peer's published commit is in its own history;
3. no other running peer shows `acting: true`;
4. it isn't draining, and no older non-draining instance runs on its node.

It re-checks all four after publishing `acting: true`, before its first action. Two further checks each make a claim that nobody else could see impossible: an agent whose own instance isn't in its running-peer list refuses to lead, and the re-read must show its own flag set.

#### Sync failures and self-draining

**`Source()` errors after N consecutive failed syncs** (`AGENT_SYNC_FAILURE_THRESHOLD`, default 10 at the default 30s tick), rather than silently serving the last-cached designation forever. `MayAct` treats a `Source()` error as "not leader", so an agent cut off from git stops acting. It doesn't need git to know it should stop, only to have tried and failed enough times. N is set so total tolerance is minutes, this project's RTO bar, and an ordinary transient fetch failure doesn't trip it.

**A commit that fetches but fails to parse or validate is a failed sync.** It counts toward the threshold, the previous commit stays current, and that's the commit the agent publishes. An agent whose binary accepts the newer commit therefore looks *ahead* to one that doesn't, and the latter stands down: an agent that can't use `HEAD` is behind.

**On crossing the threshold, the agent self-drains:** it sets `draining` on its own instance, a single-writer write that needs only Incus. `MayAct` would already return not-leader; draining adds a durable, `incus list`-visible distinction between "alive but has given up", "dead" (heartbeat stopped too) and "still trying". It also unblocks a newer generation already waiting on this node, as any drain does. A fleet-wide git outage therefore produces zero leaders, never two.

**Un-draining is gated, on the write side.** Before an instance clears its own `draining`, it must have completed a successful sync ("not failing" isn't recovery, since a restarted agent's failure count starts at zero), and no higher-generation peer on its node may be non-draining (`MayUndrain`, a pure function). If one is, a newer generation took over while this one was drained, and this one stays drained permanently, like any post-handoff old generation; deleting it is the new leader's job. `MayAct` deliberately doesn't enforce this. Its peer scan is asymmetric: a candidate defers to an older non-draining instance, but an older instance never checks for a newer one, which is what lets the old instance keep acting (health-checking, validating its candidate) through an ordinary self-upgrade window. A symmetric check would stop it the moment a candidate existed.

So `draining` has two triggers behind one bool: a self-upgrade drain is permanent, and a sync-failure drain is conditionally recoverable. `MayAct` only asks "is this peer draining"; the agent loop tracks why.

#### Failure cases, and what they cost

- **The old primary's node is dead or partitioned.** Its `acting: true` is left behind, so the peer list counts an instance only when Incus reports it `Running`, not merely "not stopped".
  - Incus reports instances on an offline member as Stopped or Error, so they drop out without special handling.
  - The partitioned old primary stops by itself. Its agent reads its peers from its own member's Incus on every tick, and a member without quorum can't serve that read. The read errors, an error means "not leader", and the agent stops at its next tick. (Unverified; see To verify.)
  - That stops the *agent*, not its work. Work is fenced only if it can't complete without quorum, which is why the guarantee below makes "every leader action is an Incus call" a condition.
- **The old primary is hung,** with its instance Running and `acting: true`, but not ticking. Takeover blocks: safe, not live. The missing-primary-heartbeat alert fires, and the operator stops the hung agent instance. There is deliberately **no timeout** on `acting`: a process that passed its checks, froze, and woke mid-action would overlap its successor if the flag could expire. Lease designs can't make this promise.
- **Every push pauses the leader.** A follower that syncs first publishes a commit the leader lacks, so the leader stands down until it has fetched: about one fetch, given the immediate re-sync. It happens even for commits unrelated to leadership, because a stale agent can't know what a commit it lacks contains. Minutes-scale RTO absorbs it.
- **A force-push stalls leadership.** Agents on the old history and the new each see the other's commit as unknown, and all stand down until they converge: zero leaders, never two. Force-pushing the config repo's branch should be rare.
- **A rollback is the one case commits don't fence.** If a push resets the branch to an ancestor, an agent still on the newer, removed commit sees the rolled-back commit as in its history, so it keeps acting on its stale designation until its next poll. The acting handoff still prevents overlap, because the new primary waits for its flag. Only liveness suffers.

**The guarantee** is that at most one agent acts at any moment. It holds provided that:

- `acting` is written in the order above;
- the peer list reliably excludes instances that aren't running;
- one agent's write is visible to another's next read (unverified across members);
- every leader-only action is an Incus API call through the agent's own member. Across a partition, nothing in `MayAct` fences the isolated side; Incus quorum does, by refusing its calls. Work outside Incus (a git push, DNS, an external API) has no such fence. It needs its own before an agent may do it, or it waits for the ranked election. One gap remains: an Incus operation already running on the isolated daemon when the partition starts may not be stopped by it.

**The operator's failover:** if the old primary's agent is hung, stop it; then commit the new `primary`. A dead or cleanly stopped primary needs no fencing step. The code holds no timers and detects no failures. The web app already polls, so it alerts on a missing primary heartbeat, and a person acts on it. That's within 0.x's minutes-scale bar.

### Deferred: ranked election over Incus (the automated implementation)

Recorded so it can be built later without re-deriving it. Everything registers against the same `Elector`.

*Idea.* Every agent runs the same pure function over the same `incus list` snapshot; the leader is the highest-ranked (node name, then instance name) member that looks alive. Mutual exclusion is built from single-writer registers (each agent's own `user.*` keys — Dekker/bakery style), so it needs no CAS; timing enters only to recover from a crash.

*Protocol.* Constants (proposals, untuned): settle `S`≈30s, self-fence lease `L`≈60s, takeover dead-time `D`≈5min, tick≈5s.
1. **Claim.** Publish `claim=<rank, epoch>` on your own instance; wait `S`.
2. **Verify.** Read every peer. If a live claim outranks yours, withdraw. Publish-then-read means of two simultaneous claimants at least one sees the other, and the lower rank yields.
3. **Self-fence.** Act only while your last successful heartbeat write is younger than `L`. If Incus is unreachable you stop acting after `L`: the ability to heartbeat and the ability to act are one channel.
4. **Liveness** is judged on the *observer's* clock — a peer is alive if its counter changed within the TTL — so no clock is compared across machines. A freshly started agent stays passive for one TTL.
5. **Takeover of a crashed leader** only after its heartbeat has been unchanged for `D`, with `D` well above `L` plus any pause and drift margin.
6. **Planned handoff is fast.** A draining leader clears its claim and waits out its longest in-flight action; peers may claim immediately. Only a crash costs `D`.
7. **Guard every action** with a hard per-call deadline under `L`, and re-verify immediately before it.

*Guarantee, and its assumptions.* No two leaders unless a process is frozen longer than the margin between its last check and its API call, or clock rates drift beyond it — the assumption every lease system (etcd, Chubby) makes; no protocol in an asynchronous system can promise more. Minutes of idle time buy a very large margin. Before building it, verify what is *assumed*: that one agent's write is visible to another's next read (measure stale reads), and the clustered path.

*Cost.* One small config write per agent per tick and one list read; ~300–500 lines of Go plus a fake clock and simulator; three interacting timing knobs; real-Incus validation that freezes, partitions and kills a leader mid-action. Roughly 5× the code and 10× the validation of `Designated`, which is why it is deferred.

### Consequences

- **The code (#108, #101):**
  - `internal/leaderelection` holds the `Elector` interface and `Designated`. `Designation` is `Primary` plus the `Commit` it came from, since only the sync knows that. `Peer` carries `Commit`, `Acting` and `Draining`; `Registry.Record` publishes a commit and the acting state.
  - `MayAct` performs the claim itself, so a `Leader` answer always means `acting` is published and re-checked. `Stop` is separate, because only the caller knows when its work has stopped. `MayUndrain` is the un-drain gate.
  - `internal/configsync.Clone` is the persistent clone, and backs `Designated.HasCommit`.
  - `internal/agent` is the tick loop, with the drain state machine and acting bookkeeping, so #109's self-upgrade handoff can reuse it; `cmd/agent` only wires it to the environment.
- **Proven against real Incus** by `scripts/validate/agents-act-one-at-a-time-through-failover.sh`. Three agent processes, each claiming a throwaway container's identity, show that exactly one acts; that a `primary` change hands over in about one tick, with no snapshot or log interval showing two acting; and that a stale-checkout agent that believes it's primary stands down, and acts once a control stops the peers that fence it.
- **The published commit is also the diagnostic.** The web app can show each agent's lag ("node2 is 3 commits behind") and surface "Incus-alive but git-stuck" before a failover is needed.
- The `homelab-ops-meta` coordination project, the lease instance, and lease renewal at 1/3 TTL (§17, `docs/AppManager.md`) are dropped. With one designated primary, §17's lease renewal and "stop renewing on self version mismatch" (#109) reduce to the primary being told, by a designation change, to step down.
- The ETag spike script stays as characterization: it guards against re-adopting If-Match as a CAS, and an upgrade that makes it atomic would be news worth noticing (a change in its informational output is not a failure).
- Two-instance colocation on one member no longer "proves the lease"; 0.x validates the designation gate and fencing instead (#103 is reworded accordingly).
- **To verify,** on a real multi-member cluster (#184):
  - that the peer list excludes an offline member's instances, which Incus reports as Stopped or Error;
  - that a member without quorum refuses `GET /1.0/instances`, so a partitioned agent stops at its next tick (a live partition, not only node loss);
  - cross-member read-after-write visibility.

### History

- **2026-09-20 (#160, #108):** replaced §17's ETag lease with `Designated`, fenced by an operator-maintained `Epoch`. Every agent recorded the highest epoch it had seen as a ratchet, and the operator fenced the old primary before raising the epoch.
- **2026-09-20 (#187):** added the sync-failure threshold, self-draining and the un-drain gate. It also rejected git recency as a leadership input, since an unrelated commit would make a peer look "more current" and SHA freshness needs ancestry machinery an `int64` avoids.
- **2026-09-30 (#212): commits and the acting handoff replaced the epoch, superseding #187's rejection of git recency.** The epoch was a second source of truth that had to move in step with `primary`. And as shipped, a change of `primary` at the same epoch wasn't fenced at all, since `MayAct` stood down only for a *strictly higher* epoch, so both nodes acted until the old one synced. #187's objections were answered: being ahead never makes an agent leader, and with a persistent full clone the ancestry check is local. The cost moved from a counter the operator maintains to a clone the agent maintains.
- **2026-10-03 (#101, PR #219):** building it settled the force-push refspec, ancestry as the meaning of "in my history", a failed parse as a failed sync, the own-instance and own-flag checks, peers scoped to the agent's own App, draining peers skipped by rule 2, and un-draining only after a successful sync.
- **2026-10-03 (#223):** the #187, #212 and #101 addenda folded into the body above.

## 26. Incus clustering: 0.x claimed a one-member cluster it never built; splitting the fix into Tier A and Tier B (#177)

Architecture.md and §17 have said since #92's original design that "0.x initializes Incus as a one-member cluster from the start" — deliberately, so the App Manager's leader-election design (§25) would need no later migration to real clustering. Rereading the seed code while asking what remains for 0.x found that this was never built: `internal/seed.renderIncusPreseed` only ever sets `Preseed.Certificates`; the vendored `InitPreseed.Cluster` field is never populated, and a fresh node's Incus is a bare, unclustered daemon. `server_clustered: false` on the current dev host confirms it. The doc's claim and the code have been out of sync since #92 (2026-07-14).

This splits the remaining clustering work into two tiers of very different size, and schedules the small one now.

### Tier A: make the existing claim true — a real one-member cluster (Phase 3, #178) — DONE

What §25's `leaderelection` design and #101's agent already assume — one Incus API surface, reachable identically from wherever an agent runs — needs Incus actually clustered, even at one member. This turned out to be config, but not *quite* as small as the initial spike below predicted; a real boot caught what source-reading missed.

**Spike (source-reading only).** IncusOS's `incus-osd` applies the Incus seed with no clustering-specific gate at all: `internal/applications.(*incus).Initialize` calls `incusSeed.Preseed` straight into `github.com/lxc/incus/v7/client`'s `ApplyServerPreseed` — the exact function `incus admin init --preseed` itself calls. Reading that function (`client/incus_server.go`):
- It applies `config.Config` (server config, e.g. `core.https_address`) *before* it looks at `config.Cluster` — so setting `core.https_address` in our own preseed's `Config` map, not relying on `incus-osd`'s own post-init fallback (which runs *after* `Initialize`'s preseed call), is what makes the ordering work.
- Becoming a one-member cluster is `config.Cluster != nil && config.Cluster.Enabled`, which calls `UpdateCluster(config.Cluster.ClusterPut, etag)` — **bootstrap**, not join: `ClusterAddress`/`ClusterCertificate`/the join token are join-only fields and stay empty. Only `Enabled: true` and `ServerName` are needed.

**What the spike missed, and a real boot caught (CLAUDE.md's issue #5 lesson, exactly on cue).** Setting `core.https_address: ":8443"` — a wildcard bind, and the value every `scripts/validate/*.sh` script already dials — renders and applies fine (confirmed: it showed up correctly in `GET /1.0`'s `config`), but `UpdateCluster` then fails outright:

```
Cannot use wildcard core.https_address "" for cluster.https_address. Please specify a new
cluster.https_address or core.https_address
```

Found by replaying the exact `PUT /1.0/cluster` call `ApplyServerPreseed` makes directly against a real booted node — the VM's console log carries no `incus-osd` application output at all (kernel/systemd text only), so the only way to see the real error was to reproduce the call by hand. A cluster member has to advertise a concrete, routable address for future members to dial; `:8443` names none. Fix: `core.https_address` is the *instance's own static IP*, not a wildcard — `internal/seed.Render` now requires `inst.StaticIP` to compute it.

**Consequence: clustering is conditional on a known static IP, not unconditional.** A DHCP-only `Instance` (no `static_ip`, a supported mode since Roadmap Phase 0 — `TestRenderDHCP` already covered it) has no address to advertise at seed-render time. Erroring in that case would have regressed a real, tested, intentional mode — so `Render` skips the `Config`/`Cluster` preseed blocks entirely when `StaticIP` isn't valid, leaving that node a bare daemon exactly as before this existed. In practice this rarely matters: every `Instance` IPAM actually assigns an address to (the normal path from Phase 2 on) has a `static_ip` already, which is when clustering applies.

The seed change, final shape:

```yaml
incus:
  preseed:
    config:
      core.https_address: "<instance static_ip>:8443"
    cluster:
      server_name: <instance name>
      enabled: true
    certificates: [...]   # unchanged
```

**Verified against a real boot**, not just source-reading: `scripts/validate/node-boots-and-trusts-bootstrap-cert.sh` (extended with two assertions) shows a freshly installed node reporting `environment.server_clustered: true` and exactly one entry in `GET /1.0/cluster/members`, 12/0/0.

**Scope stayed deliberately small.** One member is still Incus's whole scheduler decision (§17's "no placement field" reasoning is unaffected — `Target`/cluster groups still buy nothing with one member). No join token, no membership config, no networks/storage-pool parity work: all of that is Tier B.

### Tier B: real multi-member clustering (new Phase 4)

Growing to N≥2 members is a materially bigger project — Architecture.md's "explicitly out of scope for 0.x" framing was right about *this* half of clustering, just conflated with Tier A under one sentence. It gets its own roadmap phase (the existing Phase 4, "Tailscale, logging + metrics," renumbers to Phase 5 to make room — pure renumbering, no content change, same pattern as #58's v1→0.x rename).

What it needs, roughly in dependency order:

- **Join-token flow.** A second node's seed can't be a pure function of git config the way today's is (`docs/Decisions.md` §13 assumed this) — `InitClusterPreseed` needs a `ClusterAddress`/`ClusterCertificate`/join token minted by the *already-running* cluster at seed-render time. The image route gains a live dependency on cluster state and token expiry it doesn't have today.
- **A membership config model.** Which node is member 1 (bootstrap) vs. joiners, and where that's declared — `kind: Instance`, or a new field/kind.
- **Cluster groups / `Target`.** §17 deleted `App.Node` and left `kind: App` with no placement field because one member makes placement moot. With real members it stops being moot: a `replicas: per-node` agent (or Alloy, #77) needs to land correctly, and `docs/AppManager.md`'s anticipated mechanism (tag members with a capability, target creation at the cluster group) has to actually be built.
- **Storage/network parity across members**, and a 3-VM validate script proving a lost member doesn't take the fleet down with it — Incus's own dqlite fault tolerance needs an odd count ≥3, so this is also what finally lets #92's done-when be re-measured for real (§17's "0.x proves the mechanism, not node-death fault tolerance" caveat is what this phase removes).
- **Member add/remove and quorum-loss recovery** as an operator workflow.
- **Revisit `leaderelection`.** §25's `Designated` and the deferred ranked-over-Incus election are both node-scoped already and don't need rework to keep working across real members; N≥3 real Incus members are what would make automated (ranked) election worth its cost over `Designated`'s operator step, per §25's own trade-off.

Tier B is filed as a tracking issue plus its workstreams, all `later` — nothing here blocks Phase 3.

## 27. Pivot to a real 3-member cluster; the agent joins members; AppManager's app reconciliation paused (2026-09-28)

The project's next goal is a real 3-member Incus cluster (Phase 4), ahead of the rest of #92.

**Why.** An operational cluster now matters more than AppManager. Work so far has run into problems that came from not having a real cluster, and the operator needs a working cluster in any case. AppManager's app reconciliation resumes afterwards, on a real physical cluster, where its HA can be tested for real rather than on one member.

The app-manager agent still ships, but its first real jobs are **cluster membership**, not app deployment:

- joining new members (#180);
- making sure every member runs an agent (#203).

The rest of the blue-green reconcile machinery (#98, #103, #109) is paused, and so is cluster-group placement (#182). An agent that is deployed and does little else is an acceptable Phase 3 end state.

What follows was first read from `lxc/incus/v7` v7.5.1 and IncusOS source, then checked by hand on `homelab-host` in the #193 spike (evidence below). The web app's interim security posture was raised at the same time but is independent of this pivot; it is §28.

### What a join actually does to the joining node

- **The joiner's global database is wiped.** `cluster.Join` removes `GlobalDatabaseDir()` and adopts the cluster's (`internal/server/cluster/membership.go`). Anything recorded there on the joiner — instances, profiles, trusted certificates — is gone afterwards. `clusterPutJoin` also rejects a server that is already clustered (`400 This server is already clustered`), so a node booted from today's seed can't join over the API.
- **So an agent cannot join its own node.** It would be an instance whose record disappears mid-call. The joiner must be driven from outside, and must be empty when it joins.
- **Today's seed does the wrong things on a joiner, whichever way it joins.** `ApplyServerPreseed` applies config, then pools, networks and profiles, then `certificates`, and only then `cluster`. IncusOS's `applyDefaults` runs after the whole preseed.
  - The break-glass cert, the one-shot bootstrap cert and (#99) the `incus-socket` profile are written locally and then discarded by the join.
  - The join needs `member_config` for the `local` zfs pool's `source=local/incus`. No other member-specific key applies to our seeds: there is no physical network, because IncusOS creates one only for a bridge interface with the `instances` role and our `network.yaml` sets `management` only. `incusbr0` has no member-specific keys.
  - **If the join happens from the seed** (a `cluster_token` in the preseed), it runs before `applyDefaults`, which then skips quietly because the cluster's pools already exist. The joiner never gets the `backups`/`images`/`logs` volumes or its `storage.*_volume` settings.
  - **If the join happens over the API after `applyDefaults`,** the join fails outright when given `member_config`: it tries to *update* the existing `local` pool with a member-specific key, is refused, and spends the token. Without `member_config` it succeeds, but the `storage.*_volume` keys (member-local, in the node database) are left pointing at volumes the cluster has no records for, whose ZFS datasets still exist. Only Incus's internal recovery API repairs that. Hence `apply_defaults: false` on joiners.
- **A joiner trusts its seed's certificates before it has finished initializing.** They're applied before `applyDefaults` runs, so a joiner answers `auth: trusted` while still initializing. `GET /os/1.0/applications/incus` → `state.initialized: true` is the signal to wait for, and the joiner cert can read it.
- **Members must match versions.** `Accept` rejects a joiner whose schema or API version differs from the cluster's (`membershipCheckClusterStateForAccept`; not yet exercised). #68 (one flasher-tool/IncusOS pin) helps. But IncusOS updates itself, so members also drift apart depending on when each one updated. The join loop has to tolerate a mismatch (below).

### Decision: an agent on an existing member drives the join

The designated primary's agent (§25) reconciles declared membership (#181) against `GET /1.0/cluster/members` and joins what is missing:

1. **Find the joiner** on its own subnet (below), and wait for `/os/1.0/applications/incus` → `initialized: true`.
2. **Check its version.** Read `GET /1.0` on the joiner. While its version differs from the cluster's, wait; IncusOS's own updates usually close the gap. A version mismatch at `Accept` is retried, not treated as a failure.
3. **Mint the token over its own unix socket** (`POST /1.0/cluster/members {"server_name": <name>}`, through the `incus-socket` proxy, #99). No credential trusted by the cluster exists outside the node, so §4's custody principle holds without new machinery, and the web app needn't mint tokens.
   - The response is an operation, not a token string. The token is `base64(JSON{server_name, fingerprint, addresses, secret, expires_at})`, assembled from the operation's metadata, as the `incus` CLI does.
   - It expires after `cluster.join_token_expiry` (default **3 hours**) and is single-use. A *failed* join still spends it, and leaves the joiner's server cert in the cluster's trust store.
4. **Build `member_config` from live state.** `GET /1.0/cluster` lists the member-specific keys; fill in `local`'s `source=local/incus`. Also read `GET /1.0` → `environment.certificate` over the socket, and check its SHA-256 against the token's `fingerprint`. Most of #183 (storage/network parity) is this step.
5. **Call `PUT /1.0/cluster` on the joiner** over the LAN, and wait on the operation it returns (the joiner cert stays trusted long enough for the wait to succeed). A token alone doesn't join: the body also needs `server_address` (the address discovery found), `cluster_address` (from the token's `addresses`) and `cluster_certificate`. Without `cluster_address`, `clusterPut` takes the request as a *bootstrap*, and the joiner would become a one-member cluster of its own. The minimal body that worked:

   ```json
   {
     "server_name": "joiner-a",
     "enabled": true,
     "server_address": "192.168.1.211:8443",
     "cluster_address": "192.168.1.210:8443",
     "cluster_certificate": "<PEM of member1's cluster cert>",
     "cluster_token": "<base64 token>",
     "member_config": [
       {"entity": "storage-pool", "name": "local", "key": "source", "value": "local/incus"}
     ]
   }
   ```

6. **Fix up the new member.** Four calls against any member, with any cluster-trusted cert. Nothing else is missing: the profile, pools, networks and trust all come from the cluster.

   ```
   POST  /1.0/storage-pools/local/volumes/custom?target=<member>  {"name":"backups","type":"custom","content_type":"filesystem"}
   POST  /1.0/storage-pools/local/volumes/custom?target=<member>  {"name":"images", ...}
   POST  /1.0/storage-pools/local/volumes/custom?target=<member>  {"name":"logs", ...}
   PATCH /1.0?target=<member>  {"config":{"storage.backups_volume":"local/backups","storage.images_volume":"local/images","storage.logs_volume":"local/logs"}}
   ```

**Not from the seed.** Both routes join and leave the same gap, but a seed-time join bakes a 3-hour, single-use token, member 1's address and its cluster certificate into the image. Joining over the API means seeds don't depend on live cluster state. This reverses §26's expectation that a joiner's seed would carry a join token.

**Bring-up order:**

1. Member 1 bootstraps itself from its seed (§26 Tier A).
2. The web app deploys the agent onto member 1 (#100).
3. The agent joins members 2 and 3, and puts an agent on each (#203, below).

**The joiner's seed** skips clustering, profiles, pools and networks, sets `apply_defaults: false` **explicitly** (IncusOS applies defaults when there is no Incus seed at all), trusts the joiner cert (below), and sets `dns.hostname` so the node reports its name (below). The renderer needs the membership model (#181) to know which role an `Instance` has.

```yaml
# network.yaml: as today, plus
dns:
  hostname: <instance name>   # without it, the node reports its machine UUID
# incus.yaml
apply_defaults: false          # explicit: no incus.yaml at all means apply_defaults: true
preseed:
  config:
    core.https_address: "<static_ip>:8443"
  certificates:
    - {name: <name>, type: client, certificate: <break-glass cert>}
    - {name: <name>-joiner, type: client, certificate: <joiner cert>}
# no cluster block, no profiles, no pools, no networks
```

A joiner booted this way comes up with an empty Incus: no pools, no networks, and a `default` profile with no devices. The join replaces its trust store; the break-glass cert keeps working afterwards only because member 1 was seeded with it.

**The agent's scope, in issue terms:**

- Needed: #99, #100 (both the web app route and the `bootstrap deploy-agent` CLI path), #101, #102, #160 and #203.
- Paused: #98 (apart from #203's slice), #103, #109 and #182.

### Decision: every member runs an agent (#203)

Per-node agents come from #98's reconcile loop ("the fleet self-expands", `docs/AppManager.md`). Pausing all of #98 would leave only member 1 with an agent.

The web app can't fill the gap. Its one-shot bootstrap cert is revoked after first use (`internal/nodeprovision`), and the join wipes the joiners' copies, so after the first agent it holds no credential on any member. Losing member 1 would then leave nothing to join a replacement, and no agent for a `Designated` failover to promote.

So #98's zero-match branch is un-paused for the agent App alone (#203). Each tick, the designated primary creates generation 0 of the agent on any declared member that has none. Nothing else of #98 comes with it: no image-change handling, no blue-green, no teardown. Agent upgrades stay an operator step until #98 and #109 resume.

- **Each agent is placed on its own member** (`target=<member>`), since it reaches that member's Incus through the `incus-socket` proxy. This is the first place 0.x sets a target. The target comes from per-node synthesis, not from a declared placement field. So `docs/AppManager.md`'s "no `Target`" rule still holds for declared Apps, and #182 stays paused.
- **The CLI deploy path is no longer deferred.** `bootstrap deploy-agent` (#100) uses the break-glass cert over the LAN and needs no web app. If every agent is lost, nothing is left to recreate one, so this is the recovery path.

### Decision: the joiner credential is operator-generated, for now

The join call (step 5) needs a credential the *unjoined* node trusts. Its seed currently trusts only two certs: the break-glass cert (key held by the operator) and the web app's one-shot cert (key held by the web app). Neither belongs in the agent.

**A dedicated joiner cert.** The operator generates it with `bootstrap gen-cert`.

- Its public half goes into the web app's deployment config, like the break-glass cert, and is preseeded into joining nodes' seeds only.
- Its private half goes to the agents.
- It limits itself by construction. It only works against fresh, unjoined nodes. The join replaces the node's trust store, so each node stops trusting the cert as soon as it joins (confirmed: `auth: trusted` before, `untrusted` after). The exposure window is a node that has booted but not yet joined.

**Not yet decided: how the private half reaches the agents.** The config repo is public, so git is out. Any agent that may become primary needs the key, so a `Designated` failover (§25) shouldn't require pushing it again. #194 decides, alongside the joiner seed variant. Options:

- **Push it into each agent.** The operator uses the break-glass cert to push the key into each agent instance (`incus file push`) or into a custom volume. Simple, but a failover or a recreated agent needs another push.
- **Store it once in cluster state**, e.g. in a `user.*` key in a dedicated project. Whichever agent is primary reads it over its socket, so a failover needs no re-push. This doesn't widen exposure today, because every client the cluster trusts already has full admin rights. A restricted cert added later could read project config, so this would need revisiting then.
- **Put it in a private S3 bucket.** This waits on the deferred secrets overlay (§28).

Deferred alternative: the agent generates the keypair itself and publishes the public half to the web app. That removes the operator step but needs an agent-to-web-app channel that doesn't exist yet.

### Decision: the agent discovers joiners on its own subnet; trust on first use for now

The agent needs a joiner's address for `PUT /1.0/cluster`. But IPAM-assigned addresses live only in the web app's store (§12; git write-back was deferred), and the joiner isn't in Incus yet to ask. Addresses should also stay out of the public config repo (§28).

**Discovery (chosen).** Members cluster over one LAN, so a joiner is on the same subnet as the agent's own node.

- The agent reads that subnet from its host's network state over the Incus socket, so no address ranges are needed in git.
- It probes those addresses on `:8443` with the joiner cert. Only fresh, unjoined nodes trust that cert. An instance behind its member's `incusbr0` NAT reaches a joiner's `:8443` (confirmed).
- It matches a host that responds to a declared `Instance` by `environment.server_name` from `GET /1.0`. Unclustered, that is the OS hostname, which IncusOS sets to the machine UUID unless `network.yaml` sets `dns.hostname`, so the renderer must set it. Only a trusted cert sees `environment` at all.

This needs no web app, no addresses in git and no new credential, and the web app's IPAM stays authoritative. An explicit `static_ip` in a git-addressed network (§28) skips discovery for that node.

Rejected alternatives:

- **The agent asks the web app** for the joiner's address over the tunnel. That puts the web app on the join path through a node-to-web-app call, which breaks the rule below.
- **The web app records pending members in the cluster at render time**, through a restricted Incus cert. That gives the web app a standing cluster credential, which §4 has so far avoided.

**What discovery doesn't prove.** A client cert proves the agent's identity to the joiner. It proves nothing about the joiner to the agent.

- Any host on the LAN can accept the joiner cert and report a declared name, and the agent would then hand it a join token.
- The token is a bearer credential: whoever redeems it becomes a member and receives the cluster certificate's private key.
- A joiner's server certificate is generated on first boot, so there is nothing to pin when the seed is rendered.
- A `static_ip` has the same gap, only narrowed from "whoever answers on the subnet" to "whoever holds that address".

**Interim: trust on first use, accepted for the homelab LAN.** The risk is narrowed three ways:

- mint a token only after discovery has found a candidate, and only one per declared name;
- keep `cluster.join_token_expiry` short;
- raise an alert if the declared node still answers as unjoined after its name has already joined, since that means someone else redeemed the token.

This sits uneasily with §28, which treats anyone on the LAN as a real threat to the web app. It is accepted because joins are rare and the operator starts them, and because the alternative needs a channel that doesn't exist yet.

**Deferred: pin the joiner's server certificate.**

- The WireGuard tunnel is the one channel whose node identity is fixed at render time, since the web app generated each node's key. So the web app can read a joiner's certificate fingerprint from a TLS handshake over the tunnel.
- The open question is how that fingerprint reaches the agent. Either the operator commits it to git beside the instance (one manual step per join; fingerprints aren't secret), or the agent reads it from a read-only web-app route (automatic, but it puts the web app on the join path).
- Either way, the fingerprint would also replace the self-reported server name as the way to match a joiner.

Not decided; revisit with #194.

### The web app must be able to be down without affecting the cluster

The web app is a provisioning and observation plane, never on the cluster's runtime path. Checked against the design above:

- **Cluster traffic** runs member to member over the LAN. The WireGuard tunnels only carry web-app-to-node management, so a down web app idles them and nothing else.
- **The agent** syncs git itself (`docs/AppManager.md`). It discovers and joins members, mints tokens over its own socket, and holds the joiner key. Joins and, later, reconciliation keep working with the web app down.
- **Lost while the web app is down, by design:**
  - rendering seeds and images for new nodes;
  - the web app's `deploy-agent` route (the CLI path still works);
  - the missing-primary-heartbeat alert (`docs/AppManager.md`). That one is monitoring, not function; Phase 5's Grafana stack is its longer-term home.
- **Rule for future work:** nothing on a node or in the agent may call the web app synchronously. If a feature seems to need that, it belongs in git or in the agent.

### Operations Center, again

`docs/Architecture.md` said wrapping Operations Center would be revisited "once real multi-member clustering (Phase 4) is actually on the table". It now is.

- Operations Center already clusters IncusOS nodes and generates the seeds for joining them, which overlaps with most of this section.
- §0's other objection still stands. Operations Center needs a trusted cert in its own seed before it will talk to anyone, so it moves the node #0 bootstrap problem rather than removing it.

It's worth an hour's read of how Operations Center joins members before building #180, if only to borrow its answers to the questions above.

### Evidence: the manual join spike (#193, 2026-09-29)

Run by hand on `homelab-host`. Every node booted IncusOS **202609271243** (the stable channel's current release, straight from the CDN) with Incus **7.5.1** (562 API extensions). The versions matched at every join. IncusOS checked for updates during the run and found nothing newer, so the version-skew path wasn't exercised.

Four nodes were booted, all on `home-lan`, and all three joiners joined (`Online`, `database-standby` in member1's `GET /1.0/cluster/members`):

- **member1** from today's seed (§26 Tier A), listed alone as `database-leader`;
- **joiner-a**, the joiner seed above without `dns.hostname`, joined over the API;
- **joiner-b2**, with `apply_defaults: true`, no `cluster` block and `dns.hostname: joiner-b2`, joined over the API;
- **joiner-seed**, joined at seed time from a `cluster` preseed carrying a `cluster_token`.

| Claim (first read from source) | Result | Evidence |
|---|---|---|
| The join wipes the joiner's global database | **Confirmed** | joiner-a's two preseeded client certs were gone afterwards. Its trust store became the cluster's: the member server certs plus member1's break-glass entry. Its `default` profile became the cluster's. |
| `clusterPutJoin` rejects an already-clustered server | **Confirmed** | `PUT /1.0/cluster` with a join body against member1: `400 This server is already clustered`. |
| A seed-time join leaves the joiner without volumes and `storage.*_volume` keys | **Confirmed** | joiner-seed had neither. IncusOS still reported the app `initialized: true`, so `applyDefaults` skipped quietly rather than failing. |
| An API join after `applyDefaults` keeps the `storage.*_volume` keys but loses the volume records | **Confirmed, and worse** | joiner-b2: with `member_config`, `Failed to update storage pool "local": Config key "source" is cluster member specific`, which spent the token. Retried with a fresh token and no `member_config`, it joined, and `incusbr0`'s addresses were overwritten with the cluster's. The keys survived; recreating the volumes failed (`dataset already exists`). `POST /internal/recover/import` with `{"pools":[{"name":"local","driver":"zfs","config":{"source":"local/incus"}}]}` on the joiner re-imported all three. |
| `member_config` is needed for the `local` pool's `source` | **Confirmed, for an empty joiner** | member1's `GET /1.0/cluster` listed `local`'s `source` and `zfs.pool_name`, both empty. Supplying `source=local/incus` alone was enough for joiner-a and joiner-seed. |
| `member_config` is needed for a physical network's `parent` | **Wrong for our seeds** | No physical network exists; see above. |
| Members must match versions | **Not exercised** | Every node ran the same release. |
| The joiner cert limits itself | **Confirmed** | Before: `auth: trusted` on joiner-a. After: `auth: untrusted` on both joiner-a and member1. |
| An instance behind member1's `incusbr0` NAT can reach a joiner's `:8443` | **Confirmed** | An Alpine container on member1 (`10.21.89.249`) fetched `https://192.168.1.211:8443/1.0` with the joiner cert and got `auth: trusted`. |

Also found:

- **The join body:** removing fields one at a time from joiner-a's body gave `400 No target cluster member certificate provided` without `cluster_certificate`, and `400 No server address provided for this member` without `server_address`. Removing `cluster_address` wasn't tried; the bootstrap reading is from source.
- **Server names:** joiner-a (no `dns.hostname`) reported `5ae89e64-7325-4ef7-91cb-3eb434705b04`; joiner-b2 reported `joiner-b2`.
- **Initialization:** joiner-b2's first trusted `GET /1.0` had no `storage.*_volume` keys; they appeared seconds later.
- **The fix-up** fixed both joiner-a (API route) and joiner-seed (seed route). On joiner-a, a container launched with `--target joiner-a` afterwards got an address from joiner-a's own `incusbr0`.
- **Every member's `incusbr0` gets the same subnet.** `ipv4.address` isn't member-specific, so each member runs its own NAT'd bridge on the cluster-wide subnet (`10.21.89.1/24` here). Normal for Incus clusters, and harmless, since the bridges never meet.
- **Removing a fixed-up member takes more than one call.** `DELETE /1.0/cluster/members/joiner-a` refused while it held custom volumes (`Node still has the following custom volumes: backups, images, logs`). After unsetting the three keys, deleting `logs` failed with `dataset is busy`, because it stays mounted as Incus's log directory until the daemon restarts. `?force=1` removed the member, its volume records and its server cert. Worth recording in the runbook (#185).
- **IncusOS tags `local/incus` with `incusos:use=incus`** when `applyDefaults` creates it. A joined member's dataset, created by the join instead, lacks the tag. From IncusOS source it is only reported in `/os/1.0/system/storage` state, so the gap is cosmetic.
- **An IncusOS VM takes a fully allocated 50 GiB.** The installer refuses a smaller disk and wipes the whole target. On `homelab-host`'s btrfs pool that allocates all 50 GiB, and a first attempt at this spike filled the pool. Any multi-VM validate script (#184) needs about 55 GiB per node.

**No characterization script was kept.** The behaviours worth guarding are a real join and its fix-up. A script for them needs two IncusOS VMs (about 110 GiB) and about 20 minutes. It belongs with #180's agent-driven join, which exercises exactly this path, rather than as a standalone guard on hand-written calls the agent will replace.

### History

- **2026-09-28 (#202):** the pivot, with the join mechanics read from source and a manual join spike (#193) required before building any of it.
- **2026-09-29 (#193):** the spike confirmed the source reading, except that no physical network needs `member_config`. It added what a join body needs beyond the token, how the token is assembled, the wait for `initialized`, `dns.hostname` on joiners, and the four-call fix-up.
- **2026-10-03 (#223):** the spike addendum folded into the body above; its evidence kept as its own subsection.

## 28. The web app's interim security posture: WireGuard API by default, one operator-held key (2026-09-28)

Raised alongside §27 but independent of it: none of this blocks Phase 4. It is scheduled in `docs/Roadmap.md` § Web app hardening (#195–#200). Proper auth is deferred (end of this section); this is what stands in for it meanwhile.

**The problem.**

- `POST /instances/{name}/seed` and `GET /instances/{name}/image` are unauthenticated (#63).
- Every seed contains that node's WireGuard **private** key (`internal/seed`'s `network.yaml`).
- Compose publishes the API on every interface of the host.

So anyone who can reach the web app can fetch a node's key and pose as that node on the tunnel. The store also holds every instance's credentials and its IPAM history in plaintext, with no backup (§12 already noted it "needs a backup/migration story it doesn't have today").

### Decision: serve the API over WireGuard to operator peers by default; a host listener only by explicit opt-in (#195)

By default, the HTTP API moves off the host port and onto the web app's existing in-process tunnel. Which listeners serve it is an operator setting (§29), and tunnel-only is the default. (First decided as tunnel-only with no alternative; see History.)

- **How.** `internal/wireguard` already runs a userspace netstack, whose `netstack.Net` provides `ListenTCP`. The same `http.Handler` is served on `WebAppAddr` (`10.100.0.1`) inside the tunnel, so it still needs no TUN device and no `NET_ADMIN`. Only the WireGuard UDP port stays exposed on the host.
- **The operator becomes a peer.** The operator generates their own WireGuard keypair. Its public half reaches the web app through deployment config, like the break-glass cert (§4); the private half never leaves the operator. Several devices means a list of keys. When sync reconciles tunnel peers, it must keep the operator peers, not just instances.
- **Nodes are peers too, so filter by source address.** WireGuard drops any packet whose source address isn't in the sending peer's `AllowedIPs`. So a connection's source overlay address reliably identifies which key sent it. Middleware admits only operator overlay addresses. Nodes, and anyone holding a node's leaked key, are refused.
- **What this buys.** Mutual, key-based authentication and encryption in transit for the operator, with no auth code beyond an address check.
- **What it doesn't.**
  - No multi-user support, and no per-person audit beyond "which key".
  - Browsers treat plain HTTP over the tunnel as insecure, so anything that needs a secure context waits for TLS.

  Full auth comes back when the web app must be reachable off the tunnel or by more than one person.
- **The host listener: an explicit opt-in.** An operator setting also serves the API as plain HTTP on an address the operator chooses, such as localhost or one LAN interface. It is off unless set, and has no default bind address. When it's on, startup logs a warning naming the exposure: over it, the seed and image routes hand out node WireGuard private keys to anyone who can reach that address. The source filter above applies only to the tunnel listener. TLS on the host is the expected third mode, not built yet.
- **Dev and validate.** `docker compose` and the `scripts/validate/` web-app family call `:8080` directly today. They will either:
  - join as a peer (`cmd/validate-tunnel-harness` already dials through the tunnel in-process), or
  - opt in to the host listener, bound to localhost.

  At least one validate script exercises the tunnel-only default. `/healthz` stays on a local port for container health checks.

### Decision: a CLI first, no web UI yet (#196)

The web app's API is small (sync, status, list networks and instances, seed, image) and nearly read-only, because desired state is edited in git, not in the app. There is one operator. A CLI subcommand group fits that: in the existing `bootstrap` binary, or a sibling `cmd/` binary. It:

- authenticates by being a WireGuard peer — the address check above. No mTLS, sessions or OIDC;
- streams multi-GB image downloads straight to a file or device;
- decrypts downloads locally, where the key is (below);
- is scriptable from `scripts/validate/`.

Incus's own web UI already covers cluster- and instance-level views. A UI of our own would earn its place later, for at-a-glance fleet status: sync warnings, tunnel handshakes, cluster membership, agent health.

### Decision: one operator-held symmetric key; the store encrypted at rest (#197)

The operator holds one symmetric key: a random 256-bit key generated by the CLI, not a passphrase. If a passphrase is ever wanted, derive the key with `x/crypto/argon2`. The web app only ever keeps the key in memory, and everything the web app writes or serves is encrypted with it.

- **Separate verbs, so a mistake can't destroy state:**
  - `init` sets the key when no store exists yet.
  - `unlock` supplies the key after a restart. It must decrypt the existing store; a wrong key is an error, and nothing is written.
  - `rotate` takes the old and new keys and re-encrypts.
  - `reset` is for a lost key. It must be asked for explicitly, asks for confirmation, and moves the old ciphertext aside rather than deleting it.
- **Sealed on restart.** A restarted web app serves only its unlock route, to operator peers, until it is unlocked — the same model as Vault's unseal. That is acceptable only because the cluster doesn't depend on the web app (§27).
- **The live store is encrypted at rest.** It is small and rarely written. So the web app holds the working copy in memory (sqlite `:memory:`, or a `VACUUM INTO` round-trip) and writes the whole file back, encrypted, after each change.
- **Format: `filippo.io/age`, not a hand-rolled one.** Multi-GB images can't be sealed as one AEAD message, because nothing could be authenticated until the end. They need a chunked streaming construction with a final-chunk marker, so truncation is detected. age's payload format is exactly that, and it has a symmetric mode. Encryption formats are where hand-rolled crypto usually goes wrong.
- **A reset's blast radius.** Losing the key loses the stored credentials and the IPAM history. The tunnel identity survives (next decision). Management of existing nodes is lost until they're re-enrolled, but the cluster keeps running.
- **Accepted:** the web app can read everything whenever it is unlocked. That's fine while it is the operator's own single-tenant tool. Asymmetric encryption, which would let it write backups it can't read, is deferred (below).

### Decision: the web app's WireGuard identity is operator-supplied deployment config (#197)

The tunnel's own key can't be inside the encrypted store. The operator unlocks over the tunnel, so the tunnel has to come up before the key arrives. The identity follows the break-glass cert's pattern (§4) instead:

- The operator generates the WireGuard identity once and keeps their own backup of it.
- The web app reads it from deployment config, alongside the operator peer list, and never generates it.
- Snapshots don't include it, so it is the one secret on the web app's disk in plaintext. That key alone grants no Incus access, since each node's API still requires a trusted client cert.

**Migrate, don't regenerate.** Today's identity lives in the store's `wireguard_identity` table, and its public half is baked into every node's seed. It must be exported to the new config file once. Regenerating it would break every node's tunnel until that node is reflashed.

### Decision: encrypted downloads and snapshots (#198, #200)

WireGuard protects the transfer, but the aim is that plaintext secrets never land on the operator's disk.

- **Seeds and images** come back encrypted under the key; the CLI never sends the key with a download request. The CLI decrypts as a stream straight onto the install media, so no plaintext copy touches a filesystem. age's chunk authentication catches a truncated or altered download.
- **Store snapshots** are a consistent copy (sqlite `VACUUM INTO`), encrypted the same way and downloadable from a route. One is taken after each change. The store changes rarely (on sync, IPAM assignment and credential minting), so this gives near-zero data loss without Litestream's continuous WAL shipping.
- **Optionally pushed to S3 (#200).** An off-cluster bucket (Backblaze B2, Cloudflare R2 or similar) means that losing the cluster and the web app together is recoverable.
- **No separate signature.** age authenticates the payload, and only the operator and the web app hold the key, so a signature would protect against nobody new. Signing comes back with asymmetric encryption, if that is ever adopted.
- **Restore:**
  1. start a fresh web app with the same deployment config, including the WireGuard identity;
  2. `init` it with the same key;
  3. load the snapshot with the CLI. The web app decrypts it in memory.

### Decision: network addressing from git or from web app state, one source per network (#199)

The config repo is public, and addresses in it describe the operator's LAN. Git always keeps each `kind: Network`'s *name*, which is what `Instance.network` references. The addressing — `cidr`, `gateway`, `dhcp_excluded_range`, `dns` — comes from exactly one of two sources per network.

- **Exactly one source per network.** A `kind: Network` in git carries either all of the addressing fields or none of them.
  - All of them: the network is git-addressed, for a private repo.
  - None: the network is state-addressed, and its addressing lives in the web app's state.
  - A partial set is a validation error.
- **A conflict is an error, never a precedence rule.** If git and state both address the same network, the sync (or the load) is rejected, the same stance as #52's "no silent last-wins". The operator removes one side.
- **`static_ip` follows its network's mode.** In git it is valid only for a git-addressed network. Pins for a state-addressed network live in state, so a public repo never carries an address.
- **One resolve step.** Before validation, IPAM and seed rendering, the web app joins git's network names with the addressing from whichever source holds it. Everything downstream sees one resolved `Network` and doesn't know its mode.
- **Where state addressing comes from.** Interim: a deployment-config file on the web app, like `CLIENT_CERT_PATH`. Target: loaded through the CLI into the encrypted store, once #197 lands.
- **What moves with it.**
  - The cross-field checks in `config.Validate` that need addresses move from git sync into the resolve step: static IP inside the CIDR and range, gateway collision (#53), duplicate names (#52).
  - Changes to state-addressed networks are reported when they're loaded, since `configdiff` doesn't see them.
  - IPAM (§12, `docs/Ipam.md`) is otherwise unchanged. It already assigns from a store, not from git directly.
- **Optional guard against leaking addresses into a public repo.** Repo visibility can't be detected through a plain git transport. So git-addressed networks could require an explicit deployment flag (e.g. `CONFIG_REPO_PRIVATE=true`); without it, git addressing is rejected rather than silently published.
- **Unaffected:**
  - The agent. It discovers joiners from its host's subnet (§27) and never reads addressing. A private repo does mean agents need a read-only deploy key, since they sync git themselves.
  - The bootstrap CLI. It reads a local file, which can keep full `kind: Network` documents.
- **Cost.**
  - The resolve step and the all-or-nothing rule are small.
  - The real cost is validation coverage: the web-app validate scripts should exercise both modes.
  - Addressing held in state is lost if both the key and the backups are lost. It is easy to re-enter, since the operator knows their own LAN.

### Considered and deferred

- **Proper auth and a JS client** (sized 2026-09-28).
  - Auth alone is roughly three to four issues: TLS; OIDC login (Authorization Code + PKCE via `coreos/go-oidc`, with the server holding the login so the browser only ever gets a session cookie); session and CSRF middleware; an identity provider (Dex) in compose for validation; and device-code or mTLS access for the CLI.
  - The JS client is the larger cost. The repo has no Node toolchain, build stage or CI job yet, and every screen adds to it.
  - Faster routes, if auth is wanted sooner: an auth proxy in front (oauth2-proxy, Caddy `forward_auth`) with almost no Go changes, or tailnet identity headers once Tailscale lands (Phase 5).
- **Asymmetric encryption (GPG or age X25519).** This would let the web app write backups it can't read, and snapshot signing belongs with it.
  - GPG fits best if the operator already has a key, especially on a hardware token: one key both decrypts backups and verifies signatures. Its Go dependency is the cost. `golang.org/x/crypto/openpgp` is frozen and deprecated, and the maintained `github.com/ProtonMail/go-crypto` is heavier than this repo usually accepts (Development Conventions' "small dependency" rule).
  - age X25519 plus stdlib `crypto/ed25519` is lighter, and age can encrypt to an SSH key the operator already has.
- **OIDC.** Incus supports OIDC natively (`oidc.issuer`, `oidc.client.id`, `oidc.audience`, optionally OpenFGA for authorization). An identity provider could replace much of this project's cert handling: short-lived, revocable tokens instead of one-shot certs (§4, `internal/nodeprovision`) and the joiner cert; human login to Incus; and a real answer to #63. Open questions before a spike:
  - **Where the provider runs.** Inside the cluster is circular: the cluster would depend on an identity provider it hosts, so losing the cluster loses auth. Running it alongside the web app keeps the dependency where one already exists. An external hosted provider adds an internet dependency, though node0 already needs the internet (§23). The break-glass cert stays regardless, as the recovery path when the provider is down.
  - **Machine identities.** Incus's OIDC support is designed around user login. Whether it accepts client-credentials tokens for the agent and the web app is unverified.
  - **Joiners.** OIDC config is cluster-wide, so it arrives with the join. A fresh unjoined node would still need its own seed-time trust, so OIDC shrinks the joiner-cert problem rather than removing it.
  - **Candidates**, lightest first: Dex, Pocket ID, Kanidm, Authelia; Zitadel and Keycloak are heavier.
- **An S3 secrets overlay on top of git.** Not a replacement: git gives history, review, and the commit-to-commit diff that config sync and §25's epoch fencing rely on. The gap is that the public repo can't declare anything secret: the joiner cert's private key, a future OIDC client secret, a Tailscale authkey (#76). A private S3 bucket read beside git would fill it. It's worth spiking together with #200, since both need the same bucket, credentials and encryption story.

### History

- **2026-10-03 (#247, #195).** The tunnel-only API became the default rather than the only mode. An explicit, opt-in host listener on an operator-chosen address replaced the earlier dev-only localhost flag, and TLS was named as a later third mode. Why: the web app now runs on appliance machines in several topologies (§29), and every deployment setting is an operator choice with a safe default.

## 29. The web app runs on an appliance machine; every deployment setting is an operator choice (2026-10-03)

Resolves §9's open question for 0.x. The web app runs on its own machine, outside the cluster it manages, which fits §27: the cluster must keep running while the web app is down or locked. That machine is either:

- **an internet-reachable VM.** Nodes behind home NAT dial out to it over the tunnel, which is the topology #91 built for and `node-tunnel-survives-nat-and-provisions.sh` proves; or
- **a spare machine on the same LAN as the nodes,** such as a laptop.

The operator reaches it in both cases.

### Decision: one image, two host forms

Both forms run the same published OCI image (#240) with the same deployment config directory (#241), so moving between them is a migration, not a rebuild.

- **A dedicated Docker box first (#243).** A production compose file, plus a runbook per topology. It works the same on a VM and a laptop, and it is the deployment target §9 already chose. The host OS is the operator's to patch.
- **An IncusOS turnkey host later (#246).** The bootstrap CLI renders a seed for a standalone IncusOS machine, which isn't a cluster member. `bootstrap deploy-web` then runs the web app on it as an Incus OCI container, reusing #100's `deploy-agent` mechanism. Pointing the same command at a cluster member is how the web app later moves into the fleet (§9).
  - Unverified, to settle in #246: whether a typical spare laptop meets IncusOS's hardware needs (TPM 2.0 and Secure Boot unless degraded, §6; Wi-Fi support), and whether a given cloud provider will boot IncusOS.
- **Rejected for now: a custom mkosi-built appliance image.** It is the same tool family as §15's helper OS, none of which exists yet, and it is the most work for the least reuse.

### Decision: every deployment setting is an operator choice, with a safe default

No single fixed posture fits a public VM, a LAN laptop and a later move into the fleet. So each setting is deployment config with a secure default, documented in #241's contract:

| Setting | Default | Alternatives |
| --- | --- | --- |
| Host form | Docker box (#243) | IncusOS turnkey host (#246); plain binary + systemd (§9's other target, not yet planned) |
| API listener | Tunnel only, operator peers (§28, #195) | Opt-in plain-HTTP host listener on a chosen address; TLS later |
| Where install media is built | Server-side, when `BASE_IMAGE_PATH` is set (today's image route) | Client-side image build from a downloaded seed; a `SEED_DATA` stick beside a stock IncusOS image (#242) |
| node0 provisioning | By the web app over the tunnel (#244) | Bootstrap CLI, offline, no tunnel (today's Flow A) |
| WireGuard endpoint | A DNS name | An IP address |
| Snapshots | Local | Local plus off-cluster S3 (#200) |

- **Install media.** On an internet VM, server-side builds mean a multi-GB transfer across the internet per node, plus VM disk for each copy. The seed is a few KB.
  - IncusOS reads a user-provided FAT or ISO volume labelled `SEED_DATA` beside an unmodified install image (upstream `doc/reference/seed.md`). The stick must be removed after install.
  - Both client-side options need a base image on the operator's machine, which #206 currently blocks `flasher-tool` from downloading.
- **node0.** Both paths call the same `seed.Render`. A web-app-rendered seed already sets the node up as a one-member cluster (§26 Tier A) and adds the tunnel, so making the web app the default is mostly a change of order: the appliance first, then node0. The bootstrap CLI stays for offline bring-up, and for the IncusOS turnkey host itself.
- **The endpoint.** `WIREGUARD_ENDPOINT` is baked into every node's seed, so an IP address pins the web app to one host. IncusOS accepts a hostname (`networkd_validate.go` checks it with `net.SplitHostPort`) and renders it into systemd-networkd's `Endpoint=`. Unverified: whether a running node re-resolves the name after the address changes. #245 probes it.

### Decision: migration first; HA later, as a warm standby

- **Migration is §28's restore path (#245):**
  1. the same deployment config, including the operator-supplied WireGuard identity (#197);
  2. `init` with the same key;
  3. load the latest snapshot (#198);
  4. repoint the endpoint's DNS name.

  No node is reflashed. This covers laptop to VM, Docker box to IncusOS, and the later move into the fleet.
- **HA, when wanted, is a warm standby with manual failover, not active/active.**
  - Two live instances sharing one WireGuard identity would fight over node endpoints, because WireGuard roams each peer to whichever address it last heard from.
  - The store is single-writer SQLite.
  - So a standby restores the latest snapshot and takes over when DNS or a floating IP flips. That is the same shape as §25's designated primary, and §27 already accepts web app downtime.
  - Decide this after #245 works; it isn't scheduled.

### Considered and deferred

- **TLS as a third API listener mode.** It comes with, or before, any web UI (§28's deferred auth).
- **The bootstrap CLI rendering tunnel config for node0.** Once #197 makes the web app's identity operator-supplied, the CLI knows its public key offline. The node's own keypair would still have to be imported into the web app's store. Not needed while the web app provisions node0 by default.

## Sources consulted

- [Incus — REST API ("PUT vs PATCH": ETag / `If-Match`)](https://linuxcontainers.org/incus/docs/main/rest-api/) (§25: the documented contract is lost-update protection for one client's GET-then-PUT, not exactly-one-winner)
- [Incus — `ApplyServerPreseed`, `client/incus_server.go`](https://github.com/lxc/incus/blob/main/client/incus_server.go) and [IncusOS — `incus-osd/internal/applications/app_incus.go`](https://github.com/lxc/incus-os/blob/main/incus-osd/internal/applications/app_incus.go) (§26: the exact call path a seed's `cluster:` preseed goes through, and why `core.https_address` must be set via `Preseed.Config`, not left to `incus-osd`'s own post-init fallback)
- [Incus v7.5.1 — `cluster.Join`/`Accept`](https://github.com/lxc/incus/blob/v7.5.1/internal/server/cluster/membership.go), [`clusterPutJoin`](https://github.com/lxc/incus/blob/v7.5.1/cmd/incusd/api_cluster.go) and [member-local server config](https://github.com/lxc/incus/blob/v7.5.1/internal/server/node/config.go) (§27: the join wipes the global database, rejects a version mismatch, and keeps member-local `storage.*_volume` keys)
- [IncusOS — Installation seed reference](https://linuxcontainers.org/incus-os/docs/main/reference/seed/)
- [IncusOS — System security](https://linuxcontainers.org/incus-os/docs/main/reference/security/)
- [IncusOS — Operations Center application](https://linuxcontainers.org/incus-os/docs/main/reference/applications/operations-center/)
- [IncusOS — Download / flasher tool](https://linuxcontainers.org/incus-os/docs/main/getting-started/download/)
- [Operations Center source (FuturFusion)](https://github.com/FuturFusion/operations-center)
- [Nomad — `update` stanza](https://developer.hashicorp.com/nomad/docs/job-specification/update) (#92's blue-green reconciliation model — see `docs/AppManager.md`)
- [Kubernetes — Deployment strategies (Recreate vs. RollingUpdate)](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#strategy)
- [Argo Rollouts — BlueGreen strategy](https://argo-rollouts.readthedocs.io/en/stable/features/bluegreen/)
- [Kubernetes — Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/) (§18: the precedent for code-per-app on classes 3/5, vs. declarative probes/hooks for the rest)
- [CloudNativePG](https://cloudnative-pg.io/) and [Zalando postgres-operator](https://github.com/zalando/postgres-operator) (§18: what the ecosystem converged on for class-3 Postgres, rather than declarative config)
- [Patroni — DCS requirements](https://patroni.readthedocs.io/en/latest/) (§19: the external-DCS dependency Galera/CockroachDB avoid)
- [Kubernetes — version skew policy](https://kubernetes.io/releases/version-skew-policy/) (§19 / `docs/AppClasses.md`: why "the image string differs" is too coarse a signal for classes 3/5)
- [`systemd.netdev(5)` — `[WireGuard] RouteTable=`](https://manpages.ubuntu.com/manpages/noble/man5/systemd.netdev.5.html) (§24: "Defaults to false" — with it off, `AllowedIPs` install no routes, which is the whole of #157)
- [`systemd.network(5)` — `[Address] AddPrefixRoute=`](https://manpages.ubuntu.com/manpages/noble/man5/systemd.network.5.html) (§24: "Defaults to true" — why the address's prefix length is the available lever)
- [IncusOS — seed reference (`doc/reference/seed.md`)](https://github.com/lxc/incus-os/blob/main/doc/reference/seed.md) and [`incus-osd/internal/network/networkd_validate.go`](https://github.com/lxc/incus-os/blob/main/incus-osd/internal/network/networkd_validate.go) (§29: a user-provided `SEED_DATA` volume beside a stock install image; the WireGuard peer `Endpoint` accepts a hostname)
