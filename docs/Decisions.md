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
Not yet decided — revisit when Phase 2/3 needs nodes to talk back to the app.



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
authoring per node. See `docs/AppManager.md` § `kind: AgentConfig`.

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

- **Incus's own dqlite fault-tolerance needs an odd member count ≥3.** A
  1-2 member cluster still gets this design's *agent* HA benefit once
  more than one member exists, but the Incus control-plane itself stays a
  single point of failure until then. Not a blocker for 0.x (one member
  anyway) — revisit when a real multi-member cluster is on the table.
- **Leader-to-node partitions.** If the leader loses reach to a specific,
  still-alive node, that node's Apps go unreconciled until the partition
  heals or the lease moves to an agent that can reach it. Accepted,
  consistent with this project's existing minutes-scale-RTO tolerance
  (`docs/AppManager.md`'s Prior-art section already frames this project's
  HA bar that way).

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
