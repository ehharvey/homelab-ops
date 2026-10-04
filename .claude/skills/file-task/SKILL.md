---
name: file-task
description: File a GitHub issue in this repo's Roadmap-task shape (Roadmap phase / What needs to be built / Done when / Doc references) with the right labels. Use for "file an issue", "file a task", "open an issue for this", "track this as an issue", "make a ticket".
argument-hint: "[what the task is]"
---

# /file-task

File one issue shaped like `.github/ISSUE_TEMPLATE/roadmap_task.yml`, the form
you'd get from the web UI. Read that template for the sections and the title
prefix rather than working from memory. If the item is really an unresolved
design question, use `design_decision.yml`'s shape instead, since the task
template itself says so. Use `bug_report.yml` for a defect.

## Before drafting

- **Duplicates:** `gh issue list --state all --search "<keywords>" --limit 10 --json number,title,state`.
  If one already covers it, say so and stop, or offer to comment on it instead.
- **Phase:** take the phase name from `docs/Roadmap.md`'s `## ` headings
  (`grep -n '^## ' docs/Roadmap.md`). The template's dropdown and the GitHub
  milestones both lag the Roadmap's renumbering. Tooling that isn't on the
  Roadmap says so in that section (e.g. "Not on the Roadmap. Claude Code
  tooling.").
- **Decisions:** if the task touches a settled design area, cite the binding
  `docs/Decisions.md` §N. Ask `decisions-scout` when you don't already know
  which sections apply. Don't read Decisions.md whole.

## Body

The four sections as `### ` headings, in the template's order:

- `### Roadmap phase`: the heading text, plus the tracking issue if there is one.
- `### What needs to be built`: scoped to one checklist item or a small group.
  Dependencies are native links, set when filing (below), not prose. A
  one-line "why" beside one is fine (e.g. "Needs #A's renderer registry."),
  but the link is the record. `/next` reads only links, and flags dependency
  prose that has none.
- `### Done when`: concrete, checkable `- [ ]` items. Where a real pipeline is
  involved, one of them names the `scripts/validate/` behaviour that proves it
  (see `CLAUDE.md` § Validating changes for real).
- `### Doc references`: `docs/<File>.md § Section` / `§N`, plus code paths.

Write the "why" into the body. The implementer has only this text and the docs.

## Labels and milestone

- Always `task`.
- `phase-<n>` when the phase has one (`gh label list --search phase`).
- `ai-optimization` for Claude Code tooling and context work.
  `documentation` when the deliverable is docs.
- `Ready` **only** when the design is settled and nothing needs planning
  before implementation. If you aren't sure, leave it off and say so.
- `proposed` when the issue is **your** idea, not the operator's: a worker's
  "Noticed" item in `/run-chain`, or something you spotted and chose to file.
  A `proposed` issue never gets `Ready` from you. The operator triages it,
  either adding `Ready` and removing `proposed`, or closing it. `/next` and
  `/run-chain` pick up only `Ready` issues, which is what makes triage the
  gate. Leave `proposed` off when the operator asked for the issue directly.
  Put `Noticed while working on #<n>.` in a proposed issue's
  `### What needs to be built` when it came out of another issue's work.
  `/next` shows that origin.
- Milestone: only when an open milestone's title matches the phase exactly
  (`gh api 'repos/{owner}/{repo}/milestones?state=open' --jq '.[].title'`).
  If none matches, skip it and say which one was missing.

## Filing

Write the body to a scratch file and run:

    gh issue create --title "[Task]: <imperative summary>" --body-file <file> --label task --label ... \
      --blocked-by <A>,<B> --blocking <C>

`--blocked-by` lists the issues this one can't start before; `--blocking`
lists existing issues that wait on this one. Leave out whichever is empty.

Report the issue URL, the labels and links you set, and anything you left for the
operator to decide, such as `Ready` or a missing milestone. Don't add an
unchecked `docs/Roadmap.md` item here. That's a doc change and goes through a
commit like any other.
