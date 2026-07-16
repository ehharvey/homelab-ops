# App Classes — cutover shapes a renderer has to implement (0.x)

Purpose
- Name the classes of App this project expects to run, so a renderer author knows which cutover shape they're implementing before they write `Promote`.
- Record the axis that actually predicts renderer work — **what blue/green overlap costs** — rather than the more obvious but less useful "who writes".
- Record which classes could be driven declaratively and why the renderer registry stays curated anyway (see `docs/Decisions.md` § App classes).

This is a reference doc: `docs/AppManager.md` owns the mechanism (lease, reconcile algorithm, blue-green state machine), and this doc classifies the workloads that mechanism runs. Nothing here is implemented in 0.x beyond the `agent` renderer — classes 3–6 exist to be designed against, not built now (see `docs/Decisions.md` § Stateful app support and `docs/Out of Scope.md`).

**Scope:** every class here describes *one App's* cutover shape. Relationships between Apps — ordering, references, grouping several roles into one unit — are a different axis this taxonomy doesn't cover and `kind: App` can't express; see § Composition below.

## The axis: what does blue/green overlap cost

The reconcile algorithm's whole shape is "create a candidate alongside the current instance, health-check it, promote or revert". That only makes sense if the two generations can coexist. So the question that determines what a renderer's `Promote` must do is not *who writes* — it's **can blue and green run at the same time, and what does it cost when they do**.

Classifying by write topology gets you close but splits in the wrong places: it puts k8s workers and etcd members in the same bucket ("many writers") when one is trivial and the other is the hardest case here, and it calls the agent "stateless" when the agent's safety comes from a lease, not from statelessness.

## The six classes

Names are primary; the numbers are stable IDs for cross-referencing from issues and other docs.

| # | Class | Overlap | `Promote` | Example |
|---|---|---|---|---|
| 1 | Stateless | free | no-op | HAProxy, nginx, a static site |
| 2 | Lease-guarded singleton | wrong, but not corrupting | no-op — the app's own lease covers it | the `agent`, Alloy (#77), a cron/queue runner |
| 3 | Asymmetric handover | green joins read-only | switchover | Postgres + replicas |
| 4 | Symmetric scale-out | both serve | drain | k8s worker nodes |
| 5 | Quorum member | perturbs fault-tolerance math | member add/remove | etcd, k8s control plane, Galera |
| 6 | Exclusive | impossible | destructive in-place | N=1 database, Prometheus TSDB, this repo's own web app |

### 1. Stateless — overlap is free

No durable state, no scarce resource, no coordination. Two generations running at once is simply two copies serving. `Desired` builds the `InstancesPost`, `Healthy` is a probe, `Promote` returns the old generation as safe to retire and does nothing else. This is the class the reconcile algorithm was designed around, and the only one that needs no thought.

### 2. Lease-guarded singleton — overlap is wrong, but not corrupting

Nothing on disk, but running two active copies is *incorrect*: two reconcilers both acting, two cron runners both firing, two allocators both handing out the same address. Blue-green inherently creates that overlap during the health window.

