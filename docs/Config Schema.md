# Config Schema — the fleet config repo (0.x)

The fleet is declared in a git repo of k8s-style YAML documents. This page is
the reference for that repo: which files are read, every field each `kind`
accepts, and what checks it. The code is the source of truth
(`internal/config` parses and validates, `internal/configsync` reads the
repo). Where this page and the code disagree, the code is right, and this
page needs fixing.

Working examples live in [`examples/nodes/`](https://github.com/ehharvey/homelab-ops/tree/main/examples/nodes). A test parses and
validates every one of them the way a sync does, so they can't drift from
this schema.

Later sections:

- **What each reader does with the repo:** the web app, `bootstrap
  render-seed`, and the agent.
- **Known gaps:** things the checks don't catch today.
- **Proposed additions:** a sketch of the schema changes in flight (#181).
  None of those is implemented, and the strict parser rejects them today.

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

The web app syncs from these deployment settings (`docs/Deployment.md` lists
every setting, and the `web.env` file they go in on an appliance):

| Setting | Meaning |
|---|---|
| `CONFIG_REPO_URL` | The repo: any go-git transport (`https://`, `git://`, a local path). Unset disables sync. |
| `CONFIG_REPO_REF` | The **branch** to read. Default `main`. A tag or commit SHA is not accepted. |
| `CONFIG_SYNC_INTERVAL` | Background poll interval (e.g. `5m`). Unset means sync only on `POST /sync`. |
| `CONFIG_REPO_PRIVATE` | `true` allows network addressing in git. Default `false`: a sync whose repo addresses any Network is rejected, naming the network. Any value other than `true` or `false` stops the web app at startup. See Network addressing below. |
| `NETWORK_ADDRESSING_PATH` | The addressing file: addressing for the networks git names without any. Unset means none, so every Network must then be addressed in git. Read on every sync. |

Each sync is a fresh shallow clone of that one branch. No git credentials are
supported yet, so the repo must be readable anonymously (see Known gaps). Keep
in mind that a public repo publishes everything in it, which is why network
addressing can stay out of git (Network addressing, below, and §28 of
`docs/Decisions.md`).

## Documents

Every document must set `kind:`. Anything else is a parse error: a missing
`kind`, an unrecognised one, or an unknown field (a typo'd `statc_ip` fails
loudly rather than being dropped). Four kinds exist: `Network`, `Instance`,
`App` and `Designation`.

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
| `cidr` | prefix | yes, unless the network is state-addressed | The LAN's subnet. IPv4 in practice: IPAM and the network/broadcast checks are IPv4-only. In a repo the web app syncs, a Network may leave out all four addressing fields (`cidr`, `gateway`, `dhcp_excluded_range`, `dns`) and take them from the addressing file instead; see Network addressing, below. `render-seed` always needs them here. |
| `gateway` | address | see rule | Must be inside `cidr`. Needed for seed rendering whenever an instance on this network has a `static_ip`, since it becomes the node's default route. `Validate` doesn't catch its absence; rendering does. |
| `dhcp_excluded_range` | `"<start>-<end>"` | no | The static range, meaning the addresses the LAN's own DHCP server must not hand out. Both ends must be inside `cidr`, with start ≤ end. When set, every `static_ip` on this network must fall inside it, and IPAM assigns omitted ones from it. |
| `dns` | list of addresses | no | Nameservers rendered into each node's `network.yaml`. |

#### Network addressing: from git or from the web app's state (#199)

A Network's addressing, its `cidr`, `gateway`, `dhcp_excluded_range` and
`dns`, comes from exactly one place (`docs/Decisions.md` §28). Git always
holds the network's name.

- **Git-addressed**: the Network in git sets `cidr` (its other three fields
  stay optional, as in the table). This needs a private repo, so the web app
  rejects it unless `CONFIG_REPO_PRIVATE=true`.
- **State-addressed**: the Network in git carries only its name, and its
  addressing comes from the web app's addressing file
  (`NETWORK_ADDRESSING_PATH`). A public repo then publishes no addresses.

```yaml
# In git (a public repo is fine):
kind: Network
name: home-lan
---
# In the addressing file, NETWORK_ADDRESSING_PATH on the web app:
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dhcp_excluded_range: 192.168.1.200-192.168.1.250
dns: [192.168.1.1]
```

The addressing file holds full `kind: Network` documents and nothing else,
parsed and checked like the repo's. Each sync runs parse → resolve → validate:
the resolve step joins git's names with the file's addressing, and the checks
above then run on the joined Networks, so a state-addressed network gets the
same `cidr`, `gateway`, range and `static_ip` checks as a git-addressed one.
The resolve step rejects each of these, naming the network:

- **Partial addressing in git**: a `gateway`, `dhcp_excluded_range` or `dns`
  without a `cidr`. A Network carries all its addressing or none.
- **Both sources**: a network addressed in git that also has an entry in the
  file. Neither wins; remove one.
- **An unknown entry**: an entry in the file for a network git doesn't
  declare, or a second entry for the same network, or one without a `cidr`.
- **No addressing at all**: a name-only Network with no entry in the file.
- **A `static_ip` on a state-addressed network.** Its instances get IPAM
  addresses. Pinning one, held in state rather than git, is #514.
- **Git addressing without `CONFIG_REPO_PRIVATE=true`.**

A rejected sync is logged, naming the network, and `POST /sync`'s status says
where to look. Partial addressing and a `static_ip` on a state-addressed
network are the commit's own faults, whatever the web app's settings: 502, as
for any other invalid commit. The rest answer 409, naming `CONFIG_REPO_PRIVATE`
and `NETWORK_ADDRESSING_PATH`, since the fix may be in git or in those
settings.

The file is read on every sync, so an edit to it takes effect on the next sync
even though the commit hasn't moved. It's also read once at startup: a path
that can't be read or parsed, or an entry with no name, no `cidr`, or the name
of an earlier entry, stops the web app, naming the setting. If it becomes
unreadable later, `POST /sync` answers 500 naming the file rather than 502.
Each state-addressed network whose addressing is loaded for the first time or
changes is reported: logged, in `POST /sync`'s `warnings`, and in
`GET /status`'s `sync_warnings`, which keeps each network's latest until its
addressing changes again or it leaves state addressing (moved into git, or
deleted). The config diff can't attribute those changes, since no commit made
them.

The agent never reads addressing, so it accepts a name-only Network, while
still rejecting partial addressing and a `static_ip` on it. So does
`bootstrap deploy-agent`, which reads the same files and deploys only the
agent. `render-seed` reads a local file and renders addresses, so it keeps
requiring full Network documents. A later step (#514) loads addressing and pins through the CLI into
the web app's encrypted store instead of a file.

### `kind: Instance`

One physical machine, the host OS it runs (`os`, IncusOS unless set), and the
cluster runtime it runs workloads under (`runtime`, Incus unless set).

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
| `os` | string | no, default `incusos` | The host OS, which picks how the machine's seed and image are rendered (`internal/hostos`, `docs/Decisions.md` §31). Only `incusos` is implemented. `talos` and `debian` are known but have no backend yet, and any other value is unknown; each is rejected with its own message (`instances[N].os`). |
| `runtime` | string | no, default `incus` | The cluster runtime, `incus` or `kubernetes`, which picks how the cluster is bootstrapped, joined and upgraded (`internal/clusterruntime`, `docs/Decisions.md` §31). It must be one the `os` can run (the table below), and every Instance in the repo must share it. Only `incus` is implemented; `kubernetes` is known but has no backend yet. It's on the Instance because there's no cluster entity: every Instance in a repo is a member of its one cluster. |
| `name` | string | yes | Unique among Instances, and a lowercase hostname label: 1–63 of `a-z`, `0-9` and `-`, not starting or ending with `-`, not all digits. Lowercase only, because `Node0` and `node0` would be "unique" and yet name the same host. It's the node's identity everywhere downstream: its Incus cluster member name, its cert names, its web-app store key, and the agent's `AGENT_NODE_NAME`. #193 also makes it the node's hostname. The rule is stricter than Incus's own member-name check, so a name that passes is valid everywhere. |
| `mac` | string | yes, in effect | The NIC IncusOS configures as `eth0`. Not validated; a wrong MAC leaves the node's network unconfigured at boot. Quoting is optional. |
| `network` | string | yes | A declared Network's `name`, checked for every instance, DHCP or not. The instance must sit on that network. |
| `static_ip` | address | no | Must be inside the network's `cidr` and its `dhcp_excluded_range` (if set). It can't be the gateway, network or broadcast address. Only allowed on a git-addressed network: on a state-addressed one it's rejected, and IPAM assigns. **Omitted means something different per reader**: the web app assigns one, while `render-seed` renders DHCP (see What each reader does). The address matters beyond networking, because it's what makes a node an Incus cluster member (§26). A DHCP node comes up unclustered. |
| `disk` | string | yes | Only `single` is implemented (checked at render). |
| `nic` | string | yes | Only `single` is implemented (checked at render). |
| `security.tpm` | bool | no, default `false` | Whether the machine has a TPM. `false` tells the IncusOS installer to proceed without one. |
| `security.secure_boot` | bool | no, default `false` | The same, for Secure Boot. |
| `applications` | list | yes | IncusOS host applications. Only `[incus]` is supported (checked at render). This is unrelated to `kind: App`. |

#### `os` and `runtime`

Which runtimes each host OS can run (§31). `internal/config` holds this as one
table, and a new host OS or runtime adds a row:

| `os` | allowed `runtime` |
|---|---|
| `incusos` | `incus` |
| `talos` | `kubernetes` |
| `debian` | `incus` or `kubernetes` |

`Validate` checks each Instance in this order and reports the first that
applies, so each Instance gets the one error that names its fix:

1. An unknown `os` reports "not a known host OS", and no pair check.
2. An unknown `runtime` reports "not a known runtime; known: incus,
   kubernetes", and no pair check.
3. A pair the table doesn't allow reports only that, for example
   `os "talos" only runs kubernetes, not "incus"` (at
   `instances[N].runtime`). That holds even when the `os` or `runtime` also has
   no backend: adding the backend wouldn't make the pair valid.
4. An allowed pair goes on to the no-backend errors, `os` then `runtime`. So
   `os: talos` with `runtime: kubernetes` reports that neither has a backend
   yet.

**One cluster per repo.** Every Instance must resolve to the same `os` and the
same `runtime`, because a cluster's members all run one host OS (§30) and one
runtime. Resolved values are compared, so an omitted field equals its default.
Each Instance that differs from the first is reported at `instances[N].os` or
`instances[N].runtime`, in addition to its own errors above. An Instance whose
`os` or `runtime` is unknown is left out of the comparison, since it already
has its own error. If #181 or #440 later lets a repo hold several clusters,
this check narrows to each cluster's members.

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

### `kind: Designation`

Which node's agent leads: `leaderelection.Designated`, `docs/Decisions.md`
§25 (#101).

```yaml
kind: Designation
primary: node0   # an Instance name: a node, not an agent instance
```

| Field | Type | Required | Rules and meaning |
|---|---|---|---|
| `primary` | string | yes | Must name a declared Instance, exactly as written. |

- **At most one per repo.** A second is a validation issue
  (`designations[1]`). Zero is valid and means no agent acts: the safe
  default, matching `MayAct` treating an unknown designation as "not leader".
- **A standalone host needs one too.** A host in no multi-member cluster is a
  one-member cluster whose primary is its one host (`docs/Decisions.md`
  §30), and it leads only when a Designation names it: no agent is its own
  primary implicitly. Without one, its agent never acts, so it neither
  creates agents nor upgrades itself.
- **So only one cluster or standalone host per repo upgrades unattended.**
  With at most one Designation, the cluster it names is the only one whose
  agent self-upgrades when the agent App's `image` changes (#109). Any other
  standalone host declared in the same repo upgrades by the operator-driven
  CLI fallback (§30, not built yet) until #440 decides how a repo designates
  more than one (post-1.0).
- **There's no `epoch`.** A stale checkout is fenced by git itself: each
  agent publishes the commit it's on, and an agent behind a peer stands down.
  A new primary also waits for the old one to publish that it has stopped
  acting. So a failover is one commit changing `primary` (§25). An `epoch:` left in a repo is rejected as an unknown field
  rather than silently ignored.
- **It names a node, not an agent instance,** so the agent's own blue-green
  self-upgrade needs no change here.
- **Why a kind of its own** rather than `primary: true` on an `Instance` or a
  field on the agent `App`: leadership is fleet-wide, not a property of a
  node or of the agent's image; a separate document shows up on its own in a
  change report; and it can be validated, which a string in `params` can't.
- The web app parses and validates it, but doesn't store it or report
  changes to it yet.

## What each reader does with the repo

| Reader | Reads | Differences |
|---|---|---|
| **Web app** | The branch above, on `POST /sync` or every `CONFIG_SYNC_INTERVAL` | Resolves each Network's addressing from git or the addressing file, runs `Validate` on the result, then IPAM. An omitted `static_ip` gets an address from the network's `dhcp_excluded_range`, stable across syncs. It errors if the range is unset or exhausted. The web app stores the result, reports what changed against the previous sync, and renders each node's seed and image from it. |
| **`bootstrap render-seed`** | One local file (`--file`), no git | Needs **exactly one** Network and one Instance; Apps are ignored. There's no IPAM, so an omitted `static_ip` means DHCP, and the node comes up unclustered. Use [`examples/nodes/single-node/`](https://github.com/ehharvey/homelab-ops/tree/main/examples/nodes/single-node). |
| **Agent** (`cmd/agent`, #101) | The same repo, from its own `CONFIG_REPO_URL`/`CONFIG_REPO_REF` | Runs with the web app down. It keeps a persistent full clone (`AGENT_REPO_DIR`) and fetches into it, rather than a fresh shallow clone, because leader election asks whether a peer's commit is in its history. It runs `Validate` too, except that a name-only (state-addressed) Network passes, and a commit that fails keeps the previous one current. Today it reads only the Designation; it never reads addressing. |

## Known gaps

Things the checks don't catch today, recorded so nobody assumes they do:

- **Names derived from an `Instance.name` can exceed Incus's 63-character
  cap.** A per-node App's instances are named `<app>-<instance>`, and the
  agent's also get a `-g<N>` generation suffix. So a name can pass the rule
  above and still produce an invalid derived one. Capping `Instance.name`
  alone can't fix it, because the length also depends on the App's name and
  on an unbounded generation. The check belongs where the derived name is
  built (#203, and #98 when it resumes).
- **A missing `gateway` passes `Validate`.** It's caught at render instead,
  which is later than an operator would expect.
- **`mac` is free text.**
- **The web app's store doesn't keep `os`.** A stored Instance reads back with
  `os` omitted, which is right while `incusos` is the only accepted value, and
  the config diff treats `os: incusos` and an omitted `os` as the same. The
  store refuses any other `os` rather than storing it as `incusos`. It gains
  the column with the second backend, which needs it to dispatch.
- **No private repos.** Sync passes no credentials. §28's git-addressed
  networks assume a private repo, and `CONFIG_REPO_PRIVATE=true` only
  asserts it: the web app can't check, and today the repo must still be
  anonymously readable. Real private-repo sync needs credentials first. So
  does the agent syncing one (§28 mentions a read-only deploy key).

## Proposed additions

A spike for #207: the schema changes other issues will make, sketched against
the schema above so they can be judged together. **Nothing here is
implemented.** The strict parser rejects every field and kind in this section.
Each owning issue makes the final call; where there's a recommendation, it's
only that.

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
