# Development Conventions

Working conventions established while building the bootstrap CLI (issue #1). Not architecture — see `Architecture.md`/`Roadmap.md` for that — this page is about *how* we work: branching, PRs, Go project layout, and tooling.

## Docs live in-repo, synced to the wiki

Design docs (`Architecture.md`, `Roadmap.md`, `Open Questions.md`) live under `docs/` in this repo — that is now the canonical source. This reverses an earlier decision to keep them in the GitHub wiki instead: the wiki repo is a separate clone with no PR review, so doc changes bypassed the normal review flow that code changes go through, and keeping a second working tree (`../wiki`, auto-cloned by the devcontainer) in sync by hand was friction for no real benefit. Docs now get the same PR review as code, in the same checkout, with no separate clone required.

A GitHub Action (`.github/workflows/sync-wiki.yml`) pushes `docs/*.md` to the wiki repo automatically on every push to `main` that touches `docs/**`. The wiki is a **read-only mirror** — never edit pages there directly; any hand-edit will be silently overwritten (or just diverge unnoticed) on the next sync, since the sync only pushes from `docs/` and never pulls wiki changes back. To change doc content, edit `docs/` and open a PR like normal.

Code-level docs (READMEs, package comments) stay in the main repo as usual — that part hasn't changed.

## Branching & issues

- One branch per issue, named `eharvey/#<issue-number>` (e.g. `eharvey/#1`, `eharvey/#6`).
- Work lands via PR, not direct pushes to `main`.
- PR body includes `Closes #<issue-number>` so merging auto-closes the issue — don't close issues manually.

## PR body format

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

Keep the "why" in the PR description, not in code comments — code comments should only explain non-obvious *invariants*, not the history of how a feature came to be.

## Roadmap checklist convention

When a `Roadmap.md` checklist item is completed, check it off in its own commit (not bundled into the code commit it's documenting) and annotate which issue closed it:

```markdown
- [x] Offline self-signed cert/key generation: DONE; see #1
```

This keeps `Roadmap.md` as a live, skimmable status board rather than a static spec.

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

Decided in `Open Questions.md` § Validation approach (#47): **roll our own, as a stdlib `net/netip` parse/validate split** — no validation library (`go-playground/validator`/`zog` both evaluated and rejected there). Conventions for implementing it (#46):

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
- **Nested under `internal/`, not top-level `third_party/`:** Go's `internal/` import boundary stops this vendored copy from ever becoming a stable dependency surface for code outside this module — it's plumbing for `internal/seed`, not a public API. The `third_party` path segment underneath still flags "this code isn't ours" to a reader. (The top-level `third_party/incus-os` submodule is the *raw upstream checkout*; `internal/third_party/incusos` is the *Go package actually compiled* — two different things that happen to share a name fragment.)
- **Why vendor instead of `go get`-ing `github.com/lxc/incus-os/incus-osd/api/seed` directly:** the `incus-osd` module's `go.mod` pulls in `tailscale.com`, the full `incus/v7` tree, FuturFusion's modules, etc. (~30 direct deps) for module-graph resolution, even though the seed/network structs themselves only import Go stdlib. Same "small, stable dependency" judgment call as the cert-generation precedent above — copy the small self-contained piece rather than import the module it happens to live in.
- **Why vendor instead of hand-authoring our own equivalent structs:** there's no canonical example `install.yaml`/`network.yaml` in upstream's docs to check our format against (confirmed against `doc/reference/seed.md`) — the structs *are* the reference format. Using the real upstream types means our rendered YAML matches by construction instead of by careful transcription.
- `internal/seed.Render` is where single-instance/single-network cardinality enforcement lives (disk/NIC must be `"single"`, exactly one matching `Network` per `Instance`) — consistent with the note in the YAML-parsing section above that `config.Parse` itself stays cardinality-agnostic and this is deferred to whatever consumes `Config` next.

**Follow-up TODO (not yet done):** `internal/cert` predates this vendoring approach and currently hand-replicates Incus's cert shape via stdlib `crypto/x509` rather than vendoring Incus's own cert-generation source. Worth revisiting whether the same "vendor a small, license-permissive, self-contained file" reasoning should apply there too.

## Preseeding Incus cert trust via `incus.yaml` (issue #5)

Established by `internal/seed`'s `incus.yaml` rendering and `scripts/validate-issue-5.sh`:

- **Import `lxc/incus/v7/shared/api` directly instead of vendoring it.** Unlike `install.yaml`/`network.yaml` (vendored into `internal/third_party/incusos`, see issue #3 above), `incus.yaml`'s `InitPreseed`/`CertificatesPost` types are imported straight from `github.com/lxc/incus/v7/shared/api`. Confirmed via `go list -deps` that this specific subpackage pulls in only stdlib + one yaml module — not the `client`/`incus-osd` dependency trees this repo otherwise avoids. The "vendor a small piece instead of importing a big module" judgment call (see the cert-generation and issue #3 precedents above) only applies when the import would otherwise be heavy; when it isn't, importing the real upstream type directly is simpler and keeps us byte-for-byte correct against Incus's actual API shape.
- **`scripts/vendor-incusos.sh` pins `go.mod`'s `lxc/incus/v7` version to whatever the `incus-osd` submodule itself depends on** (reads it out of the submodule's own `go.mod`, then `go get`s that exact version), rather than letting the two version pins drift independently. This runs automatically as part of the existing `.github/workflows/bump-incusos.yml`, so a submodule bump can't silently leave our `InitPreseed` usage out of sync with what a real `incus-osd` build expects.
- **`Certificate` must be base64-encoded raw DER, not PEM** — Incus's `CertificatesPost.Certificate` field expects that exact encoding (per its `CertificatePut`/`CertificatesPost` shape), so `renderIncusPreseed` strips the PEM wrapper (`encoding/pem.Decode`) before base64-encoding the raw bytes. Getting this wrong fails silently at the YAML level (it's still a valid string) and only surfaces as a real Incus node refusing to trust the cert.
- **Validation scripts should drive the real stack, not just check that files exist or that unit tests pass.** `scripts/validate-issue-5.sh` (`make validate-issue-5`) drives the full pipeline — `gen-cert` → `render-seed` → `build-image` → boot a real Incus VM off the produced `.img` → poll the node's static IP over HTTPS with the cert — as one end-to-end check that proves both install success and cert trust simultaneously. This caught real bugs a file-existence check would have missed (see below); prefer this pattern whenever there's a real artifact or service in the loop. Requires `INCUSOS_BASE_IMAGE`; skips that section gracefully when unset. A real VM boot is one instance of this principle, not the only one — `scripts/validate-issue-21.sh`/`validate-issue-22.sh` apply the same idea to the web app: drive the real `docker compose` stack against a real git remote, pushing a second commit to it to prove store-replace and diff behavior, rather than relying on `go test` alone.
- **Real-VM testing surfaced bugs invisible to unit tests** — worth the cost of running it before considering an issue genuinely done: `flasher.go`'s `seedFiles` list wasn't updated when `incus.yaml` was added, so the cert preseed was silently dropped from every built `.img` (the actual reason cert trust never worked, despite `incus.yaml` rendering correctly in isolation); IncusOS requires a network interface's `Name` to be set or it's left unconfigured at boot; a static IP with no default route leaves a node unable to reach anything off-subnet (now an explicit `Render` error if `Network.Gateway` is unset and an instance has a `static_ip`). None of these were catchable by testing the seed-rendering logic alone — only booting a real seeded image surfaced them.

## Web app dependency choices (issues #20, #21)

Established by `internal/configsync` and `internal/store`:

- **`github.com/go-git/go-git` for config sync, not shelling out to a `git` binary.** Cloning in-process (into an in-memory filesystem via `go-billy`'s `memfs`, see `internal/configsync/sync.go`) means the deployed binary/distroless image has no `git` on `$PATH` to depend on — consistent with the no-k8s, minimal-deployment goal from #18.
- **`modernc.org/sqlite` for the local store, not `mattn/go-sqlite3`.** It's a pure-Go, CGO-free driver, which keeps `CGO_ENABLED=0` builds and the distroless `Dockerfile` working without a C toolchain in the image — same "small, stable, dependency-light" judgment call as the cert/YAML precedents above, just applied to a runtime dependency instead of a vendored algorithm. `store.Open` defaults to `:memory:` (non-persistent, scoped to the process) unless `STORE_PATH` is set to a file.

## Linting & CI

- `golangci-lint` config lives in `.golangci.yml` at the repo root. Start from `default: standard` and add linters deliberately rather than maximal strictness — e.g. `gosec` is non-negotiable for any package handling key material, but noisy style linters (`cyclop`, `dupl`, `funlen`, `wsl`, etc.) are left off a young codebase to avoid PR friction.
- `Makefile` targets: `build`, `test` (`-race -cover`), `lint`, `fmt`, `tidy`, `clean`. Plain Make — no justfile/task-runner, since Make is already in the devcontainer base image.
- CI (`.github/workflows/ci.yml`) runs two parallel jobs on push/PR to `main`: `build-test` (`go build` + `go test`) and `lint` (`golangci-lint-action`).
- `make lint` runs `golangci-lint` via the official `golangci/golangci-lint` Docker image (pinned to a specific tag in the `Makefile`'s `LINT_IMAGE` var) rather than a locally installed binary — no toolchain setup needed beyond Docker, and the version is pinned in one place instead of relying on whatever happens to be on a contributor's `$PATH`.

## Generated artifacts never get committed

Anything an offline tool generates locally (certs, keys, seeds, images) goes under a single gitignored directory (e.g. `bootstrap-output/`) rather than scattered paths — one `.gitignore` entry covers all of it, and it's much harder to accidentally commit a private key.
