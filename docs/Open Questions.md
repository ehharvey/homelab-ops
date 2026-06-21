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
Yes, we should use Operations Center and thinly wrap it since it provides an API.

Another challenge is bootstrap process, since operations-center will not exist yet. So this may wrap Operations Center *if/once it exists*, though this in itself can remain an open question since much is still TBD for later nodes.

## 1. GitHub-sourced config (note 2)

- One repo or one-per-environment? Public, private (needs a deploy key/PAT), or both?
- Pull model: poll on an interval, manual "sync" button, or webhook-triggered?
- What's authoritative if the repo and the running state disagree — does the app reconcile (GitOps-style, like Flux/ArgoCD) or just diff-and-warn?
- Validation/dry-run before applying a pulled config to real hardware?
- Rollback story if a pulled change breaks a node?

### Answer
1. To start with, one repo per environment. A later iteration could include sharing mechanism (e.g., 2 environments share a repo but different branches / commits)
2. Public to start, private later
3. Just diff-and-warn for now
4. This will happen later, do nothing for now
5. This will be decided later, do nothing for now

## 2. Bare-metal instance definitions via YAML (note 3)

- What identifies a physical machine in the YAML — MAC address, serial/asset tag, a manually assigned name? How does the app match a booting machine to its definition?
- Does one YAML file = one machine, or one file describing the whole fleet?
- Does this schema double as (or wrap) an IncusOS install seed (`install.yaml`/`network.yaml`/`applications.yaml`), or is it a separate higher-level format the app translates into seeds?
- Multi-node clusters vs. independent single-node hosts — in scope for v1?


### Answers
1. MAC address to start
2. K8s-style. Essentially where all objects are discriminated by a `kind:` key. This gets merged together
3. Higher-level format. Should be simpler
4. v1 should focus on single-node hosts. We will evaluate multi-node afterwards

## 3. USB installer generation (note 5)

- Generated via the **flasher-tool** (Go CLI, `go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool`) invoked by the app, or via Operations Center's built-in image generation (see §0)?
- ISO vs. raw `.img`? Per the docs, the ISO is *not* hybrid — if these need to boot from a USB stick, you need the `img` format, not `iso`. Worth confirming since the original note says "ISOs to download."
- Per-instance image means baking that machine's seed (cert, network, install target) into the image at generation time — where do generated images live, and how does the user get them onto a USB stick (web download + `dd`/Rufus, or does the app drive that step too)?
- Storage/cleanup policy for generated images (regenerate on demand vs. cache)?

### Answer
1. Unless there is another way, we will need the flasher for the very first node. We will evaluate later
2. Must be on USB, so img
3. Both: download for later dd or do it with the app, whatever is easiest
4. Simplest, cache can be later

## 4. Client certificates (note 6)

- What are these certs *for* — trusting the app's own management UI, trusting it against each node's Incus API, or both? (Operations Center uses client certs purely for its own web UI/API auth.)
- Does the app run its own CA and issue/sign certs per node, or generate self-signed certs per machine?
- Where are private keys stored, and what's the rotation/revocation story if a node is decommissioned or a USB stick is lost?

### Answers
1. Cert is for initial incus client authentication against incusos
2. Probably self signed is fine
3. We will decide this later. Focus is just one node
4. To be decided later

## 5. IPAM (note 7)

- Scope for v1: just "assign a static IP per node" bookkeeping, or real subnet/VLAN modeling with conflict detection?
- Does the app *configure* the node's network (writing `network.yaml` into its seed) or only *record* the assignment for a human to apply elsewhere (e.g., on the router/DHCP server)?
- IPv6 in scope, or IPv4-only for now?
- Any integration with an existing DHCP/DNS setup (e.g., reserve the address there too), or does this app become the sole source of truth?

### Answers
1. Just assign. Track networks to help with IP generation. Basic duplicate detection. Do consider networks with existing DHCP (track usable static IP ranges that fall outside of DHCP)
2. Configures network via writing network.yaml.
3. Just IPV4 for now
4. App becomes sole source of truth for now

## 6. TPM / Secure Boot (note 10) — flagging a likely misunderstanding

