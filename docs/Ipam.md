# IPAM — IP Allocation & Conflict-resolution (0.x)

> **Direction (2026-11-02, `docs/Decisions.md` §31, §5):** IPAM also drives DHCP reservations (Kea) and DNS records, with a DHCP mode per `Network` (`off`, `proxy`, `reservations`, `full`). The `dhcp.mode` and `dhcp.pool` fields, the per-mode ranges below and the reservations output are built (#490). Serving DHCP and DNS from the appliance is Phase 5, as are rendering `full` nodes as DHCP clients (the seed writes `static_ip` in every mode until then) and carving the appliance's own address out of the ranges.

The two ranges, per mode (#490)
- A Network has two ranges: the static range (`dhcp_excluded_range`) and the DHCP pool (`dhcp.pool`). Either, both or neither may be written. When only one is, the other is its complement within the CIDR, which can be two spans. Written ones must lie in the CIDR and must not overlap each other.
- Each range, written or derived, is **uncarved**. Its **effective** range is the uncarved one minus the addresses never allocated: the network, broadcast and gateway addresses, and each of the Network's `dns` addresses inside the CIDR. Carving can split a range, so an effective range is a list of ranges.
- Explicit `static_ip`s are checked against the **uncarved** node range, so a node that is itself the DNS server can pin its own address. A gateway, network or broadcast `static_ip` is still rejected (`config.Validate`). Auto-assignment draws from the **effective** node range, and so will Kea's dynamic pool.
- Which range is the node range depends on the mode:

| `dhcp.mode` | IPAM hands out | `dhcp_excluded_range` | `dhcp.pool` | Node IPs come from |
|---|---|---|---|---|
| `off` (default) | nothing | the static range, which the router never leases | the router's dynamic pool | the static range |
| `proxy` | boot options only | as `off` | as `off` | the static range |
| `reservations` | the reservations, with no dynamic pool | as `off` | as `off` | the static range |
| `full` | the reservations plus dynamic leases, as the one authority | addresses IPAM never uses, for downstream instances to take statically | IPAM's dynamic pool, which holds the nodes' reservations too | the pool |

- With neither range written, `off`, `proxy` and `reservations` have no auto-assign pool (an explicit `static_ip` need only be in the CIDR), and `full`'s uncarved pool is the whole CIDR.
- The derivation and carving live in one place, `config.Network.Allocation`, which `config.Validate`, `ipam.NewNetworkPool` and `bootstrap parse-config` all read (and Phase 5's Kea renderer will).

Reservations (#490)
- `ipam.Reservations(network, instances)` turns a network's Instances, after `Assign` (the store's addresses, §12), into reservations: the normalised `mac`, the assigned IP, and the hostname, the bare `Instance.name`.
- It skips an Instance without a `mac` (allowed only on `off` and `proxy` networks) and errors on one with a `mac` but no address (`ErrUnassigned`). It doesn't read the mode: whether a network's reservations are served is the consumer's call. It has no route and serves no DHCP yet.
- On a `reservations` or `full` network every Instance needs a `mac`, and no two may share one (compared normalised); a `mac` is syntax-checked at parse in every mode.

Purpose
- Track Network definitions (CIDR, the static range and the DHCP pool), as resolved by the sync: each network's addressing comes from git (a private repo) or from the web app's addressing file, one source per network (`docs/Decisions.md` §28, #199). IPAM sees only the resolved Network and doesn't know which.
- Assign stable IPv4 addresses to Instances when `static_ip` is omitted.
- Validate explicit `static_ip` values.
- Persist assignments in the durable store so IPs survive restarts.

High-level policy (0.x)
- Explicit static_ip in the incoming desired-state is accepted if valid and not already held by a different instance.
  - Valid means: parseable IP, belongs to the Network's CIDR, and (if the network has a node range) belongs to its uncarved node range: the DHCP excluded range in `off`, `proxy` and `reservations`, the pool in `full`.
  - Not already held means: no *other* instance's prior-assigned IP (per the durable store) is the same address on the same Network. The same instance reasserting its own prior IP explicitly is fine.
- A prior-assigned IP (from the durable store) is reserved for its instance. If an explicit static_ip in the incoming config collides with another instance's prior-assigned IP, the sync is rejected (ErrDuplicate) — the operator must free the address first (e.g. change or remove the holding instance) rather than have it silently relocated.
- When static_ip is omitted, the instance's own prior-assigned IP is reused if it's still valid for the Network, isn't a carved address (gateway, network, broadcast, in-CIDR dns), and isn't reserved by someone else's explicit value; otherwise a fresh address is auto-assigned from the effective node range.
- Duplicate explicit static_ip values among current desired-state Instances for the same Network are rejected.
- Duplicate explicit static_ip values across different Networks are allowed if the CIDRs do not overlap (same numeric IP may exist on different subnets).
- Pool exhaustion (no free IPs in the effective node range: the DHCP excluded range, or the pool in `full`) results in a clear error (ErrPoolExhausted).

Network & field validation
- Network fields to validate (post-Parse, and in the web app post-resolve: the address checks below run on the joined Network, whichever source addressed it):
  - name: non-empty and unique within the Config.
  - cidr: parseable as IPv4 CIDR. In git it may be omitted together with the other addressing fields, for a state-addressed network; the resolve step fills them in from the addressing file before these checks, and rejects a partial set, a network addressed in both sources, and a `static_ip` in git on a state-addressed network (`docs/Config Schema.md` § Network addressing).
  - gateway: parseable IP and contained in the CIDR.
  - dhcp_excluded_range, dhcp.pool: optional; when present each must be "start-end" with parseable IPv4s, both inside the CIDR, start <= end, and the two must not overlap.
  - dhcp.mode: optional, one of `off` (the default), `proxy`, `reservations`, `full`; an unknown one fails at parse.
  - dns: syntactic sanity checks for entries.
- Instances:
  - network: refers to an existing Network by name.
  - static_ip: if supplied, parseable and in-network and in the uncarved node range (if there is one): the DHCP excluded range, or the pool in `full`.
  - mac: a 48-bit Ethernet MAC (`config.ParseMAC`: `net.ParseMAC`, 6 octets) in every mode; required and unique per network in `reservations` and `full`.

Assignment algorithm (conceptual)
1. Validate incoming Config (Networks + Instances).
2. Validate explicit static_ip values for syntax and membership (CIDR / uncarved node range).
3. Reserve explicit static_ip values among the current batch (reject duplicates within the batch).
4. For each explicit static_ip, check it against prior-assigned IPs (per Network, from the durable store): if it equals a *different* instance's prior IP, reject the sync (ErrDuplicate). If it equals the *same* instance's own prior IP, accept it as usual.
5. Auto-assign remaining instances (no explicit static_ip), in two passes. First, every one of them takes back its own prior-assigned IP if it's still valid for the Network and not reserved by an explicit value from step 3/4. Then each one still without an address draws the next free address from the Network's effective node range (the excluded range, or the pool in `full`) in ascending order, skipping already-taken addresses. Reusing every prior before any draw is what keeps a new instance listed ahead of an existing node from drawing the node's address and renumbering it.
6. Persist the resulting assignments in the durable store.

Conflict-resolution summary
- Incoming explicit static_ip vs prior-held IP (different instance): rejected — the sync fails with ErrDuplicate rather than silently relocating the prior holder's address.
- Incoming explicit static_ip reasserting the same instance's own prior-held IP: accepted, no-op.
- Incoming explicit static_ip vs incoming explicit static_ip (same IP): reject as a duplicate (error).
- Prior-held IP vs prior-held IP across separate instances in store should not exist; store consistency should prevent it. On detection, treat as conflict and surface error for operator intervention.

Determinism & stability
- Auto-assignment must be deterministic given:
  - Network's effective node range,
  - taken set (explicit+accepted priors),
  - iteration order over instances (prefer stable ordering, e.g., sorted by instance name).
- Prior-assigned IP for an instance is reused if valid and not reserved by an explicit static_ip belonging to someone else; if another instance's explicit static_ip collides with it instead, the sync is rejected (see Conflict-resolution summary) rather than silently redrawing the prior holder.

Error signals (examples)
- ErrInvalidCIDR / validation errors: malformed CIDR / gateway / excluded-range.
- ErrOutOfRange / ErrInvalidStaticIP: explicit static_ip not in network or uncarved node range.
- ErrDuplicate: duplicate static_ip among current desired-state Instances for the same Network.
- ErrPoolExhausted: no available addresses to auto-assign.
- ErrUnassigned: Reservations given an Instance with a mac but no address.

Examples
- Network lan: 192.168.1.0/24, excluded 192.168.1.200-192.168.1.203
  - Prior store: node-a → .200
  - Incoming config: node-a (static_ip omitted), node-b static_ip: .200
  - Result (0.x): rejected with ErrDuplicate — .200 is reserved for node-a; node-b must pick a different address or the operator must free .200 first.
  - Contrast: if node-a's own entry instead explicitly reasserts static_ip: .200, that's accepted (no-op) — an instance reclaiming its own address is never a conflict.

Testing guidance
- Unit tests should cover:
  - explicit static_ip acceptance and rejection (out-of-range, not in the node range, per mode),
  - the derived and carved ranges per mode, and auto-assignment skipping an in-CIDR dns address,
  - duplicate explicit static_ip detection (within the current batch, same Network),
  - pool exhaustion,
  - stability: prior-held IP reused when no explicit static_ip,
  - an instance explicitly reasserting its own prior-held IP: accepted, no-op,
  - conflict with prior-held IP owned by a *different* instance: rejected (ErrDuplicate) — the sync fails rather than silently redrawing the prior holder,
  - determinism of assignment order.
- Example test names:
  - TestExplicitStaticIPAcceptedWhenValid
  - TestDuplicateExplicitStaticIPsRejected
  - TestPoolExhaustion
  - TestAssignmentStableAcrossResyncs
  - TestExplicitStaticIPReassertingOwnPriorIPAccepted
  - TestExplicitStaticIPRejectedWhenConflictsWithPriorAssignedIP

Notes / alternatives
- An earlier draft of this doc proposed "explicit wins, prior is silently redrawn" — i.e. a new instance's explicit static_ip could bump a different instance off its existing address without any error. Rejected: silently relocating one node's static IP because another node's config happened to claim the same address is exactly the kind of surprise that should be a hard, visible failure for hardware provisioning (see CLAUDE.md's note on issue #5 — passing tests don't guarantee no surprises on real hardware). 0.x instead reserves prior-assigned IPs against explicit conflicts (this doc's current policy).

Linkage
- This doc complements the high-level notes in docs/Architecture.md. Keep implementation-level validation in internal/config/validate.go and assignment logic in internal/ipam/*.