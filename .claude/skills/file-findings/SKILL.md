---
name: file-findings
description: File the unfiled entries of the findings log (scripts/finding.sh) as `proposed` issues, never `Ready`. Drops findings an open issue already covers, marks every filed or duplicate record, and reports the ones too thin to file. Use for "file the findings", "/file-findings", "N unfiled findings", or step 5 of /run-chain.
---

# /file-findings

The findings log (`scripts/finding.sh`, #314) holds what sessions and
issue-workers noticed outside their issue. The `SubagentStop` hook records each
`issue-worker` `## Noticed` bullet there, and sessions record their own with
`scripts/finding.sh add`. This skill turns the unfiled ones into `proposed`
issues for the operator to triage. `/run-chain` § 5 runs exactly these steps.

## 1. Check GitHub, then read the log

    gh api rate_limit --jq .rate.remaining
    scripts/finding.sh list --unfiled --json

If the `gh` call fails, **stop**: report that GitHub is unreachable and that
the log is unchanged. Don't mark anything. If the list is empty, say so and
stop. It's bounded to the last 50 records; `finding.sh count --unfiled` tells
you whether there are more (`--limit` raises the bound).

Each record has `id`, `kind` (bug, drift or follow-up), `where` (`path:line`),
`text`, and `origin` (the issue it was noticed on, or null).

## 2. Drop duplicates

- **Within the log**: records describing the same problem (the same `where`,
  or the same defect worded differently) become one issue. File it once, then
  mark every one of those records with its number.
- **Against GitHub**: for each remaining record,
  `gh issue list --state open --search "<keywords from where and text>" --limit 5 --json number,title`.
  When an open issue already covers it, don't file:
  `scripts/finding.sh mark-filed <id> <that issue's number>`.

## 3. Hold back the thin ones

A record too thin to write an honest, checkable `### Done when` from (you
can't say what "fixed" looks like, or the `where` no longer exists) stays
unfiled. List it in the report as a candidate for the operator or
`/refine-issue`. Don't guess at a scope to make it fileable.

## 4. File the rest

Follow `/file-task` (`.claude/skills/file-task/SKILL.md`) for the template,
title, body sections and labels. It is the authority, so don't work from memory.
For a finding:

- `kind: bug` takes `bug_report.yml`'s shape and labels; `drift` and
  `follow-up` take the task shape with `task`.
- Always `proposed`; **never `Ready`**. Add `ai-optimization` for Claude Code
  tooling, as `/file-task` says.
- When the record has an `origin`, `### What needs to be built` (or the bug's
  description) starts with the exact sentence `Noticed while working on #<origin>.`
  (`/next` reads it), then gives the `where` and the `text`.
- `gh issue create --body-file <scratch file>`, as `/file-task` § Filing says.

Mark each record **as soon as its issue exists**, before filing the next, so
a failure part-way never files the same finding twice:

    scripts/finding.sh mark-filed <id> <new issue number>

If `gh` fails part-way, stop there. The records already filed are marked;
the rest stay unfiled for the next run.

## 5. Report

- Filed: each new issue's URL, its labels, and the record(s) it came from.
- Duplicates: each record and the open issue it was marked with.
- Left unfiled: each thin record (`id`, `where`, `text`) and what's missing.
- `scripts/finding.sh count --unfiled` at the end, which should equal the
  number left unfiled.
