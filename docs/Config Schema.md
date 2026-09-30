# Config Schema — the fleet config repo (0.x)

The fleet is declared in a git repo of k8s-style YAML documents. This page is
the reference for that repo: which files are read, every field each `kind`
accepts, and what checks it. The code is the source of truth
(`internal/config` parses and validates, `internal/configsync` reads the
repo). Where this page and the code disagree, the code is right, and this
page needs fixing.

Working examples live in [`examples/`](https://github.com/ehharvey/homelab-ops/tree/main/examples). A test parses and
validates every one of them the way a sync does, so they can't drift from
this schema.

Later sections:

- **What each reader does with the repo:** the web app, `bootstrap
  render-seed`, and the agent (planned).
- **Known gaps:** things the checks don't catch today.
- **Proposed additions:** a sketch of the schema changes in flight (#101,
  #181, #199). None of those is implemented, and the strict parser rejects
  them today.

## The repo

- **Every `*.yaml` and `*.yml` file at the repo root is read**, in file-name
  order, and merged into one fleet.
  - Subdirectories are ignored, so they're a safe place for anything else
    (docs, scripts, drafts).
  - How documents are split across files carries no meaning: one file, or
    one per kind, or one per node, parse the same.
- **Each file holds one or more documents**, separated by `---`.
- **Validation errors cite positions in the merged list**, e.g.
  `instances[2].static_ip`: counted in file-name order, then document order.
- **A sync is all or nothing.** A document that fails to parse, or a fleet
  that fails validation, rejects the whole commit. The previous synced state
  stays in place.

The web app syncs from:

| Setting | Meaning |
|---|---|
| `CONFIG_REPO_URL` | The repo: any go-git transport (`https://`, `git://`, a local path). Unset disables sync. |
| `CONFIG_REPO_REF` | The **branch** to read. Default `main`. A tag or commit SHA is not accepted. |
| `CONFIG_SYNC_INTERVAL` | Background poll interval (e.g. `5m`). Unset means sync only on `POST /sync`. |

Each sync is a fresh shallow clone of that one branch. No git credentials are
supported yet, so the repo must be readable anonymously (see Known gaps). Keep
in mind that a public repo publishes everything in it. §28 of
`docs/Decisions.md` covers what that means for addresses (#199).

## Documents

Every document must set `kind:`. Anything else is a parse error: a missing
`kind`, an unrecognised one, or an unknown field (a typo'd `statc_ip` fails
loudly rather than being dropped). Three kinds exist: `Network`, `Instance`
and `App`.

Addresses (`cidr`, `gateway`, `static_ip`, `dns`, `dhcp_excluded_range`) are
checked for syntax at parse time. The semantic checks below run afterwards, as
one pass that reports every issue at once.

### `kind: Network`

A LAN the nodes sit on. IPAM draws static addresses from it (`docs/Ipam.md`).

```yaml
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dhcp_excluded_range: 192.168.1.200-192.168.1.250
dns: [192.168.1.1]
```

| Field | Type | Required | Rules and meaning |
|---|---|---|---|
| `name` | string | yes | Non-empty and unique among Networks. What `Instance.network` refers to. |
| `cidr` | prefix | yes | The LAN's subnet. IPv4 in practice: IPAM and the network/broadcast checks are IPv4-only. |
| `gateway` | address | see rule | Must be inside `cidr`. Needed for seed rendering whenever an instance on this network has a `static_ip`, since it becomes the node's default route. `Validate` doesn't catch its absence; rendering does. |
| `dhcp_excluded_range` | `"<start>-<end>"` | no | The static range, meaning the addresses the LAN's own DHCP server must not hand out. Both ends must be inside `cidr`, with start ≤ end. When set, every `static_ip` on this network must fall inside it, and IPAM assigns omitted ones from it. |
| `dns` | list of addresses | no | Nameservers rendered into each node's `network.yaml`. |

### `kind: Instance`

One physical IncusOS machine.

```yaml
kind: Instance
name: node0
mac: 02:00:00:00:00:01
network: home-lan
static_ip: 192.168.1.201
disk: single
nic: single
security:
  tpm: true
  secure_boot: true
applications: [incus]
```

| Field | Type | Required | Rules and meaning |
|---|---|---|---|
| `name` | string | yes, in effect | The node's identity everywhere downstream: its Incus cluster member name, its cert names, its web-app store key, and the agent's `AGENT_NODE_NAME`. Not validated yet (see Known gaps). |
| `mac` | string | yes, in effect | The NIC IncusOS configures as `eth0`. Not validated; a wrong MAC leaves the node's network unconfigured at boot. Quoting is optional. |
| `network` | string | yes | A Network's `name`. The instance must sit on that network. |
| `static_ip` | address | no | Must be inside the network's `cidr` and its `dhcp_excluded_range` (if set). It can't be the gateway, network or broadcast address. **Omitted means something different per reader**: the web app assigns one, while `render-seed` renders DHCP (see What each reader does). The address matters beyond networking, because it's what makes a node an Incus cluster member (§26). A DHCP node comes up unclustered. |
| `disk` | string | yes | Only `single` is implemented (checked at render). |
| `nic` | string | yes | Only `single` is implemented (checked at render). |
| `security.tpm` | bool | no, default `false` | Whether the machine has a TPM. `false` tells the IncusOS installer to proceed without one. |
| `security.secure_boot` | bool | no, default `false` | The same, for Secure Boot. |
| `applications` | list | yes | IncusOS host applications. Only `[incus]` is supported (checked at render). This is unrelated to `kind: App`. |

`tunnel_ip` is **not** a config field. The web app assigns each node's
WireGuard overlay address itself, and a `tunnel_ip:` in git is rejected as an
unknown field.

### `kind: App`

A workload the app-manager agent reconciles against live Incus.
`docs/AppManager.md` explains the design, and `docs/AppClasses.md` covers
which kinds of App a renderer can be.

```yaml
kind: App
name: agent
type: agent
replicas: per-node
image:
  server: https://ghcr.io
  protocol: oci
  alias: ehharvey/homelab-ops/agent:latest
params: {}
```

| Field | Type | Required | Rules and meaning |
|---|---|---|---|
| `name` | string | yes | Non-empty and unique among Apps. A `per-node` App's instances are named `<name>-<instance name>`. |
| `type` | string | yes | The renderer-registry key, e.g. `agent`. Not checked against a registry at sync time: config doesn't know which renderers a binary registered. |
| `replicas` | count or `per-node` | yes, no default | How many, never where. A positive count, or `per-node` for one per declared Instance. At most one `per-node` App per `type`. |
| `image.server` | URL | no | Image server, e.g. `https://ghcr.io`. |
| `image.protocol` | string | no | `oci`, `simplestreams`, `incus`, or empty. |
| `image.alias` | string | one of the two | Image alias, e.g. `owner/repo:tag`. |
| `image.fingerprint` | string | one of the two | Image fingerprint. `alias` or `fingerprint` must be set. |
| `params` | map of string to string | no | Opaque, renderer-specific settings. |

There's deliberately no placement, strategy or version field; see
`docs/AppManager.md` § `kind: App` schema. App reconciliation is paused under
§27 (#98) apart from the agent itself (#203).

## What each reader does with the repo

| Reader | Reads | Differences |
|---|---|---|
| **Web app** | The branch above, on `POST /sync` or every `CONFIG_SYNC_INTERVAL` | Runs `Validate`, then IPAM. An omitted `static_ip` gets an address from the network's `dhcp_excluded_range`, stable across syncs. It errors if the range is unset or exhausted. The web app stores the result, reports what changed against the previous sync, and renders each node's seed and image from it. |
| **`bootstrap render-seed`** | One local file (`--file`), no git | Needs **exactly one** Network and one Instance; Apps are ignored. There's no IPAM, so an omitted `static_ip` means DHCP, and the node comes up unclustered. Use [`examples/single-node/`](https://github.com/ehharvey/homelab-ops/tree/main/examples/single-node). |
| **Agent** (planned, #101) | The same repo, cloned by the agent itself (§27) | Runs with the web app down. It reads Apps and the designation (Proposed, below), never addressing. |

## Known gaps

Things the checks don't catch today, recorded so nobody assumes they do:

- **`Instance.name` is not validated.** An empty name and a duplicate name both
  pass `Validate`. The web app then fails on the duplicate with a store error
  rather than a validation issue. Since the name becomes the Incus member name
  and, per #193, the node's hostname, it should be required, unique and a
  valid hostname label.
- **A DHCP instance's `network` isn't checked by `Validate`.** With no
  `static_ip`, `Validate` skips the instance. In the web app, IPAM rejects the
  unknown name instead, as an IPAM error rather than a validation issue. In
  `render-seed`, it surfaces at rendering.
- **A missing `gateway` passes `Validate`.** It's caught at render instead,
  which is later than an operator would expect.
- **`mac` is free text.**
- **No private repos.** Sync passes no credentials. §28's git-addressed
  networks assume a private repo, so they need this first. So does the agent
  syncing one (§28 mentions a read-only deploy key).

## Proposed additions

A spike for #207: the schema changes other issues will make, sketched against
the schema above so they can be judged together. **Nothing here is
implemented.** The strict parser rejects every field and kind in this section.
Each owning issue makes the final call; where there's a recommendation, it's
only that.

### The leader designation (#101)

The agents need to know which node's agent leads: `leaderelection.Designation`
in §25.

```yaml
kind: Designation
primary: node0   # an Instance name: a node, not an agent instance
```

That's the whole document. A stale checkout is fenced by git itself: each
agent publishes the commit it's on, and an agent behind a peer stands down.
A new primary also waits for the old one to publish that it has stopped
acting. So a failover is one commit changing `primary`. This replaced an
earlier `epoch` field; see §25's #212 addendum.

**Recommendation: a new singleton `kind: Designation`.**

- **Leadership is fleet-wide.** It isn't a property of the agent App's image
  or cardinality.
- **A separate document shows up on its own.** The web app's change report
  lists a failover as a change to leadership, not as an edit to a node or
  an App.
- **It can be validated.** On the agent App it would be either a generic App
  field that means something to one renderer only, or a string in `params`,
  which nothing checks.

`primary: true` on an `Instance`, with at most one allowed, was considered.
It needs no new kind, but it reads as an edit to a node rather than a
change of leadership.

Rules:

- At most one per repo. Zero means no agent acts. That's a safe default, and
  it matches `MayAct` treating an unknown designation as "not leader".
- `primary` must name a declared Instance, so #210's name validation comes
  first.

### Cluster membership (#181)

The seed renderer and the agent need to know whether a node bootstraps the
cluster or joins it (§27). The #193 spike showed the role is fixed when the
node is flashed: a node seeded as a bootstrap member can never join. That
makes it a property of the node.

```yaml
kind: Instance
name: node1
# ...
cluster_role: join   # bootstrap | join
```

**Recommendation: a field on `Instance`**, rather than a `kind: Cluster`
listing members, with exactly one `bootstrap` per fleet.

- **No join order is needed.** The agent joins missing members one at a time,
  each against the running cluster, so document order never becomes a
  contract. That was #181's worry about deriving the role from order.
- **Open: the default when omitted.** Treating an omitted role as `bootstrap`
  keeps today's single-node repos valid. But a second node that forgets the
  field would then start a second cluster. Requiring the field, as `replicas`
  is, is safer. It costs a one-line edit to existing repos, including
  `dev/git-fixture`.
- **The first designated primary must be the bootstrap member,** because
  that's where the first agent is deployed (#100). Once other members have
  joined, a failover can name any member.

### Network addressing from web app state (#199)

With a public repo, a Network may carry only its name. Its addressing then
lives in web app state (§28):

```yaml
kind: Network
name: home-lan   # addressed from web app state: no cidr, gateway, range or dns here
```

The rules, from §28:

- **All or nothing.** A Network carries all its addressing fields or none; a
  partial set is an error.
- **One source per network.** If git and state both address the same network,
  the sync is rejected.
- **`static_ip` follows its network's mode.** In git it's allowed only on a
  git-addressed network.

`render-seed` keeps reading full Network documents from its local file. An
example repo in this mode, plus a proposed format for the web app's addressing
file, is drafted in #208 and waits on #199.

### Not a schema change: the node's hostname (#193)

The spike showed a node that hasn't joined reports its machine UUID as its
server name unless `network.yaml` sets `dns.hostname`. The fix is for the seed
renderer to set `dns.hostname` from `Instance.name`, with no new field. That
also makes the name validation under Known gaps a prerequisite.

### Placement (#182, paused)

A placement field on `App` (Incus cluster groups) is anticipated, as a
separate field from `replicas` and invalid alongside `per-node`
(`docs/AppManager.md`). It's paused with App reconciliation (§27), so no shape
is proposed here.
