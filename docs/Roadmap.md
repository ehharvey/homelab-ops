# Roadmap — Homelab Ops App (v1)

Phased build order derived from `Architecture.md`. Each phase should produce something runnable/demonstrable before moving on.

## Phase 0 — Node #0 bootstrap tool

Goal: get one IncusOS machine up and trusted, with nothing else running yet.

- [ ] Offline self-signed cert/key generation
- [ ] Minimal `Instance`/`Network` YAML parsing (just enough for one node)
- [ ] Seed renderer: `install.yaml` (disk target + TPM/Secure Boot flags) + `network.yaml` (static IP or DHCP) + `applications.yaml` (`incus` only)
- [ ] Shell out to `flasher-tool` to produce a `.img`
- [ ] Manually flash + install; confirm Incus is reachable and trusts the generated cert

**Done when:** node #0 is running IncusOS + Incus, reachable, and the bootstrap cert authenticates against it.

## Phase 1 — Web app skeleton + config sync

- [ ] Go service scaffold, deployed to the local dev k8s cluster
- [ ] GitHub config sync: pull one public repo, parse k8s-style multi-doc YAML (`kind: Network`, `kind: Instance`)
- [ ] In-memory/local store for parsed objects
- [ ] Diff against last-synced state; surface warnings (no auto-apply)

**Done when:** pushing a YAML change to the repo produces a visible diff/warning in the app, with no side effects on real nodes yet.

## Phase 2 — IPAM + app-driven installer generation

- [ ] IPAM: register `Network` CIDRs + DHCP-excluded ranges, assign static IPv4s to instances, duplicate-detect
- [ ] Reuse Phase 0's seed-rendering logic inside the app, parameterized by any `Instance`
- [ ] Per-instance cert issuance (self-signed), basic local storage
- [ ] Wire IPAM-assigned IP into the rendered `network.yaml`
- [ ] Serve generated `.img` for download, and/or flash directly — pick whichever's faster to ship

**Done when:** the app can take a new `Instance` entry from the synced repo and produce a working installer end-to-end, without the bootstrap CLI.

## Phase 3 — Tailscale + logging

- [ ] Accept an operator-supplied Tailscale authkey per instance; bake into seed via IncusOS's Tailscale service
- [ ] Stand up an Alloy Incus instance; point node syslog at it
- [ ] Alloy → Grafana Cloud forwarding confirmed end-to-end

**Done when:** a freshly provisioned node is reachable over Tailscale and its logs show up in Grafana Cloud.

## Deferred — not v1

Tracked here so they don't get lost, not because they're scheduled:

- Multi-node clusters; revisit wrapping Operations Center once that's real
- GitOps auto-apply + rollback (today: diff-and-warn only)
- Private repos, repo-sharing across environments
- IPv6, DHCP/DNS write-back
- Cert rotation/revocation, moving off self-signed to a real CA
- Multi-disk / multi-NIC instance overrides
- Commit-hash-per-node tracking
- Phone-home / hardware-manifest reporting over Tailscale
- Migrating the web app's own runtime off the dev k8s cluster and into the IncusOS fleet
