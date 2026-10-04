# CLAUDE.md

Guidance for Claude Code working in this repo. Context lives in `docs/`, but
read it **selectively**: it totals ~90k tokens, and everything you read is
carried, and paid for, on every later turn of the session.

- `docs/Architecture.md` — what this app is and how the pieces fit; read it
  for orientation
- `docs/Config Schema.md` — the fleet config repo's format, field by field
  (`internal/config` is the source of truth; `examples/` must keep passing)
- `docs/Roadmap.md` — current phase/status; check off items you complete.
  Read the current phase's section (`grep -n '^## Phase'`), not the whole file
- `docs/Development Conventions.md` — the rationale behind the Conventions
  summary below. That summary covers routine work. Before writing code that
  touches CI, lint config, vendoring, Go layout, or the ship flow, read the
  relevant section (`grep -n '^## '`)
- `docs/Decisions.md` — resolved design decisions (~45k tokens; **never read
  it whole**). `grep -n '^## ' docs/Decisions.md` is the index. Read the
  sections that the issue or the files you touch cite
  (`grep -rn '§[0-9]\+' <paths>`), one range at a time:
  `sed -n '/^## 25\./,/^## 26\./p' docs/Decisions.md`

## Keeping context lean and current

Across 12 sessions reviewed in #221, docs were ~40% of all tool output, and
long multi-issue sessions were ~77% of the cost.

- **Bound every output**: `gh … --json … --jq`, `grep`/`tail` on logs,
  `sed -n` ranges on large files. When output is saved to a tool-results file
  for being too large, grep or `sed -n` it; never read it whole, because that
  just brings the same bulk back a turn later.
- **Navigate Go code with the `gopls` MCP tools, not grep/`cat`** (#226;
  grepping and reading Go was ~11% of tool output in #221). `go_search` finds a
  symbol by name; `go_symbol_references` (`file` + `symbol`, spelled as that
  file refers to it, e.g. `config.Parse`) finds type-aware uses that grep
  misses; `go_file_context` summarises a file's cross-file dependencies;
  `go_diagnostics` checks build errors after edits. `go_package_api` dumps a
  whole package's API, so use it on small packages only (Incus `shared/api` is
  ~185 KB). Keep grep for Markdown, YAML, logs and string literals.
