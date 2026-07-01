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

## Conventions (see docs/Development Conventions.md for full detail/rationale)

- One branch per GitHub issue: `eharvey/#<n>`. PRs use a Plan/Test plan
  body and `Closes #<n>` — don't close issues manually.
- Core logic lives in `internal/<package>/`, decoupled from CLI/cobra
  concerns; `cmd/bootstrap/cmd/` is one file per subcommand, self-registered
  via `init()`.
- Prefer stdlib, or vendoring one small file, over importing a large module
  for one piece of logic (see `internal/cert`, `internal/third_party/incusos`).
- When a `Roadmap.md` checklist item is finished, check it off in
  `docs/Roadmap.md` in its own commit, annotated with the closing issue
  number.
- Generated artifacts (certs, keys, seeds, images) are gitignored under
  `bootstrap-output/` and `*.img` — never commit them.
- `//nolint:gosec` directives here suppress *real* findings (e.g. G304 file
  paths, G306 `0644` perms, G706 log injection on operator-supplied input),
  not noise — don't strip them assuming they're spurious; `make lint` is the
  arbiter.

## Validating changes for real

Unit tests don't catch everything here — issue #5 shipped with a passing
test suite but a silently dropped seed file that only surfaced when
booting a real Incus VM. `scripts/validate-*.sh` each drive a real pipeline
end-to-end, proving a "done when" criterion `make test` can't — run the
relevant one before calling related work done. Two families:

- **Bootstrap/Phase-0** (e.g. `validate-issue-5.sh`): drive the real Incus
  remote `homelab-host`, sometimes booting a real VM off the produced `.img`.
- **Web app** (e.g. `validate-issue-22.sh`, `validate-config-sync-poll.sh`):
  bring up the real `docker compose` stack (web + a throwaway git remote in
  `dev/git-fixture`) and assert against its HTTP API and logs — needs Docker.

Not every script is named `validate-issue-N.sh`; a fix without its own issue
can take a descriptive name (`validate-config-sync-poll.sh`).
