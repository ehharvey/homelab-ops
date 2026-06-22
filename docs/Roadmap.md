# Roadmap — Homelab Ops App (v1)

Phased build order derived from `Architecture.md`. Each phase should produce something runnable/demonstrable before moving on.

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