- **Supersede in place; newest wins.** A decision's body and an issue's body
  state the current rule; history goes in a `### History` subsection or in
  issue comments (`docs/Development Conventions.md`, #223). Where older text
  still stacks dated notes or addenda, the newest overrides older text where
  they conflict. Say which parts you're treating as superseded.
- **Verify tradeoff claims before presenting them.** For dependency weight,
  use `go mod graph | grep <module>` (is it already in the graph?) and
  `go mod why -m <module>` (is it built today?). For Incus behaviour, check the
  module-cache source or run a real probe. Otherwise, label the claim unverified.
- **One issue per session.** After a PR ships, don't roll into the next issue
  in the same context. Post anything not yet captured (findings, open
  questions) as an issue comment, then suggest a fresh session. The one
  sanctioned exception is **`/run-chain`**, which the operator invokes. Its
  root session implements nothing; it collects briefs and hand-back reports
  while `issue-worker`s do the work, so its context stays small across issues. Operator
  note: resuming a large session after more than an hour idle, or switching
  models mid-session, rewrites its whole context into the cache.
- **Mechanics.** In a worktree-isolated session, run git as plain standalone
  commands; git inside Python heredocs or long `&&` chains gets refused. Wait
  on CI with `gh pr checks <n> --watch` in the background, not `sleep` loops.

## Commands

    make build   # builds bin/bootstrap (CLI), bin/web (web app) and bin/agent (app-manager agent)
    make test    # go test ./... -race -cover
    make lint    # golangci-lint via Docker — slow; run before declaring done
    make fmt     # gofmt + goimports

    make wt N=<n>  # worktree for issue #n on eharvey/#<n>; prints its path
    make wt-gc     # remove worktrees whose PR merged (make wt-list: dry run)

    make validate           # the unattended validate suite (~2.5m, needs Docker)
    make validate-hardware  # the Incus/VM subset (~30m, boots real VMs)

## Conventions (see docs/Development Conventions.md for full detail/rationale)

- One branch per GitHub issue: `eharvey/#<n>`, landing as **exactly one
  commit** — `main` is rebase-only, so N branch commits become N commits on
  main. Enforced by `.githooks/pre-push` (install: `make hooks`) and the
  required `one-commit` check in `.github/workflows/pr-shape.yml`.
- Write the Plan/Test plan sections and `Closes #<n>` in the *commit message
  body*; `make ship` runs `gh pr create --fill`, which copies them into the PR.
  Don't close issues manually.
- `make ship` pushes and opens the PR, then stops. `make lgtm` enables
  auto-merge once you've read the diff — **that's the operator's call, not
  yours; never run it unless asked.**
- Core logic lives in `internal/<package>/`, decoupled from CLI/cobra
  concerns; `cmd/bootstrap/cmd/` is one file per subcommand, self-registered
  via `init()`.
- Prefer stdlib, or vendoring one small file, over importing a large module
  for one piece of logic (see `internal/cert`, `internal/third_party/incusos`).
- When a `Roadmap.md` checklist item is finished, check it off in
  `docs/Roadmap.md` in the *same* commit as the work, annotated with the
  closing issue number (it used to want its own commit; see #119).
- Generated artifacts (certs, keys, seeds, images) are gitignored under
  `bootstrap-output/` and `*.img` — never commit them.
- `//nolint:gosec` directives here suppress *real* findings (e.g. G304 file
  paths, G306 `0644` perms, G706 log injection on operator-supplied input),
  not noise — don't strip them assuming they're spurious; `make lint` is the
  arbiter.

## Worktrees (#252)

- **The main checkout (`/workspaces/infra`) is the operator's.** It often holds
  uncommitted work. Don't edit, switch branches or commit there; every issue
  gets its own worktree: `make wt N=<n>` for the main session (then work from
  the printed path), or `isolation: worktree` for `issue-worker`.
- **Run `make wt-gc` at the start of a session.** It removes worktrees, and
  their local branches, only when nothing can be lost (clean, and the PR merged
  at that commit, or no commits beyond main). It keeps and reports the rest.
- **gopls sees the main checkout, not your worktree.** The MCP server is rooted
  where the session started, so in a worktree it resolves imports to main's
  `internal/` (verified, #252). There, check with `go build ./...` / `go vet`
  and read the worktree's files; a session *started* in a worktree is fine.
- Hooks need no setup (`core.hooksPath` is relative). Validate scripts that
  bring up compose stacks share host ports, so only one worktree runs them at
  a time.

## Validating changes for real

Unit tests don't catch everything here — issue #5 shipped with a passing
test suite but a silently dropped seed file that only surfaced when
booting a real Incus VM. `scripts/validate/*.sh` each drive a real pipeline
end-to-end, proving a "done when" criterion `make test` can't — run the
relevant one before calling related work done. Two families:

- **Bootstrap/Phase-0** (e.g. `node-boots-and-trusts-bootstrap-cert.sh`):
  drive the real Incus remote `homelab-host`, sometimes booting a real VM off
  the produced `.img`. Override the target with `VALIDATE_INCUS_REMOTE` /
  `VALIDATE_INCUS_PROJECT` / `VALIDATE_INCUS_NETWORK` (#132), or
  `VALIDATE_INCUS_POOL` / `VALIDATE_ALPINE_CT` / `VALIDATE_ALPINE_VM` (#131).
  They launch from **pinned** base-image aliases, not `images:alpine/edge` —
  `.devcontainer/scripts/3-pin-validate-images.sh` creates them, and a moving
  upstream tag was a real source of silent drift (`docs/Decisions.md` §21).
- **Web app** (e.g. `sync-warns-on-config-diff.sh`,
  `background-poll-warns-on-config-diff.sh`): bring up the real
  `docker compose` stack (web + a throwaway git remote in `dev/git-fixture`)
  and assert against its HTTP API and logs — needs Docker.

Scripts are named for the **behaviour they prove**, not the issue that
prompted them (#138) — an issue number ages into meaninglessness, and the
originating issue is recorded in each file's header comment instead. The
names are meant to make the suite readable as a set: `sync-warns-on-config-diff`
and `background-poll-warns-on-config-diff` are visibly a pair proving the same
behaviour on two code paths.

`scripts/` itself holds only non-validation tooling — `lint-mermaid.sh`, `worktree.sh`,
`vendor-incusos.sh`, `ship.sh`, `lgtm.sh`, `auto-rebase.sh`.

They share one harness (`scripts/validate/lib.sh`, #140), so an unmet
prerequisite is a **SKIP with exit 3**, never a FAIL — "you didn't install a
tool" and "the thing under test is broken" are different outcomes. `--strict
--allow-skip <tag>` is how CI says "these must actually run": a route that
silently gains a precondition fails rather than skipping quietly. Ask the
scripts what they need rather than looking it up:

    ./scripts/validate/run.sh --describe

Full detail in `scripts/validate/README.md`.

## Subagents (`.claude/agents/`, #220)

Delegate work that reads a lot and concludes a little, so the bulk never enters
the main context:

- **`decisions-scout`**: before planning or implementing an issue, get the
  binding decisions, the issue's current scope, and the doc constraints as a
  ~2k-token brief, instead of reading `Decisions.md` or long issue bodies yourself.
- **`validate-runner`**: any validate script or `make validate*` run. It
  returns PASS/FAIL/SKIP per check and only the failing log lines.
- **`issue-worker`**: one issue end to end, in its own worktree, for parallel
  work. The handoff carries the *distilled* spec (a `decisions-scout` brief
  plus the exact `§N`/doc sections to read), not "read `docs/` in full". Every
  file you tell it to read, it pays for again.
- The built-in **`Explore`**: Incus/IncusOS source questions in the Go module
  cache (`$(go env GOMODCACHE)/github.com/lxc/incus/v7@…`). `gopls` only
  indexes packages this repo imports, so it can't see e.g. the Incus client.

Don't delegate design Q&A, rebases, or issue filing: they're a few calls
each, so a subagent's startup costs more than it saves.

## Skills (`.claude/skills/`, #222)

The recurring loops, so a session runs them rather than re-deriving them:

- **`/next`**: what to work on. One bounded report (`next.sh`) covers Ready
  issues and their blockers, dependency inconsistencies, the current phase's
  unchecked Roadmap items, open PRs and in-flight branches.
- **`/file-task`**: file an issue in `roadmap_task.yml`'s shape, with labels.
  Dependencies are native blocked-by links (`--blocked-by` / `--blocking`),
  which are all `/next` reads.
- **`/ship-issue`**: the pre-ship checklist, ending at `make ship`. It never
  runs `make lgtm`.
- **`/run-chain [<n> …]`** (#253, operator-invoked only): a root session works
  through the `Ready` queue (`queue.sh`: run / park / skip). Each issue gets
  a `decisions-scout` brief, an `issue-worker`, a Done-when audit and
  `make ship`. It never runs `make lgtm`. Workers' "Noticed" items are filed
  as **`proposed`** issues, which nothing picks up until the operator triages
  them to `Ready`.

They point at this file and `docs/Development Conventions.md` for the rules
rather than restating them. Change the rules there.
