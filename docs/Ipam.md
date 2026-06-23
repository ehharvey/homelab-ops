# IPAM — IP Allocation & Conflict-resolution (v1)

Purpose
- Track Network definitions (CIDR + DHCP excluded range).
- Assign stable IPv4 addresses to Instances when `static_ip` is omitted.
- Validate explicit `static_ip` values.
- Persist assignments in the durable store so IPs survive restarts.

High-level policy (v1)
- Explicit static_ip in the incoming desired-state is accepted if valid and not already held by a different instance.
  - Valid means: parseable IP, belongs to the Network's CIDR, and (if a DHCP excluded range is configured) belongs to that excluded range.
  - Not already held means: no *other* instance's prior-assigned IP (per the durable store) is the same address on the same Network. The same instance reasserting its own prior IP explicitly is fine.
- A prior-assigned IP (from the durable store) is reserved for its instance. If an explicit static_ip in the incoming config collides with another instance's prior-assigned IP, the sync is rejected (ErrDuplicate) — the operator must free the address first (e.g. change or remove the holding instance) rather than have it silently relocated.
- When static_ip is omitted, the instance's own prior-assigned IP is reused if it's still valid for the Network and not reserved by someone else's explicit value; otherwise a fresh address is auto-assigned.
- Duplicate explicit static_ip values among current desired-state Instances for the same Network are rejected.
- Duplicate explicit static_ip values across different Networks are allowed if the CIDRs do not overlap (same numeric IP may exist on different subnets).
- Pool exhaustion (no free IPs in the DHCP excluded range) results in a clear error (ErrPoolExhausted).

Network & field validation
- Network fields to validate (post-Parse):
  - name: non-empty and unique within the Config.
  - cidr: parseable as IPv4 CIDR.
  - gateway: parseable IP and contained in the CIDR.
  - dhcp_excluded_range: optional; when present must be "start-end" with parseable IPv4s, both inside the CIDR, start <= end.
  - dns: syntactic sanity checks for entries.
- Instances:
  - network: refers to an existing Network by name.
  - static_ip: if supplied, parseable and in-network and in DHCP excluded range (if set).

Assignment algorithm (conceptual)
1. Validate incoming Config (Networks + Instances).
2. Validate explicit static_ip values for syntax and membership (CIDR / excluded range).
3. Reserve explicit static_ip values among the current batch (reject duplicates within the batch).
4. For each explicit static_ip, check it against prior-assigned IPs (per Network, from the durable store): if it equals a *different* instance's prior IP, reject the sync (ErrDuplicate). If it equals the *same* instance's own prior IP, accept it as usual.
5. Auto-assign remaining instances (no explicit static_ip): reuse the instance's own prior-assigned IP if it's still valid for the Network and not reserved by an explicit value from step 3/4; otherwise draw the next free address from the Network's excluded range in ascending order, skipping already-taken addresses.
6. Persist the resulting assignments in the durable store.

Conflict-resolution summary
- Incoming explicit static_ip vs prior-held IP (different instance): rejected — the sync fails with ErrDuplicate rather than silently relocating the prior holder's address.
- Incoming explicit static_ip reasserting the same instance's own prior-held IP: accepted, no-op.
- Incoming explicit static_ip vs incoming explicit static_ip (same IP): reject as a duplicate (error).
- Prior-held IP vs prior-held IP across separate instances in store should not exist; store consistency should prevent it. On detection, treat as conflict and surface error for operator intervention.

Determinism & stability
- Auto-assignment must be deterministic given:
  - Network's excluded range,
  - taken set (explicit+accepted priors),
  - iteration order over instances (prefer stable ordering, e.g., sorted by instance name).
- Prior-assigned IP for an instance is reused if valid and not reserved by an explicit static_ip belonging to someone else; if another instance's explicit static_ip collides with it instead, the sync is rejected (see Conflict-resolution summary) rather than silently redrawing the prior holder.

Error signals (examples)
- ErrInvalidCIDR / validation errors: malformed CIDR / gateway / excluded-range.
- ErrOutOfRange / ErrInvalidStaticIP: explicit static_ip not in network or excluded range.
- ErrDuplicate: duplicate static_ip among current desired-state Instances for the same Network.
- ErrPoolExhausted: no available addresses to auto-assign.

Examples
- Network lan: 192.168.1.0/24, excluded 192.168.1.200-192.168.1.203
  - Prior store: node-a → .200
  - Incoming config: node-a (static_ip omitted), node-b static_ip: .200
  - Result (v1): rejected with ErrDuplicate — .200 is reserved for node-a; node-b must pick a different address or the operator must free .200 first.
  - Contrast: if node-a's own entry instead explicitly reasserts static_ip: .200, that's accepted (no-op) — an instance reclaiming its own address is never a conflict.

Testing guidance
- Unit tests should cover:
  - explicit static_ip acceptance and rejection (out-of-range, not in excluded range),
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
- An earlier draft of this doc proposed "explicit wins, prior is silently redrawn" — i.e. a new instance's explicit static_ip could bump a different instance off its existing address without any error. Rejected: silently relocating one node's static IP because another node's config happened to claim the same address is exactly the kind of surprise that should be a hard, visible failure for hardware provisioning (see CLAUDE.md's note on issue #5 — passing tests don't guarantee no surprises on real hardware). v1 instead reserves prior-assigned IPs against explicit conflicts (this doc's current policy).

Linkage
- This doc complements the high-level notes in docs/Architecture.md. Keep implementation-level validation in internal/config/validate.go and assignment logic in internal/ipam/*.