# Development Conventions

Working conventions established while building the bootstrap CLI (issue #1). Not architecture — see `Architecture.md`/`Roadmap.md` for that — this page is about *how* we work: branching, PRs, Go project layout, and tooling.

## Docs live in-repo, synced to the wiki

Design docs (`Architecture.md`, `Roadmap.md`, `Decisions.md`) live under `docs/` in this repo — that is now the canonical source. This reverses an earlier decision to keep them in the GitHub wiki instead: the wiki repo is a separate clone with no PR review, so doc changes bypassed the normal review flow that code changes go through, and keeping a second working tree (`../wiki`, auto-cloned by the devcontainer) in sync by hand was friction for no real benefit. Docs now get the same PR review as code, in the same checkout, with no separate clone required.

A GitHub Action (`.github/workflows/sync-wiki.yml`) pushes `docs/*.md` to the wiki repo automatically on every push to `main` that touches `docs/**`. The wiki is a **read-only mirror** — never edit pages there directly; any hand-edit will be silently overwritten (or just diverge unnoticed) on the next sync, since the sync only pushes from `docs/` and never pulls wiki changes back. To change doc content, edit `docs/` and open a PR like normal.

Code-level docs (READMEs, package comments) stay in the main repo as usual — that part hasn't changed.

### Supersede in place (#223)

A decision or an issue body always reads as the **current** rule. Changes don't stack.

Stacked changes were the most likely source of a wrong implementation. `Decisions.md` §25 had three addenda, each overriding parts of the body or of the addendum before it. #101's body was a stack of dated scope notes, newest on top, and its handoff prompt had to list by hand which parts to ignore. Every reader, human or Claude, had to reconcile the layers, and paid in tokens to read the dead ones.

**When a decision changes:**

- **Rewrite the rule in place.** Edit the body so it states what holds now. Don't append an addendum that overrides it.
- **Fold findings in where they apply.** What building something settled (a verified behaviour, a narrowed rule) goes into the part of the body it refines.
- **Mark what was replaced as superseded, with a link.** A short pointer where the old rule stood is enough, e.g. "(first chosen with an epoch fence; see History)". Another decision that a change supersedes says so the same way, as §25 does for §17's lease.
- **Keep the history in a closing `### History` subsection.** One dated entry per change, with its issue or PR: what changed and, for a reversal, why. It's a record, not a rule, so a reader after the current rule can stop before it.
- **Evidence can stay as its own subsection** (a spike's measurements, say) when the body cites it. Label it as evidence, not as an addendum.
- **Cite the section, not the addendum** (`§25`, not "§25's #212 addendum"), so citations survive the next change.

**When an issue's scope changes:** edit the body so it states the current spec, and post the change as a comment: a dated note saying what changed and why. The body is what an implementer (or a handoff prompt) reads; the comments are the history.

Sections written before this convention may still end in addenda. The newest one wins where they conflict; fold them in place the next time the section changes.

## Branching & issues

- One branch per issue, named `eharvey/#<issue-number>` (e.g. `eharvey/#1`, `eharvey/#6`).
- Work lands via PR, not direct pushes to `main`.
- PR body includes `Closes #<issue-number>` so merging auto-closes the issue — don't close issues manually.
- **One issue, one commit** (added 2026-07-18, see #119). `main` merges PRs with rebase only — never squash — so a branch with N commits lands as N commits. The branch itself has to be the single commit.

Landing finished work is two steps (`scripts/ship.sh`, `scripts/lgtm.sh`):

- **`make ship`** — pushes and opens the PR with `gh pr create --fill`. Stops there. CI runs while you read the diff. First it makes the branch shippable (#319): a branch of several commits is squashed into one that keeps the *first* commit's message (where the Plan, Test plan and `Closes #<n>` were written; it prints the commits it folded in), and a branch behind `main` is rebased onto it, never merged. It refuses, and pushes nothing, when a rebase is already in progress (finish with `git rebase --continue`, then `make ship`), when its own rebase stops on a conflict (the rebase is left stopped for you to resolve), or when the branch is stacked, meaning another local or `origin` branch contains some of its commits: squashing or rebasing would rewrite them under that branch, so rebase onto `main` after it lands (or `make restack` once #254 provides it). A stale local branch can trip that refusal falsely; `make wt-gc` clears those. Then it copies the issue's details into the commit (#450): the issue is `SHIP_ISSUE`, else the `<n>` of a branch named `*/#<n>`, and any other branch keeps its message (ship says so). The issue's title, prefix stripped, replaces the subject, and a delimited block with its scope, Done-when, a bundle's open sub-issues and `Closes` lines goes before the trailer paragraph, regenerated rather than repeated on each ship; see § PR body format. It amends only when that changes the message, so an unchanged re-ship keeps the SHA, and a failed issue lookup refuses, pushing nothing. Otherwise a branch already one commit ahead of current `main` ships unchanged. Squashing and rebasing live in `ship` rather than a git hook because `pre-push` can't change what it pushes and `post-commit` also fires mid-rebase. Until #281, a branch pushed earlier with no PR open that `ship` then squashes, rebases or amends is refused as non-fast-forward. Run it again after amending the commit (e.g. for review feedback) to re-ship to the open PR (#273): it force-pushes with a lease on the PR's head SHA, so it refuses to overwrite anything pushed since it read the PR, and it leaves the PR's title and body alone (`/address-feedback` syncs them from the commit). It refuses a closed or merged PR.
- **`make lgtm`** — enables auto-merge on the current branch's PR, so it merges once the required checks pass. `make lgtm PR=<n>` for a PR opened another way.

Splitting them (see #125) is what keeps a PR from merging before anyone has looked at it. It's a speed bump, not an enforced control: a real approval gate isn't available to a solo dev, since GitHub won't let you approve your own PR and requiring one review would deadlock every PR. `required_approving_review_count` is therefore 0 and `make lgtm` is the honour-system stand-in.

`lgtm` runs locally rather than as a workflow reacting to an "lgtm" comment on purpose: a merge enabled with `GITHUB_TOKEN` is attributed to `github-actions[bot]`, and GITHUB_TOKEN-driven pushes don't trigger further workflow runs — which would silently stop `sync-wiki.yml` mirroring `docs/**`. A local `gh` keeps the merge attributed to a real user, so push-triggered workflows on `main` keep firing.

One kind of PR skips `make lgtm` (#287): a **provenance-only** incus-os submodule bump, one whose diff is the submodule pointer plus the `// Vendored from github.com/lxc/incus-os @ <sha>` stamps and nothing else. `bump-incusos.yml` opens it and enables auto-merge itself, both with the `ISSUE264_REBASE_BOT_PAT` secret rather than `GITHUB_TOKEN`, so the required checks run and the merge is attributed to the PAT's user, for the reasons above. It still needs every required check. **Every other bump**, and every other PR, still waits for `make lgtm`. See § Vendoring upstream IncusOS seed structs.

Three things enforce the one-commit shape, in order of how fast they tell you:

1. `.githooks/pre-push` refuses a multi-commit push locally. Install once with `make hooks` (the devcontainer does it on start via `.devcontainer/scripts/4-install-git-hooks.sh`). Both also set `commit.cleanup=scissors`; see § PR body format. Both set `core.hooksPath` to the relative `.githooks`, but Claude Code's worktree setup rewrites the shared value to the main checkout's absolute `.githooks` (#279). The hook therefore first sources `.githooks/run-own-copy.sh`, which hands off to the pushing checkout's own `.githooks/pre-push` when that is a different file, so a worktree's branch is checked by its own copy. That holds only when the copy git starts has the hand-off: a main checkout on a branch from before #279, or with that line edited out, checks every worktree's push with its own copy. Any hook added to `.githooks` starts with the same `.` line.
2. The `one-commit` job in `.github/workflows/pr-shape.yml` fails the PR.
3. `one-commit` is a required status check on `main`, so a failing PR can't merge.

`make ship` squashes for you; the hook and the check refuse and print the manual form, which is:

```sh
git fetch origin main
git reset --soft "$(git merge-base FETCH_HEAD HEAD)"
git commit -C "$(git rev-list --reverse FETCH_HEAD..HEAD@{1} | head -1)"
```

It resets to the merge-base rather than to `FETCH_HEAD`, because on a branch behind `main` a soft reset to `FETCH_HEAD` keeps the branch's older tree and silently reverts whatever landed on `main` since. It reuses the first commit's message rather than the last's (`HEAD@{1}`), because the first is where the Plan and Test plan were written. And it takes that message verbatim with `-C`, not `-c`: an edited message goes through git's default `strip` cleanup, which deletes every line starting with `#`, `## Plan` and `## Test plan` included. Amend the message afterwards if it needs changing, the way § PR body format says.

Why one commit rather than squash-merging: squash-vs-rebase was a per-PR judgment call with no right answer (#119), and rebase-only removes the decision while keeping history linear. The cost is moving the squash upstream, to the branch, where a force-push is still cheap and the commit message is still editable.

### Sizing and bundles (#409)

Every issue pays a roughly fixed pipeline cost (scout, worker, lint, review, ship), so a 10-line fix costs about what a 100-line one does. The `small` label and bundles pay it less often.

- **`small`** means the change is expected to be **≤ ~100 lines in one area** of the repo, with **no open design question** and **no new validate script**. It's a sizing hint, not a priority, and there's deliberately no `trivial`. Apply it when the Done-when and Doc references make the size plain; leave it off when unsure. `/file-task` applies it and `/refine-issue` re-sizes against it.
- **Fast lane:** `/run-chain` skips the decisions-scout for a `small` issue whose body cites no decision (no `§<digit>`, no mention of `docs/Decisions.md`; `cites` in `.claude/skills/lib.jq`). The issue body is the worker's whole spec, so a refinement that adds a `§N` citation also moves the issue to the full lane, which is the point of citing it.
- **A bundle** is a `[Task]: Bundle: …` parent issue (`/bundle`) with its `small` issues attached as native sub-issues. It ships as one branch, `eharvey/#<parent>`, and one commit whose message has `Closes #<parent>` plus one `Closes #<n>` per open sub-issue; `audit.sh` checks every one, since a child the commit doesn't close stays Ready and gets redone. While the parent is open, its children never run alone (`/run-chain` skips them, `/next` tags them `bundled`). The parent's blockers are its own plus its children's, and a blocker inside a bundle counts as the bundle. Revert a bundle by closing the parent as not planned; a closed bundle holds nothing back, and `/bundle` can move its children into a new one. An umbrella issue (any other title) doesn't bundle: its sub-issues still run alone.

## PR body format

**Write these two sections in the commit message body, not the GitHub web form.** `gh pr create --fill` (what `make ship` uses) takes the PR title from the commit subject and the PR body verbatim from the commit message body, so a well-formed commit satisfies this convention with nothing retyped — and the rationale stays in `git`, readable offline and greppable via `git log`.

PRs use two sections:

```markdown
## Plan

<What this PR does and why, with enough rationale that a reviewer doesn't
have to re-derive design decisions — e.g. why an algorithm/library/default
was chosen, what alternatives were ruled out and why.>

## Test plan

- [x] <concrete check, e.g. specific test command or manual verification step>
- [x] ...
```

`make ship` owns the rest of the message (#450). It replaces the subject with the issue's title, its `[Task]: `/`[Bug]: `/`[Decision]: ` prefix stripped (an author's `type(scope): ` goes too, so the PR title is the issue's), and adds `scripts/issue-block.sh`'s block between your text and the trailer paragraph:

```markdown
<!-- ship:issue #<n> -->
## Issue #<n>: <title>

<the issue's ### What needs to be built (a bug's ### Expected behavior), verbatim>

### Done when (from #<n>)
<the issue's Done-when items, still `- [ ]`>

Closes #<n>
<!-- /ship:issue -->
```

A bundle's block also has a `### Sub-issue #<m>` section, with its Done-when and `Closes #<m>`, per open sub-issue (§ Sizing and bundles). So write only the Plan, the Test plan, `Closes #<n>` (the review audit checks for it, and the block leaves out a `Closes` line you already wrote) and the trailers; don't copy the issue's text in by hand. Each ship regenerates the block from the issue as it stands and never touches the two sections. The copied text is verbatim except that a closing keyword followed by an issue number (`Closes #12`, `fixes #3`) becomes `Closes issue #12`, so text quoting another issue's `Closes` line can't close that issue when the commit lands. Put `[skip ci]` in the body, since the subject gets replaced.

Keep the "why" in the commit message, not in code comments — code comments should only explain non-obvious *invariants*, not the history of how a feature came to be.

### Amending keeps the sections (#334)

`## Plan` and `## Test plan` start with `#`, so git's default `strip` cleanup deletes them whenever git opens the message for editing. `GIT_EDITOR=true` doesn't prevent it: git still counts the message as edited and strips it. Amend one of two ways:

- **Message unchanged:** `git commit --amend --no-edit`.
- **Message changed:** write the message to a file, edit the file, and commit from it. A message given with `-F` isn't edited, so it gets `whitespace` cleanup and keeps every line:

  ```sh
  git log -1 --format=%B > "$SCRATCH/msg"   # a session scratchpad, not /tmp
  # edit "$SCRATCH/msg"
  git commit --amend -F "$SCRATCH/msg"
  ```

Never use a bare `git commit --amend` or `git commit -c`. The squash recipe in § Branching & issues uses `-C` for the same reason. As a backstop, `make hooks` and the devcontainer set `commit.cleanup=scissors` in the shared repo config, which covers every worktree. Scissors keeps `#` lines, and in a real editor it still drops git's own commented help text, because git writes that text below a scissors line. `whitespace` would keep the help text in the message.

## Roadmap checklist convention

When a `Roadmap.md` checklist item is completed, check it off **in the same commit as the work it's documenting**, and annotate which issue closed it:

```markdown
- [x] Offline self-signed cert/key generation: DONE; see #1
```

This keeps `Roadmap.md` as a live, skimmable status board rather than a static spec.

The check-off used to be required to be *its own* commit. That was dropped for the one-issue-one-commit rule above (2026-07-18, see #119) — it was the only structural reason a compliant PR was ever two commits (it's why #87 was). The `see #N` annotation already carries the traceability the separate commit was standing in for.

**Milestones are named after the phase alone, no separate "Sprint N" counter** (changed 2026-07-03): milestones/issues used to read `Sprint N — Phase M: ...`, but sprint numbers were 1-indexed against 0-indexed phases and every sprint has mapped 1:1 to exactly one phase — so the sprint counter carried no information beyond "which phase, off by one" (`Sprint 3` did `Phase 2`'s work). GitHub milestones are now just `Phase N: <name>`, matching `Roadmap.md`'s headings; the `Sprint planning` issue template is `phase_planning.yml` (`[Phase]: ` issues). Older closed issues that predate this (`[Sprint]: N`) were retitled to match; only issue *bodies*/comments referencing the old scheme are left as historical record.

## Go project layout

Established by the bootstrap CLI (`internal/cert`, `cmd/bootstrap`):

- One binary per `cmd/<binary>/` directory, with `main.go` just wiring up `os.Exit` around a `cmd.Execute()` call.
- Cobra-based CLIs keep their command tree under `cmd/<binary>/cmd/`: a `root.go` defining the root command, and one file per subcommand (e.g. `gen_cert.go`) that self-registers via `init()` calling `rootCmd.AddCommand(...)` — so `root.go` never needs to know about every subcommand that exists.
- Core logic (anything with real behavior worth unit testing, e.g. crypto, parsing, rendering) lives in `internal/<package>/`, decoupled from cobra/CLI concerns entirely — it takes plain Go types in, returns plain Go types out. This is what makes it reusable later (e.g. the web app importing the same cert-generation logic the bootstrap CLI uses) without dragging in CLI flag-parsing.
- Module path: `github.com/ehharvey/homelab-ops` — everything (bootstrap CLI, and eventually the web app) lives in one module/repo per the Roadmap, so `internal/` packages are shared freely across binaries in this repo.

**Prefer stdlib over vendoring a big dependency for a small piece of logic.** When the bootstrap CLI needed self-signed cert generation, Incus itself exports the exact same logic (`github.com/lxc/incus/v7/shared/tls.GenerateMemCert`) — but importing it would pull in that module's full daemon dependency tree (~1000 lines of unrelated `go.sum`, including things like AWS SDK and dqlite) for one ~40-line helper. We replicated Incus's cert shape (ECDSA P-384, 10-year validity, `ExtKeyUsageClientAuth`) using only `crypto/x509` instead. Default to this judgment call: copy a small, stable algorithm rather than import a large module for it, especially when the upstream package isn't designed as a stable public API (Incus's own module has already bumped v6→v7).

## YAML fleet-definition parsing (issue #2)

Established by `internal/config` (the `Network`/`Instance` parser):

- **Library: `gopkg.in/yaml.v3`, not `sigs.k8s.io/yaml`.** Same "small, stable dependency" judgment call as the cert library decision above — `yaml.v3` is native-YAML-tag-based and has no transitive deps beyond its own test-only `gopkg.in/check.v1`, vs. `sigs.k8s.io/yaml`'s JSON-tag-based round-trip approach and larger k8s-adjacent dependency surface, for a feature that doesn't need any other k8s tooling.
- **Discriminated multi-doc decode pattern:** decode each YAML document into a `yaml.Node` first, peek its `kind:` field via a tiny one-field `discriminator` struct, then decode the node into the matched concrete type (`Network` or `Instance`). This is the standard way to do k8s-style polymorphic decoding with `yaml.v3` — a single `Decode` call can't pick its target type before seeing the data. The second decode is **strict** (see the unknown-field note below), which `yaml.Node.Decode` can't do — `KnownFields` is a `yaml.Decoder` option the node-decode path bypasses — so the node is re-marshalled and run back through a `yaml.Decoder` with `KnownFields(true)`, with the `kind` discriminator inlined (`,inline`) alongside the concrete type so the required field isn't itself flagged as unknown. Reuse this pattern rather than reinventing it when the web app's config-sync module (Roadmap Phase 1) parses the same multi-doc format.
- **No cardinality enforcement at the parse layer.** `config.Parse` collects every `Network`/`Instance` document it finds into slices — zero, one, or many of each — without erroring on count. The bootstrap CLI's *effective* single-node behavior in 0.x comes from the fleet YAML files we hand-author only containing one of each, not from a parser-level restriction. This keeps the parser reusable as-is once the web app needs to handle a whole fleet's worth of documents, instead of needing to relax a 0.x-era restriction later.
- **Unknown or misspelled fields are parse errors, not silent drops.** Decoding is strict (`KnownFields(true)`, see above): a typo'd key — `statc_ip` for `static_ip` — fails the parse instead of leaving the field zero-valued. These configs provision real hardware, so a dropped `static_ip` silently becoming a DHCP fallback is exactly the failure that must surface loudly rather than at boot. A missing or unrecognized `kind:` is likewise always an error; there's no silent skip of unrecognized documents.
- Diagnostic-only CLI subcommands are an acceptable judgment call even before a real consumer exists: `bootstrap parse-config --file <path>` only prints a summary of what was parsed (no seed rendering yet), added specifically to satisfy a "done when the CLI can read X" criterion without building a half-finished renderer to justify the command's existence.

## Config validation (issues #46, #47)

Decided in `Decisions.md` § Validation approach (#47): **roll our own, as a stdlib `net/netip` parse/validate split** — no validation library (`go-playground/validator`/`zog` both evaluated and rejected there). Conventions for implementing it (#46):

- **Parse stays structural + syntactic; semantics are a separate pass.** `config.Parse` keeps doing only multi-doc decode + strict unknown-field detection — it stays cardinality-agnostic and reusable, per the YAML-parsing note above. A separate `config.Validate(Config) Issues` does the cross-field semantic checks (gateway-∈-CIDR, range-∈-CIDR, static-ip-∈-CIDR-and-∈-range, DNS-are-IPs, name-non-empty). Don't fold semantics back into `Parse`.
- **Typed fields, not strings.** `config.Network`/`config.Instance` fields move to `net/netip` types: `CIDR → netip.Prefix`, `Gateway`/`StaticIP → netip.Addr`, `DNS → []netip.Addr`, `DHCPExcludedRange →` a small `Range` type with a custom `UnmarshalText` that splits on `-` into two `netip.Addr`. `MAC` stays a `string` — `net.HardwareAddr` has no stdlib `TextUnmarshaler`.
- **Why typed parsing is "free" here — the non-obvious part:** `gopkg.in/yaml.v3` honors `encoding.TextUnmarshaler`, and `netip.Addr`/`netip.Prefix` implement it, so they unmarshal directly from the synced YAML (and JSON, for the store/API) with syntactic validation happening *at parse time*. Optional fields need no wrapper type: an empty (`gateway: ""`) or omitted value both land as a zero `netip.Addr{}` with `IsValid()==false` and **no** error — distinguishable from a set value via `IsValid()`. This premise is pinned by `ExampleNetwork_yamlUnmarshal` in `internal/config` (an `Example` test with an `// Output:` block, runnable via `go test ./internal/config -run Example`): its assertions fail CI if a future `yaml.v3` bump ever stops honoring `TextUnmarshaler` or starts erroring on empty strings — don't assume either (it doesn't, and they don't).
- **Validation returns a value, not behaviour.** The output type is `Issue{Path, Message}` (e.g. `Path: "networks[0].gateway"`), with an `Issues` slice that satisfies `error`/has an `Empty()`. This is deliberately the shape a library like `zog` produces (`issue.Path`/`issue.Message`): it's the entire seam that lets validation be re-driven by a library later *if* an untrusted-input surface ever appears, without changing callers. Do **not** build a `Validator` interface/registry abstraction for that — the value-returning function is the whole hedge.
- **Where `Validate` runs.** As an explicit pass inside `server.SyncOnce` (`internal/server/server.go`), right beside `ipam.Assign`, classified under a new `ErrValidate` that maps to HTTP 502 — bad upstream repo content, the same stage-classification rationale as the existing `ErrIPAM`.
- **Consolidate, don't add a fourth copy.** `ipam.ValidateStaticIP`/`ExcludedRangeBounds` and `internal/seed`'s re-validation collapse into the typed model — `netip` is a comparable value usable as a map key (`net.IP` is not), so ipam's hand-rolled `compareIP`/`nextIP`/`broadcastAddr` byte-twiddling reduces to `netip`'s `.Next()`/`.Compare()`/prefix math, and `configdiff`'s `reflect.DeepEqual` keeps working (with `==` now also available).

## Vendoring upstream IncusOS seed structs (issue #3)

Established by `internal/seed` (the install/network/applications seed renderer) and `internal/third_party/incusos` (vendored upstream types):

- **`lxc/incus-os` is checked out as a sparse git submodule at `third_party/incus-os`**, pinned to a specific commit, sparse-checked-out to just `incus-osd/api` (root files like `COPYING` come along for free under cone-mode sparse-checkout). This is the explicit, git-tracked pointer to which upstream commit our seed format matches — bump it via `git -C third_party/incus-os checkout <new-sha>` + `git add third_party/incus-os`.
- **`scripts/vendor-incusos.sh` (`make vendor-incusos`) copies the small set of files we actually need from the submodule into `internal/third_party/incusos/`**, stamping each with the submodule's current commit SHA and an Apache-2.0 attribution header. This is the layer our Go code actually imports — the submodule by itself is just a reference checkout, not something `go build` touches. The only hand-edit the script makes is rewriting `seed/network.go`'s import path from `github.com/lxc/incus-os/incus-osd/api` to our local vendored path; every other file is byte-for-byte upstream.
- **`.github/workflows/bump-incusos.yml` opens the bump PR, and auto-merges it only when it is provenance-only (#287).** Most new upstream tags don't touch `incus-osd/api`, so the re-vendor changes nothing but the stamp line in each file. `scripts/incusos-bump-is-trivial.sh` checks the uncommitted result of `vendor-incusos.sh` and calls it provenance-only when all of these hold: the only changed paths are `third_party/incus-os` and files under `internal/third_party/incusos/`; no vendored file is added, deleted, renamed or changes mode; and every changed vendored line matches `^[-+]// Vendored from github.com/lxc/incus-os @ [0-9a-f]{40}$`. Such a PR gets auto-merge from the workflow and lands with no `make lgtm` once the required checks pass. **Every other bump needs `make lgtm`**: a struct field, an added or removed upstream file, or a `go.mod` pin moved by the script can pass `go build`/`go test` and still drift `internal/seed`'s field mapping silently, so a human reads it. The PR body says which path was taken; on the review path it lists the files that changed beyond the stamps, with a diff excerpt. The classifier's cases are tested in `scripts/incusos_bump_test.go`, part of `make test`.
- **Nested under `internal/`, not top-level `third_party/`:** Go's `internal/` import boundary stops this vendored copy from ever becoming a stable dependency surface for code outside this module — it's plumbing for `internal/seed`, not a public API. The `third_party` path segment underneath still flags "this code isn't ours" to a reader. (The top-level `third_party/incus-os` submodule is the *raw upstream checkout*; `internal/third_party/incusos` is the *Go package actually compiled* — two different things that happen to share a name fragment.)
- **Why vendor instead of `go get`-ing `github.com/lxc/incus-os/incus-osd/api/seed` directly:** the `incus-osd` module's `go.mod` pulls in `tailscale.com`, the full `incus/v7` tree, FuturFusion's modules, etc. (~30 direct deps) for module-graph resolution, even though the seed/network structs themselves only import Go stdlib. Same "small, stable dependency" judgment call as the cert-generation precedent above — copy the small self-contained piece rather than import the module it happens to live in.
- **Why vendor instead of hand-authoring our own equivalent structs:** there's no canonical example `install.yaml`/`network.yaml` in upstream's docs to check our format against (confirmed against `doc/reference/seed.md`) — the structs *are* the reference format. Using the real upstream types means our rendered YAML matches by construction instead of by careful transcription.
- `internal/seed.Render` is where single-instance/single-network cardinality enforcement lives (disk/NIC must be `"single"`, exactly one matching `Network` per `Instance`) — consistent with the note in the YAML-parsing section above that `config.Parse` itself stays cardinality-agnostic and this is deferred to whatever consumes `Config` next.

**Follow-up TODO (not yet done):** `internal/cert` predates this vendoring approach and currently hand-replicates Incus's cert shape via stdlib `crypto/x509` rather than vendoring Incus's own cert-generation source. Worth revisiting whether the same "vendor a small, license-permissive, self-contained file" reasoning should apply there too.

## Preseeding Incus cert trust via `incus.yaml` (issue #5)

Established by `internal/seed`'s `incus.yaml` rendering and `scripts/validate/node-boots-and-trusts-bootstrap-cert.sh`:

- **Import `lxc/incus/v7/shared/api` directly instead of vendoring it.** Unlike `install.yaml`/`network.yaml` (vendored into `internal/third_party/incusos`, see issue #3 above), `incus.yaml`'s `InitPreseed`/`CertificatesPost` types are imported straight from `github.com/lxc/incus/v7/shared/api`. Confirmed via `go list -deps` that this specific subpackage pulls in only stdlib + one yaml module — not the `client`/`incus-osd` dependency trees this repo otherwise avoids. The "vendor a small piece instead of importing a big module" judgment call (see the cert-generation and issue #3 precedents above) only applies when the import would otherwise be heavy; when it isn't, importing the real upstream type directly is simpler and keeps us byte-for-byte correct against Incus's actual API shape.
- **`scripts/vendor-incusos.sh` pins `go.mod`'s `lxc/incus/v7` version to whatever the `incus-osd` submodule itself depends on** (reads it out of the submodule's own `go.mod`, then `go get`s that exact version), rather than letting the two version pins drift independently. This runs automatically as part of the existing `.github/workflows/bump-incusos.yml`, so a submodule bump can't silently leave our `InitPreseed` usage out of sync with what a real `incus-osd` build expects.
- **`Certificate` must be base64-encoded raw DER, not PEM** — Incus's `CertificatesPost.Certificate` field expects that exact encoding (per its `CertificatePut`/`CertificatesPost` shape), so `renderIncusPreseed` strips the PEM wrapper (`encoding/pem.Decode`) before base64-encoding the raw bytes. Getting this wrong fails silently at the YAML level (it's still a valid string) and only surfaces as a real Incus node refusing to trust the cert.
- **Validation scripts should drive the real stack, not just check that files exist or that unit tests pass.** `scripts/validate/node-boots-and-trusts-bootstrap-cert.sh` drives the full pipeline — `gen-cert` → `render-seed` → `build-image` → boot a real Incus VM off the produced `.img` → poll the node's static IP over HTTPS with the cert — as one end-to-end check that proves both install success and cert trust simultaneously. This caught real bugs a file-existence check would have missed (see below); prefer this pattern whenever there's a real artifact or service in the loop. Requires an IncusOS base image: `INCUSOS_BASE_IMAGE` defaults to the version pinned in `scripts/incusos-base.version` once `make incusos-base` has fetched, verified and cached it (#296; `scripts/validate/README.md` § The IncusOS base image), and that section skips gracefully when neither the variable nor the cached pinned image is there. A real VM boot is one instance of this principle, not the only one — `scripts/validate/store-retains-synced-fleet.sh`/`sync-warns-on-config-diff.sh` apply the same idea to the web app: drive the real `docker compose` stack against a real git remote, pushing a second commit to it to prove store-replace and diff behavior, rather than relying on `go test` alone.
- **Real-VM testing surfaced bugs invisible to unit tests** — worth the cost of running it before considering an issue genuinely done: `flasher.go`'s `seedFiles` list wasn't updated when `incus.yaml` was added, so the cert preseed was silently dropped from every built `.img` (the actual reason cert trust never worked, despite `incus.yaml` rendering correctly in isolation); IncusOS requires a network interface's `Name` to be set or it's left unconfigured at boot; a static IP with no default route leaves a node unable to reach anything off-subnet (now an explicit `Render` error if `Network.Gateway` is unset and an instance has a `static_ip`). None of these were catchable by testing the seed-rendering logic alone — only booting a real seeded image surfaced them.

## Web app dependency choices (issues #20, #21)

Established by `internal/configsync` and `internal/store`:

- **`github.com/go-git/go-git` for config sync, not shelling out to a `git` binary.** Cloning in-process (into an in-memory filesystem via `go-billy`'s `memfs`, see `internal/configsync/sync.go`) means the deployed binary/distroless image has no `git` on `$PATH` to depend on — consistent with the no-k8s, minimal-deployment goal from #18.
- **`modernc.org/sqlite` for the local store, not `mattn/go-sqlite3`.** It's a pure-Go, CGO-free driver, which keeps `CGO_ENABLED=0` builds and the distroless `Dockerfile` working without a C toolchain in the image — same "small, stable, dependency-light" judgment call as the cert/YAML precedents above, just applied to a runtime dependency instead of a vendored algorithm. `store.Open` defaults to `:memory:` (non-persistent, scoped to the process) unless `STORE_PATH` is set to a file.

## Linting & CI

- `golangci-lint` config lives in `.golangci.yml` at the repo root. Start from `default: standard` and add linters deliberately rather than maximal strictness — e.g. `gosec` is non-negotiable for any package handling key material, but noisy style linters (`cyclop`, `dupl`, `funlen`, `wsl`, etc.) are left off a young codebase to avoid PR friction.
- `Makefile` targets: `build`, `test` (`-race -cover`), `lint`, `lint-docs`, `validate`, `validate-hardware`, `fmt`, `tidy`, `clean`, `hooks`, `ship`, `lgtm`. Plain Make — no justfile/task-runner, since Make is already in the devcontainer base image.
- **`make validate` runs the validation suite's unattended subset** (`scripts/validate/`, ~2.5 minutes, Docker only); `make validate-hardware` runs the Incus/VM subset, which boots real VMs and needs the host. Both go through `scripts/validate/run.sh`, which discovers scripts from their own `VALIDATE_GROUP` declarations rather than a list — so a new script is covered without editing anything central, the same reasoning as `lint-mermaid.sh` globbing `docs/*.md`. An unmet prerequisite is a **skip with its own exit code**, not a failure: see `scripts/validate/README.md` and `docs/Decisions.md` §20.
- CI (`.github/workflows/ci.yml`) runs four parallel jobs on push/PR to `main`: `build-test` (`go build` + `go test`), `lint` (`golangci-lint-action`), `docker-smoke` (builds the web image, proves `flasher-tool` executes inside the distroless final stage and the app serves `/healthz`, then, on a push to `main` only, publishes it to `ghcr.io/ehharvey/homelab-ops/web` as the commit SHA plus `latest`, #240), and `agent-image` (the same for the agent image, #102). The publish is a step gated by `if:`, not a separate job, so the checks that precede it gate it and the job still reports a status on PRs. `.github/workflows/pr-shape.yml` adds a fifth check, `one-commit`, on PRs only.
- **`build-test`, `lint`, `docker-smoke`, and `one-commit` are required status checks** on `main` (added 2026-07-18, see #119) — before that they were advisory and a PR with red CI was mergeable. Two constraints on that ruleset worth knowing before editing it:
  - `strict_required_status_checks_policy` is **on** (#264), so a PR merges only after CI has run against current `main`. That's compatible with the one-commit rule only because branches are **rebased, never merge-updated**. The "Update branch" button's default mode adds a *merge commit to the branch*, which makes it two commits and fails `one-commit`, so don't use it; use "Update with rebase", or rebase locally. Strict mode on its own would leave every other auto-merge PR `BEHIND` each time one lands. `.github/workflows/auto-rebase.yml` (`scripts/auto-rebase.sh`) rebases them on every push to `main`, and when `make lgtm` enables auto-merge. It touches only PRs that have auto-merge enabled, are based on `main` and come from this repo; a PR still being worked on mustn't be force-pushed under its author, and stacked PRs belong to `make restack` (#254). A conflicting PR gets one comment per head SHA telling you to rebase locally, and nothing else. The rebase runs with the `ISSUE264_REBASE_BOT_PAT` secret, a fine-grained PAT, rather than `GITHUB_TOKEN`, because a `GITHUB_TOKEN` push triggers no workflows and the rebased PR's required checks would never run. After a rebase on GitHub, your local branch no longer matches the remote (#263).
  - `docs.yml`'s `mermaid` job must **not** be added to the required list. It's path-filtered to `docs/**`, and a workflow skipped by a path filter reports no status at all, so requiring it would leave every non-docs PR pending forever. (A job skipped by an `if:` condition reports `skipped`, which *does* satisfy the rule — the distinction is easy to get wrong.)
- `make lint` runs `golangci-lint` via the official `golangci/golangci-lint` Docker image rather than a locally installed binary — no toolchain setup needed beyond Docker, and the version is pinned in one place instead of relying on whatever happens to be on a contributor's `$PATH`. That place is `.golangci-lint-version` (#329), and everything that runs golangci-lint reads it: the `Makefile` builds the `LINT_IMAGE` tag from it, and CI's `lint` job passes it to `golangci-lint-action` as `version-file` (the action's default is `latest`, which let CI drift ahead of `make lint`). To bump golangci-lint, edit that file alone. It runs under `flock` on `lint.lock` in the shared git dir, so runs from different worktrees go one at a time (#282): each fresh container re-downloads modules and peaks near 3 GiB, and four at once each took 335 s instead of 104 s and pushed the host into swap. A SIGKILLed `docker run` client, or a killed `flock` (which under `-o` frees the lock at once, #301), leaves its container running, so the locked run labels its container `homelab.lint.lock=<lock path>` and, under the lock and before starting its own, `docker rm -f`s every running container carrying that label, printing which it reaped (#292). With the lock held, any such container is an orphan. Worktrees share the lock path, so they reap each other's orphans; a separate clone's label value differs and is never matched. Without `flock` (macOS) or outside a git checkout it runs unlocked and unlabelled.
- **`make fmt` formats with that same golangci-lint**: it runs `golangci-lint fmt` in `LINT_IMAGE`, which applies the formatters `.golangci.yml` enables (`gofmt`, `goimports`), so a tree `make fmt` leaves behind can't fail `make lint` on formatting (#329). Before that it ran a separately pinned `goimports` binary, whose version could drift from the one bundled in the image. It needs Docker and takes ~25 s, most of it container start. It walks the tree from the repo root, skipping dot directories (so the worktrees under `.claude/worktrees/`) by default; `.golangci.yml`'s `formatters.exclusions.paths` keeps it out of the upstream submodule in `third_party/`. It doesn't take `lint.lock`, because it loads no packages or modules and so costs little beside a running lint.
- **`make lint-docs` proves `docs/`'s mermaid diagrams actually parse** (`scripts/lint-mermaid.sh`, run in CI by `.github/workflows/docs.yml` when `docs/**` changes). It follows `make lint`'s shape exactly — the official `mermaid-cli` image, pinned in one place, no local toolchain beyond Docker, so a Go repo gains no `package.json`. This exists because reviewing a diagram's *source* in a PR diff tells you nothing about whether it *renders*: `docs/AppManager.md`'s sequence diagram showed a parse error instead of a diagram on GitHub from #105 until #111, and #106 reviewed and corrected that very diagram's logic in between without anyone noticing it wasn't displaying. Same lesson as `CLAUDE.md`'s issue-#5 note (a passing test suite that still shipped a silently dropped seed file), applied to docs.

## Generated artifacts never get committed

Anything an offline tool generates locally (certs, keys, seeds, images) goes under a single gitignored directory (e.g. `bootstrap-output/`) rather than scattered paths — one `.gitignore` entry covers all of it, and it's much harder to accidentally commit a private key.
