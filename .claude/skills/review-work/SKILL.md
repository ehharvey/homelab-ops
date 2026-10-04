---
name: review-work
description: Repo-specific review of an open PR, an unshipped branch or a worktree before `make lgtm`. Runs the shared mechanical audit, matches each "Done when" item to evidence, checks Decisions conformance through decisions-scout and the repo conventions linters miss, and ends with a verdict and a list of gaps. Refuses a merged or closed PR. Never runs `make lgtm`. Operator-invoked only.
argument-hint: "<PR | branch | worktree path>"
disable-model-invocation: true
---

# /review-work

The operator's review before `make lgtm`. It answers one question: does this
work meet its issue and this repo's rules? The answer is **ready**, or a list
of specific gaps. Finding bugs in the code is `/code-review`'s job, not this
one's (step 5).

**Never run `make lgtm`**, `gh pr merge`, or anything else that merges or
enables auto-merge. Don't push, amend, or comment on the PR either: this skill
reports, and the operator acts.

Keep it bounded, as `CLAUDE.md` § Keeping context lean says: `--jq`, the
audit's `--stat`, and `git diff <base>...<head> -- <path>` for one file at a
time when a judgment needs it. Never the full diff of a large PR. Don't read
`docs/Decisions.md`, and don't run `make test`/`make lint` or validate scripts:
the evidence is what already ran.

## 1. Audit

    .claude/skills/review-work/audit.sh <target>

`<target>` is a PR number, a worktree path, or a branch (its header says how
it tells them apart). It prints the PR and base, the mechanical checks, the
facts the steps below need, the commit message, the `--stat`, the issue's
`### Done when` section and the Roadmap lines that name the issue.

- `REFUSED` (a merged or closed PR, or a branch that already merged): stop and
  report that. Review is for work that hasn't landed.
- Exit 2: the target doesn't exist. Say so and stop.
- Every `FAIL` line is a gap in the verdict, as written. Don't re-check what
  the audit checked.

## 2. Done when, against evidence

The evidence is the commit's `## Test plan`, the `--stat`, and CI:

    gh pr checks <pr> --json name,bucket --jq '.[] | "\(.bucket)\t\(.name)"'

A target with no PR has no CI checks. The verdict says so, and anything only
CI could show (lint, `examples/` passing) is "unverified", not passed.

Each `### Done when` item needs a `- [x]` Test plan line, or a file in the
`--stat`, that shows it is met. It's a gap when:

- no Test plan line or changed file backs it;
- the line that backs it is ticked but says SKIP, "not run", "left to CI",
  "should", or the like. A validate script's exit 3 is a SKIP, not a pass;
- a CI check it relies on is failing or still pending (pending: say so, it
  isn't ready yet).

## 3. Repo conventions the linters miss

Use the audit's facts section. Each item below is a gap when it fails.

- **Roadmap.** When the work finishes a `docs/Roadmap.md` item, it is checked
  off in this same commit as `DONE; see #<n>` (the audit prints the lines at
  the head, and whether `docs/Roadmap.md` changed). An issue that isn't on the
  Roadmap needs nothing.
- **Config schema.** When `internal/config/` changed, `docs/Config Schema.md`
  changed too, unless the change can't affect the format (say which). Whether
  `examples/` still pass is CI's answer from step 2; don't run `make test`.
- **Validate scripts.** When the change touches a real pipeline (`CLAUDE.md`
  § Validating changes for real), find the script whose `proves:` line covers
  it:

      ./scripts/validate/run.sh --describe | grep -E '^(name|proves):'

  The evidence is a Test plan line that names that script and its result
  (PASS). Only `make test`, a SKIP, or no line at all is a gap. When no script
  proves the behaviour, the Test plan should say so.
- **`//nolint:gosec`.** The audit lists removed directives. One that left
  with the code it covered, or moved, is fine. One stripped from code that is
  still there is a gap. Check with a scoped diff of that one file.
- **Generated artifacts.** No `bootstrap-output/` or `*.img` in the commit.
  The audit checks this; its `FAIL` line is the gap.

## 4. Decisions conformance

The audit prints the `§N` citations in the issue and in the changed files.
Not every `§N` is a `docs/Decisions.md` section: "/run-chain §3" is a skill's
section. When there are any, ask `decisions-scout` (one call, foreground):

```
Review conformance, don't brief. Issue #<n>; the change is <base>..<head>
(<branch>). Changed files: <from --stat>. For each of <§N list> that is a
docs/Decisions.md section, does the change follow it? Use scoped diffs
(git diff <base>...<head> -- <path>). Answer per section, one line each:
follows | departs (<file:line>, why) | not applicable.
```

A "departs" is a gap unless the commit's `## Plan` names that departure and
says why. With no citations, write "no decisions cited" in the verdict and
skip the call.

## 5. Verdict

End with this block and nothing after it:

```
## Review <target>: ready | not ready
PR: #<pr> (base <base>), checks <n pass, n fail, n pending> | none: not shipped, no CI checks
gaps:
- <specific gap: what is missing, and where>
decisions: <§N follows, …> | no decisions cited
next: run `/code-review high <target>` for bugs. Merging (`make lgtm`) is the operator's call.
```

**ready** means no gap at all. A branch with no PR can still be ready to ship,
but say that CI hasn't run. Always include the `/code-review high <target>`
line. Don't run `/code-review`, and don't hunt for bugs yourself: this review
covers the issue and the repo's rules, and that one covers the code.
