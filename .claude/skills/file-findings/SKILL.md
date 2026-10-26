---
name: file-findings
description: File the unfiled entries of the findings log (scripts/finding.sh) as `proposed` issues, never `Ready`. Drops findings an open issue already covers, comments a non-bug finding on its still-open, unshipped origin issue or PR instead of filing, holds non-bug findings while the `proposed` backlog is at its cap, marks every filed, commented or duplicate record, and reports the ones held or too thin to file. Use for "file the findings", "/file-findings", "N unfiled findings", or step 5 of /run-chain.
---

# /file-findings

The findings log (`scripts/finding.sh`, #314) holds what sessions and
issue-workers noticed outside their issue. The `SubagentStop` hook records each
`issue-worker` `## Noticed` bullet there, and sessions record their own with
`scripts/finding.sh add`. This skill turns the unfiled ones into `proposed`
issues for the operator to triage, and no more of them than that triage can
absorb (#410): a non-bug finding about still-open, unshipped work becomes a
comment on it, and past the backlog cap only bugs are filed. `/run-chain` § 5
runs exactly these steps.

## 1. Check GitHub, then read the log

    gh api rate_limit --jq .rate.remaining
    scripts/finding.sh list --unfiled --json

If the `gh` call fails, **stop**: report that GitHub is unreachable and that
the log is unchanged. Don't mark anything. If the list is empty, say so and
stop. It's bounded to the last 50 records; `finding.sh count --unfiled` tells
you whether there are more (`--limit` raises the bound).

Each record has `id`, `kind` (bug, drift or follow-up), `where` (`path:line`),
`text`, and `origin` (the issue it was noticed on, or null).

## 2. Merge duplicates within the log

Records describing the same problem (the same `where`, or the same defect
worded differently) become one issue or comment. Act on one id per group in
the steps below, then mark every record in the group with the same number.
This step is local. GitHub is searched only in step 6, for what will be filed.

## 3. Hold back the thin ones

A record too thin to write an honest, checkable `### Done when` from (you
can't say what "fixed" looks like, or the `where` no longer exists) stays
unfiled. That Done-when includes the proof item `/file-task`'s Ready criteria
require: a `scripts/validate/<name>.sh` or a `Real-run check:`. List a thin
record in the report as a candidate for the operator or `/refine-issue`.
Don't guess at a scope to make it fileable.

## 4. Plan: open origins and the backlog cap

If no ids are left, skip to the report. Otherwise pass them, one per group of
within-log duplicates (a group counts once against the cap):

    .claude/skills/file-findings/plan.sh <id> ...

Its header says how it decides; it changes nothing. With no ids it plans
nothing (`--all` plans every unfiled record, for a look at the whole log). It
prints the open `proposed` count against the cap (`FINDINGS_PROPOSED_CAP`,
default 15, an operator-changeable default), then one verdict per id:

- `comment #N (open issue|PR)`: not a bug, and its origin is an open PR or an
  open issue nothing has shipped for yet, so the finding belongs to that work.
- `file`: file it in step 7, unless step 6 finds an open issue covering it.
- `held: proposed backlog at N/cap`: not a bug, and the backlog is at or above
  the cap. It stays unfiled for a later run, once triage has brought the
  backlog down.

Bugs are always `file`: never held, never only a comment. A finding whose
origin has an open PR (a worker's own issue at `/run-chain` § 5) is planned
as if it had no origin, since its issue closes when that PR merges. An origin
that isn't an issue or PR here gets a warning and the same treatment. The
count is read once and goes up with every `file` verdict, so one run can't
overshoot the cap. If `plan.sh` fails, stop as step 1 says.

Mark each record **as soon as its comment or issue exists**, before the next,
so a failure part-way never posts or files the same finding twice. If `gh`
fails part-way through steps 5–7, stop there: what's done is marked, the rest
stays unfiled for the next run. Held records are left alone.

## 5. Comment on open origins

One comment per origin, covering every record planned for it (and their
within-log duplicates): a line per finding with its `kind`, `where` and
`text`, under `Noticed while working on #<origin>, outside its scope:`. The
issues endpoint takes a PR number too:

    gh api 'repos/{owner}/{repo}/issues/<origin>/comments' -F body=@<scratch file> --jq .html_url
    scripts/finding.sh mark-commented <id> <origin>

`finding.sh list --commented` lists these later, since nothing tracks a
comment once its issue or PR closes.

## 6. Drop what GitHub already covers

For each `file` verdict only:
`gh issue list --state open --search "<keywords from where and text>" --limit 5 --json number,title`.
When an open issue already covers it, don't file:
`scripts/finding.sh mark-filed <id> <that issue's number>`.

## 7. File

For the remaining `file` verdicts, follow `/file-task`
(`.claude/skills/file-task/SKILL.md`) for the template, title, body sections
and labels. It is the authority, so don't work from memory. For a finding:

- `kind: bug` takes `bug_report.yml`'s shape and labels; `drift` and
  `follow-up` take the task shape with `task`.
- Always `proposed`; **never `Ready`**. Add `ai-optimization` for Claude Code
  tooling, as `/file-task` says.
- When the record has an `origin`, `### What needs to be built` (or the bug's
  description) starts with the exact sentence `Noticed while working on #<origin>.`
  (`/next` reads it), then gives the `where` and the `text`.
- `gh issue create --body-file <scratch file>`, as `/file-task` § Filing says.

Then:

    scripts/finding.sh mark-filed <id> <new issue number>

## 8. Report

- The backlog line from `plan.sh` (`proposed backlog: N/cap`), and any
  origin warnings it printed.
- Filed: each new issue's URL, its labels, and the record(s) it came from.
- Commented: each comment's URL, the open issue or PR it went to, and the
  record(s) it covered.
- Duplicates: each record and the open issue it was marked with.
- Held: each record (`id`, `kind`, `where`, `text`) with
  `held: proposed backlog at N/cap`.
- Left unfiled: each thin record (`id`, `where`, `text`) and what's missing.
- `scripts/finding.sh count --unfiled` at the end, which should equal the
  held and thin records together.
