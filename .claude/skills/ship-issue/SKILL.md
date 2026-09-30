---
name: ship-issue
description: Pre-ship checklist for the current issue branch. Checks for one commit, a Plan/Test plan/Closes #N body and the Roadmap check-off, runs fmt/build/test/lint plus the matching validate script, then runs `make ship` and stops before `make lgtm`. Use for "ship it", "ship this", "open the PR", "ready to ship", "let's land this".
---

# /ship-issue

Take the current `eharvey/#<n>` branch to an open PR. `CLAUDE.md` §
Conventions and `docs/Development Conventions.md` § Branching & issues / § PR
body format / § Roadmap checklist convention hold the rules. This skill is the
order to check them in. Read those sections when a step fails; don't guess.

Stop at the first failing step and fix it (or report it) before going on.

1. **Branch.** `git branch --show-current` is `eharvey/#<n>`, not `main`. Take
   `<n>` from it. The working tree is clean, or its changes belong in this
   commit. **Use a git worktree**
2. **One commit.** `git fetch -q origin main && git rev-list --count FETCH_HEAD..HEAD`
   must print `1`. If it's more, squash with the recipe in Development
   Conventions § Branching & issues, keeping the fullest message.
3. **Message.** `git log -1 --format=%B`: a subject line, then `## Plan` (what
   and why, with the alternatives ruled out), `## Test plan` (`- [x]` items
   you actually ran, each one checkable), `Closes #<n>`, and the
   Co-Authored-By trailer this session gives you. `make ship` copies it into
   the PR verbatim.
4. **Roadmap.** `grep -n '#<n>\b' docs/Roadmap.md`. If this work finishes an
   item, it's checked off **in this commit** as `DONE; see #<n>`. If the issue
   isn't on the Roadmap, there's nothing to do.
5. **Checks.** `make fmt` must leave no diff. Then `make build`, `make test`,
   `make lint`, and `make lint-docs` if `docs/` changed. Hand `make lint` and
   anything longer to `validate-runner`. If the diff has no Go in it (docs or
   `.claude/` only), skip the Go steps and say so.
6. **Validate.** Get the script that proves the issue's "done when" from
   `./scripts/validate/run.sh --describe` (match on `proves:`) and run it
   through `validate-runner`. SKIP (exit 3) is not a pass: report what
   skipped and why. If no script proves it, say so. Don't claim coverage the
   suite doesn't have.
7. **Fold fixes in.** Anything steps 5–6 changed goes into the same commit with
   `git commit --amend`, and the Test plan is updated to match what actually
   ran.
8. **Ship.** `make ship`. It re-checks the branch, cleanliness and commit
   count, pushes, and opens the PR with `--fill`. Report the PR URL.

**Stop there.** `make lgtm` is the operator's call after reading the diff.
Don't run it, don't enable auto-merge another way, and don't offer to unless
asked.
