---
name: next
description: What to work on next in this repo. Reports Ready issues with their blockers, the current Roadmap phase's unchecked items, open PRs with check status, and in-flight branches, in one bounded call, then recommends. Use for "what's next", "what should I work on", "what's ready", "status", "where are we".
---

# /next

Run the report once:

    .claude/skills/next/next.sh

It makes all the gh calls and applies all the filtering itself, and prints
under ~300 lines. Don't re-derive any part of it with your own `gh issue list`
or `gh pr list` calls. The script's header comment says where each section
comes from.

## Reading it

- **Blockers are native blocked-by links**, not body prose (#234 moved the
  prose onto links). `(paused)` marks a blocker the Roadmap lists as paused.
  A link can still go stale when a pivot like `docs/Decisions.md` §27 drops a
  dependency. If you're about to recommend an issue, or rule one out, because
  of a blocker that looks doubtful, read that issue's newest dated scope note:
  `gh issue view <n> --json body --jq .body | head -40`.
- **Dependency inconsistencies** are data faults to report, not to work
  around: a `Ready` issue with an open blocker, a blocker closed as not
  planned or duplicate, a cycle, or dependency prose with no matching link.
  The prose check is a heuristic. Name each hit and suggest the fix, either
  `gh issue edit <n> --add-blocked-by <m>` or rewording a sentence that isn't
  a dependency, and leave the edit to the operator.
- **Never recommend two issues together when one blocks the other** without
  saying which comes first. Check `blocked by` / `unblocks` across your picks.
- **In-flight work comes first.** An open PR with green checks and auto-merge
  off is waiting on the operator's `make lgtm`. Say so; never run it yourself.
  A PR with failing checks, or an open issue that has a local branch but no
  PR, is unfinished work to resume.
- **Proposed — awaiting triage** lists issues Claude filed on its own
  (`proposed`, #253), with their age and the issue they came from. They're
  the operator's to triage, not work to pick up. Mention how many are waiting,
  especially old ones, and never recommend implementing one.
- **`check off?`** on a Roadmap item means its issue is closed but the box is
  unchecked. That's drift worth mentioning.
- An open issue in the current phase that isn't `Ready` needs design first.
  Suggest a `decisions-scout` brief or a design discussion, not implementation.

## Answer

Give a short recommendation, not the report pasted back:

1. what's waiting on the operator (PRs to review / `make lgtm`, proposed
   issues to triage),
2. the top one to three things to pick up, in order, each with one line on why
   and on what it unblocks,
3. anything stale you noticed (Roadmap drift, dependency inconsistencies,
   blockers that look outdated).

Planning or implementing one of them is a separate step. Follow `CLAUDE.md`
§ Subagents for how to brief it.
