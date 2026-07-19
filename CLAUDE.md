# CLAUDE.md

Guidance for Claude Code working in this repo. Full context lives in
`docs/` — read before changing anything non-trivial:

- `docs/Architecture.md` — what this app is and how the pieces fit
- `docs/Roadmap.md` — current phase/status; check off items you complete
- `docs/Development Conventions.md` — branching, PR format, Go layout,
  vendoring rules — read this *before* writing code, not after
- `docs/Decisions.md` — resolved design decisions with rationale

## Commands

    make build   # builds both binaries: bin/bootstrap (CLI) and bin/web (web app)
    make test    # go test ./... -race -cover
    make lint    # golangci-lint via Docker — slow; run before declaring done
    make fmt     # gofmt + goimports

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

`scripts/` itself holds only non-validation tooling — `lint-mermaid.sh`,
`vendor-incusos.sh`, `ship.sh`, `lgtm.sh`.

They share one harness (`scripts/validate/lib.sh`, #140), so an unmet
prerequisite is a **SKIP with exit 3**, never a FAIL — "you didn't install a
tool" and "the thing under test is broken" are different outcomes. `--strict
--allow-skip <tag>` is how CI says "these must actually run": a route that
silently gains a precondition fails rather than skipping quietly. Ask the
scripts what they need rather than looking it up:

    ./scripts/validate/run.sh --describe

Full detail in `scripts/validate/README.md`.