**This is where the `agent` actually lives — not class 1.** Its `Promote` is a no-op *because the lease makes overlap safe*, not because overlap is free: only the lease holder acts, so a candidate running alongside the current leader does nothing until it wins an election. That generalizes — any app in this class needs its own mutual exclusion (a lease, a lock, a queue's own at-most-once delivery) and gets a no-op `Promote` for free once it has one.

The failure mode to watch: an app that *looks* stateless but has no lease will double-execute during every upgrade's health window, silently and only sometimes. "Stateless, so nothing to do" is the wrong lesson to draw from the agent renderer.

### 3. Asymmetric handover — green joins read-only

One writer, N readers. Green can be created and can catch up, but only as a reader; making it the writer is a deliberate, ordered handover. `Promote` does real work: confirm the candidate is caught up, switch over, and only then is the old writer safe to retire.

Blue-green works here **only because replication makes the data recoverable from peers rather than from the instance's own disk** — a new generation rebuilds its state from the cluster. That is the single property that turns an otherwise class-6 workload into something the existing algorithm can handle. At N=1 it collapses back to class 6.

The app manager must never own the write-leader election here; see `docs/Decisions.md` § Stateful app support.

### 4. Symmetric scale-out — both serve

Members are interchangeable and there's no quorum. Green joins the cluster, blue drains and leaves. `Promote` is a drain, or a short no-op if the app tolerates abrupt loss. Joining is cheap and a bad member is contained — it gets cordoned or drained, not obeyed.

### 5. Quorum member — overlap perturbs the fault-tolerance math

Superficially class 4 (everyone writes), but adding a member is not free: it changes quorum. Adding a 4th member to a healthy 3-member Raft cluster to blue-green it is **non-monotonic** — 4 members still tolerate only 1 failure, but now need 3 to agree rather than 2. Headroom is spent for nothing, at exactly the moment the cluster is least stable. Going the other way (3 → 2) loses fault tolerance outright.

So "create a candidate alongside the current instance" is itself a quorum event here, and `Promote` is a membership change against the cluster's own API, not a traffic cutover.

**The sustained-health gate does not mitigate this.** A version-skewed member can be perfectly healthy by its own probe and still be wrong to admit. What mitigates it is version-skew rules (below), which the reconciler has no way to know.

### 6. Exclusive — overlap is impossible

Blue-green cannot happen at all, so the only path is a destructive in-place update: stop blue, start green on the same resource, accept the downtime. This must be an explicit renderer capability, not an accident.

Two ways to land here:
- **Pinned state.** A volume one instance holds and can't share: a single-instance database, Prometheus' local TSDB, and **this repo's own web app** (pure-Go sqlite, one `STORE_PATH` volume, no replication — see `docs/Out of Scope.md` on migrating it into the fleet). Nomad documents this same conclusion for host-volume-pinned singletons, and it's why cutover semantics belong to each renderer rather than to a generic volume-aware canary mechanism (`docs/AppManager.md` § Prior art).
- **A scarce non-storage resource.** Green can't start because blue holds the only one: a static IP (`config.Instance.StaticIP` exists today for Instances), a GPU or other PCI passthrough, a fixed host port.

**Open question, flagged not answered:** `kind: App` has no endpoint field, so an App that clients must reach at a stable address has no way to express one. A proxy fronting a class-3 database wants exactly that — which would make it class 6 (a static IP, needing a floating-IP handoff) rather than the class 1 it otherwise looks like. DNS write-back is out of scope, so this is unresolved. See `docs/Out of Scope.md`.

## Fan-out: an orthogonal axis

*How many* instances an App has is independent of classes 1–6:

- **Fixed N** (`replicas: 3`, or omitted → 1).
- **Per-node** (`replicas: per-node`) — one per known `kind: Instance`, synthesized fresh each tick from the fleet's node list. The DaemonSet shape.

The agent is (class 2, per-node). Alloy (#77) is (class 2, per-node) — the same shape, which is why per-node is a property of `kind: App` rather than an agent-specific singleton kind (see `docs/Decisions.md` § App Manager HA, "Follow-up: cardinality replaces `kind: AgentConfig`"). A database is (class 3, fixed N).

**Per-node synthesis must inject per-instance identity.** The synthesized App values are otherwise identical, so a renderer that needs to know *which* node it's on has no other source: the `agent` renderer already depends on this via `AGENT_NODE_NAME`/`AGENT_INSTANCE_NAME`. This is a general requirement, not an agent quirk — #79's per-instance metrics cert needs the same hook, and a fleet-wide `App.Params` map cannot carry a per-node value.

## Placement is a third axis, and it is not cardinality

*How many* (`replicas`) and *where* (Incus cluster groups) are independent questions. `App.Node` conflated them — it meant both "one instance here" and, for the agent, "wherever the fleet's nodes are" — which is why it was deleted rather than kept. `replicas` must not inherit that conflation: it answers only *how many*.

Placement itself stays deferred (`docs/Out of Scope.md`). When it lands, the intended mechanism is Incus's own cluster groups via a **separate** field, invalid in combination with `replicas: per-node`, not an operator naming individual nodes.

Worth noting for whoever revisits that rule: Kubernetes *does* allow the equivalent combination — a DaemonSet with a `nodeSelector` means "one per node **in this group**" — so per-node-within-a-group is a coherent thing to want. If it's ever wanted, the mutual-exclusion rule is what to revisit, not `replicas`' shape.

## Composition: what this taxonomy deliberately doesn't cover

Every class above describes **one App's** cutover shape. None of them says anything about *relationships between Apps* — and `kind: App` has no way to express one.

This matters most for the topology the classes were partly written for: a proxy in front of a database is a proxy App (class 1, or 6 — see the endpoint question above) *and* a database App (class 3), not one "deployment" containing both. `replicas` doesn't help here — it's **homogeneous** fan-out (N copies of one image, one renderer, differing only by ordinal), not heterogeneous composition. Several roles means several Apps, related only by convergence.

That's the same split the prior art lands on rather than a limitation being worked around: CloudNativePG models `Cluster` and `Pooler` as separate CRDs with the Pooler naming its cluster; Patroni + HAProxy keeps the proxy separate too. What such a topology actually needs from this project is a **stable endpoint** and a **cross-App reference**, both of which are small, both deferred (`docs/Out of Scope.md`) — not a grouping construct. See that doc's composition entry for why a `kind: Deployment` isn't the answer even once those land, and for the Kubernetes revisit trigger.

## Version skew: why "image differs" is not enough

The reconcile algorithm detects a version bump generically — declared `image` differs from the live instance's `user.homelab-ops.image` tag — and hands the renderer nothing but the fact that it differs. For classes 1, 2, 4 and 6 that's sufficient. For classes 3 and 5 the *kind* of change matters, and the design currently can't express it:

- **Postgres physical replication requires matching major versions.** 16.3 → 16.4 blue-greens fine; 16 → 17 **cannot** — green can never catch up from blue, because streaming replication won't cross a major version. That's not a slower upgrade, it's a different operation (`pg_upgrade`, or logical replication).
- **Kubernetes has an N-2 skew policy** and requires the control plane to upgrade before workers — an ordering constraint across two different Apps.

So a class-3 or class-5 renderer needs to classify the change, not just observe it. Deferred; see `docs/Out of Scope.md`.

## Which classes could be generic, and why the registry stays curated

**The line is: does cutover require handing over authoritative state?**

- **Classes 1, 2, 4, 6 — no.** The only app-specific questions are "is it healthy?" and "how do you stop it gracefully?", both of which are probe-shaped and genuinely declarative.
- **Classes 3 and 5 — yes.** Who's the writer, is the candidate caught up, is a switchover safe right now, does quorum survive the member add, do these two versions even speak to each other. None of that reduces to config.

Kubernetes ran this experiment at scale and the split is the same: probes and lifecycle hooks cover Deployments, DaemonSets and StatefulSets, and then the ecosystem produced CloudNativePG, Zalando's postgres-operator and etcd-operator — real code, per app — for exactly classes 3 and 5.

So a generic system wouldn't cover 1–6; it'd cover 1, 2, 4 and 6, which are the four where a Go renderer is a handful of lines anyway. The registry stays curated. The full rationale, including the revisit trigger, is in `docs/Decisions.md` § App classes.

## This project's apps, mapped

| App | Class | Cardinality | Status |
|---|---|---|---|
| `agent` | 2 | per-node | the one renderer 0.x ships (#98) |
| Alloy | 2 | per-node | Phase 4 (#77) |
| web app | 6 | 1 | migrating it into the fleet is out of scope |
| DB proxy | 1 or 6 | fixed N | a *separate* App from the database, not a role within it — unresolved pending the endpoint and cross-App-reference questions above |
| database | 3 | fixed N ≥ 3 | direction only (`Decisions.md` § Stateful app support) |
| k8s workers | 4 | fixed N | direction only |
| k8s control plane | 5 | fixed N ≥ 3 | direction only |

Everything *currently planned* is in the generic-able classes — which is precisely why the generic engine isn't needed, and why the two classes that would justify it (3 and 5) are also the two that couldn't use it.

## Notes / alternatives

- **Rejected: a declarative `class:` field on `kind: App`.** Letting an operator declare the class and getting the cutover choreography for free is the obvious generalization, and it's unsafe: the field lets an operator *lie*. Declaring class 4 for a class 3 app means two writers and data loss, with the reconciler cheerfully cooperating, and nothing in the schema can detect the mismatch. A renderer author can't make that mistake by accident — the class is implicit in the code they wrote. The taxonomy earns its keep as shared Go (an embeddable no-op `Promote` for classes 1/2, a destructive-update helper for class 6), not as a config enum.
- **Not foreclosed: the declarative path is a strict subset.** If bringing your own stateless app without writing Go is ever wanted, that arrives as *one more curated renderer* (`type: generic-stateless`, reading its probe config from `params`) — the 7th registry entry, not a second execution model. See `docs/Decisions.md` § App classes.

## Linkage

Mechanism (lease, reconcile algorithm, blue-green state machine) is `docs/AppManager.md`; rationale and trade-offs are `docs/Decisions.md` § App classes (curated registry) and § Stateful app support (databases/k8s direction), with § App Manager HA covering the fleet/lease design these classes run on. Renderer implementations and the registry live in `internal/apprenderer`; schema and validation in `internal/config`. `docs/Out of Scope.md` holds what's deferred: volumes, version-skew classification, placement, run-to-completion apps, and App endpoints.
