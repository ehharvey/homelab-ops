---
name: resume
description: Pick up in-flight work on one issue or PR in a fresh session. Resolves the issue, its PR, branch and worktree (agent worktrees included), and reports their state, the latest `/review-work` verdict with the comments after it, and the mechanical gaps from audit.sh, in one bounded call; then recommends the next step and the worktree to work from. Read-only. Operator-invoked only.
argument-hint: "<issue | PR number>"
disable-model-invocation: true
---

# /resume

Where does this one piece of work stand, and what's next? `/next` is the
repo-wide view; this is the drill-down on one target.

**Read-only.** Don't commit, push, comment, label, or create or remove a
worktree. This skill reports and recommends; the next step is a separate one.

## 1. Report

Run it once:

    .claude/skills/resume/resume.sh <issue | PR number>

It makes all the gh and git calls itself (its header comment says how it
resolves each one) and fits one screen. Don't re-derive any part of it, and
don't re-check what its audit section shows: that is
`.claude/skills/review-work/audit.sh`'s own output, trimmed.

- Exit 2: a usage error, or no such issue or PR. Say so and stop.
- **not started**: there's no branch and no PR. Point at `make wt N=<n>` and
  a `decisions-scout` brief (`CLAUDE.md` § Subagents), or `/next` for the
  wider queue. If the issue has open blockers, say that first.
- **already landed** or **closed unmerged**: nothing to resume. Mention a
  leftover worktree; `make wt-gc` removes it.

## 2. Recommend

Give a short answer, not the report pasted back:

1. **Where to work**: the worktree path from the report, or `make wt N=<n>`
   when the branch has no worktree. gopls sees the main checkout, not a
   worktree (`CLAUDE.md` § Worktrees). A dirty worktree may hold someone's
   unfinished work: say so before suggesting anything that touches it.
2. **The open gaps, in order**:
   - an open blocker on the issue;
   - each `FAIL` line from the audit, as written;
   - the review's gap lines, minus any that a later comment says is resolved
     (read the comments after it; name the comment);
   - failing or pending checks;
   - `Done when` items that the Test plan doesn't yet back (judge them the way
     `/review-work` § 2 does, without re-running anything).
3. **The skill for the next step**:
   - no PR yet: finish the gaps, then `/ship-issue`;
   - review threads or failing CI on an open PR: `/address-feedback <pr>`;
   - an open PR with no gaps left and no review: the operator's
     `/review-work <pr>`. Merging (`make lgtm`) is the operator's call; never
     run it or suggest you will.

A `decisions-scout` brief helps when the remaining work needs design context
the report doesn't carry. Suggest one in that case; don't run it by default.