IncusOS binds **disk encryption** to measured boot via TPM PCRs, and Secure Boot is what makes those measurements trustworthy ([security model docs](https://linuxcontainers.org/incus-os/docs/main/reference/security/)). Concretely:
- With TPM present + Secure Boot on: disk encryption keys are bound to a verified boot chain.
- With TPM **missing** (`security.missing_tpm: true` in the install seed): falls back to a software TPM, which the docs call out explicitly as weakening security — encryption only protects against a stolen *powered-off* disk, not a booted/tampered system.
- With Secure Boot missing/disabled: falls back to a weaker PCR (4 instead of 7), losing trust in the boot chain.



"Disable TPM, but allow Secure Boot" is a real supported combination, but it mostly throws away the disk-encryption guarantee while keeping the (now less meaningful) Secure Boot check. Is that intentional — e.g., hardware in this homelab genuinely lacks a TPM and this is an accepted tradeoff — or was the intent closer to "don't *require* Secure Boot, but keep TPM" (i.e. `missing_secure_boot: true` instead)? Worth confirming which `install.yaml` security flags should actually be set per-instance.

### Answers
1. Allow TPM to be not present on nodes for homelab usage. Allow configurable
2. Also allow secure boot configurable

## 7. Remote logging to Grafana / Tailscale (note 8)

- IncusOS's documented seed files (`install`, `network`, `applications`, `incus`, `kernel`, `migration-manager`, `operations-center`, `provider`, `update`) don't include a logging/syslog config — there's no obvious native "ship journald to X" knob on the minimal host OS itself.
- So: does log shipping happen from the **Incus host OS** (would need e.g. `systemd-journal-remote` or a syslog forwarder, if IncusOS even exposes that), or only from **workloads running inside Incus** (a Loki/Promtail/Alloy agent deployed as one of the managed instances)? These are very different integration points.
- Tailscale-accessible endpoint vs. directly reachable Grafana — does that mean Grafana itself sits on the tailnet, or is this app generating a Tailscale Funnel/serve config for it?

### Answeres
1. Run Alloy as a incus instance. Configure remote syslog from incusOs -> Alloy incus instance -> Grafana cloud
2. Tailscale logging TBD later

## 8. Tailscale (note 11)

- Installed on every IncusOS node, only on whatever runs the web app, or both?
- As an IncusOS "application" container/instance, or some other mechanism (IncusOS being a minimal/immutable host limits options here)?
- Auth key provisioning: pre-generated reusable key baked into seeds (simpler, but a standing credential in every image) vs. some per-node enrollment flow?

### Answers
1. Use the Tailscale OS service: https://linuxcontainers.org/incus-os/docs/main/reference/services/tailscale/
2. For now, just receive an authkey. We will expand later

## 9. Where does the web app itself run? (flagged as open per your earlier answer)

- Inside the homelab it manages (a container/VM on one Incus host) — needs a bootstrap path for node #1 before the app exists to generate its installer.
- Or a separate always-on box outside the cluster — avoids the chicken-and-egg problem, adds "one more thing to maintain" outside the managed fleet.
- This also interacts with §0: if Operations Center is the bootstrapper, does *this* app only need to exist after the first node is already up?

### Answers
1. ~~Dev environment on local K8s cluster~~ — superseded by #18: no k8s dependency. Dev uses Docker Compose; deployment targets are a Docker image and a plain binary.
2. Later, we will want a migration path to IncusOS cluster (inside a k8s cluster there)

## 10. Single disk / single NIC default, configurable (note 9)

- Confirms this maps to `install.yaml`'s `target` (disk selection) and `network.yaml` (interface config) per machine — does the per-instance YAML (§2) need to support arbitrary multi-disk/multi-NIC overrides in v1, or just the single/single default for now with multi as a documented future case?

### Answers
1. Future case might ask for multiple nics
2. For now just 1
3. On topic of Network: we need to configure dhcp on/off, dns source, static IPs, etc.

## Other notes
1. Track the commit hash nodes are running
2. Some phone home functionality could be a nice-to-have if this has low development cost. I.e., could the node phone the dev instance over tailscale to indicate success (and provide a manifest of it's hardware)?

## Sources consulted

- [IncusOS — Installation seed reference](https://linuxcontainers.org/incus-os/docs/main/reference/seed/)
- [IncusOS — System security](https://linuxcontainers.org/incus-os/docs/main/reference/security/)
- [IncusOS — Operations Center application](https://linuxcontainers.org/incus-os/docs/main/reference/applications/operations-center/)
- [IncusOS — Download / flasher tool](https://linuxcontainers.org/incus-os/docs/main/getting-started/download/)
- [Operations Center source (FuturFusion)](https://github.com/FuturFusion/operations-center)
