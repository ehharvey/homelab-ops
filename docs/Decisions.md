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

## 9. Where does the web app itself run? (flagged as open per your earlier answer)

- Inside the homelab it manages (a container/VM on one Incus host) — needs a bootstrap path for node #1 before the app exists to generate its installer.
- Or a separate always-on box outside the cluster — avoids the chicken-and-egg problem, adds "one more thing to maintain" outside the managed fleet.
- This also interacts with §0: if Operations Center is the bootstrapper, does *this* app only need to exist after the first node is already up?

### Answers
~~Dev environment on local K8s cluster~~ — superseded by #18: no k8s dependency. Dev uses Docker Compose; deployment targets are a Docker image and a plain binary. Later, a migration path to running inside the IncusOS-managed fleet itself is wanted (see `Architecture.md` § Web app).

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

**Local dev/test stack:** requiring live Grafana Cloud credentials for every `scripts/validate-*.sh` run (or every dev loop) is exactly the friction the repo's other dev fixtures avoid (`dev/git-fixture` for config sync). So a local stack — Grafana + Loki + Prometheus (with `--web.enable-remote-write-receiver`) under `docker-compose.yml`, mirroring the existing `web`/`config-repo` services — is always at least one of the configured destinations for `scripts/validate-*.sh`'s Phase 3 checks: no cloud credentials needed for that destination, fully automatable, torn down after each run. A real Grafana Cloud check (with the local stack configured as an *additional* destination, or on its own) remains available as a separate, credential-gated step (same gate-and-skip pattern as `INCUSOS_BASE_IMAGE`/`BASE_IMAGE_PATH` for real-VM tests) rather than the thing CI/every contributor has to run.

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
  `scripts/validate-issue-92.sh` (#103) for how this is validated today.
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

**OVN vs. plain bridges.** Incus 6.0.4 only supports OVN-type networks inside
non-default projects (#96), and OVN would allow per-project network ACLs to
keep CI workloads off the real LAN.

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

Counterweight, recorded rather than buried: `validate-issue-91.sh` at 544
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
*is* the developer's machine: `validate-sprint-3.sh` already warns against
running concurrently with `validate-issue-5.sh` (both drive the live `home-lan`
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
bridges, which `validate-issue-91.sh`'s `wan91-$$` does today. Against that,
OVN would add OVS, `ovn-northd`, `ovn-controller` and OVSDB as an always-on
failure domain to the machine every PR now depends on. Note also that
`features.networks=false` *inherits* the default project's networks rather
than losing them — measured on `homelab-host`, `user-1000` sees `home-lan`
while `homelab-dev` (`features.networks=true`) sees nothing, which is #96's
trap — so project scoping needs no OVN either.

**When this gets revisited:** OVN earns its place when parallel runs need
overlapping IP ranges or genuinely isolated L2 — simulating two separate
homelabs at once. Unique bridge names per run do not need it; identical
subnets per run do. It also flips if the bridge-ACL control cannot be made to
hold, since that protection is *structural* (the ACL and its network live in a
project the CI cert cannot reach) rather than enforced by a dedicated
"may not edit ACLs" restriction, which does not appear to exist.

## Other notes
1. Track the commit hash nodes are running
2. Some phone home functionality could be a nice-to-have if this has low development cost. I.e., could the node phone the dev instance over tailscale to indicate success (and provide a manifest of it's hardware)?

## Sources consulted

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
