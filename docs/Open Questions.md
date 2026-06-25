# Open Questions — Homelab Ops Web App

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

## Other notes
1. Track the commit hash nodes are running
2. Some phone home functionality could be a nice-to-have if this has low development cost. I.e., could the node phone the dev instance over tailscale to indicate success (and provide a manifest of it's hardware)?

## Sources consulted

- [IncusOS — Installation seed reference](https://linuxcontainers.org/incus-os/docs/main/reference/seed/)
- [IncusOS — System security](https://linuxcontainers.org/incus-os/docs/main/reference/security/)
- [IncusOS — Operations Center application](https://linuxcontainers.org/incus-os/docs/main/reference/applications/operations-center/)
- [IncusOS — Download / flasher tool](https://linuxcontainers.org/incus-os/docs/main/getting-started/download/)
- [Operations Center source (FuturFusion)](https://github.com/FuturFusion/operations-center)
