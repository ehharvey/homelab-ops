# CLAUDE.md

Guidance for Claude Code working in this repo. Full context lives in
`docs/` — read before changing anything non-trivial:

- `docs/Architecture.md` — what this app is and how the pieces fit
- `docs/Roadmap.md` — current phase/status; check off items you complete
- `docs/Development Conventions.md` — branching, PR format, Go layout,
  vendoring rules — read this *before* writing code, not after
- `docs/Open Questions.md` — resolved design decisions with rationale

## Commands

    make build   # go build -o bin/bootstrap ./cmd/bootstrap
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

## Validating changes for real

Unit tests don't catch everything here — issue #5 shipped with a passing
test suite but a silently dropped seed file that only surfaced when
booting a real Incus VM. `scripts/validate-issue-N.sh` scripts each drive a
real pipeline (real Incus remote `homelab-host`, sometimes a real VM boot)
proving one issue's "done when" criteria — run the relevant one before
calling related work done, not just `make test`.
