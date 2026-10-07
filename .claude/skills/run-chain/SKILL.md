---
name: run-chain
description: Work through a chain of Ready issues from one root session. Each issue gets a decisions-scout brief and an issue-worker in its own worktree, then a Done-when audit and `make ship`. Never `make lgtm`. Workers' "Noticed" items are filed as `proposed` issues for triage. Operator-invoked only.
argument-hint: "[<issue> ...]"
disable-model-invocation: true
---

# /run-chain

You are the **root** of a run. You implement nothing: you queue issues, pass
briefs to workers, audit what they hand back, ship, and summarise. That's
what lets one session cover several issues. Your context grows only by the
queue, briefs, bounded audit output and hand-back reports, never by docs or
code. This is the one sanctioned exception to `CLAUDE.md`'s one issue per
session. Subagents can't spawn subagents, which is why the root is a session
and not an agent.

**Never run `make lgtm`.** Don't run `gh pr merge` or enable auto-merge any
other way either. Every PR waits for the operator to read it. Running
`make ship` is authorized in chain mode, and only for a branch that passed the
audit.

## Rules for the root

- Don't read `docs/`, Go code, or full issue bodies, and don't run
  `make build/test/lint` yourself. If you need any of those to make a call,
  send the question back to the worker or park the issue.
- Bound every command (`--jq`, `sed -n`, `head`), as `CLAUDE.md` says.
- Keep a ledger at `<scratchpad>/run-chain.md`: one line per issue (state, PR,
  branch, worktree), plus a list of Noticed items and open questions. Update
  it as you go and write the summary from it, not from memory, since a long
  run may be compacted.
- Once a hand-back is in the ledger, don't quote it again. Only its
  essentials go forward: PR, branch, verdict, open questions, Noticed.

## 1. Queue

    make wt-gc                                  # once, at the start
    .claude/skills/run-chain/queue.sh [<n> ...] # run / park / skip, with reasons

No arguments takes every `Ready` issue. Arguments are filtered the same way,
so only `Ready` ones run. `queue.sh`'s header says what each verdict means. A
`proposed` issue is always skipped, even when it's also `Ready`. A parked
issue waits for its blocker's PR to merge, because stacked PRs (#254) don't
exist yet. Don't try to work around a park. Show the operator the queue in a
few lines, then start.

## 2. Brief, then hand off

For each `run` issue, in queue order, with **at most 2 workers at once** (3
only if the operator says so):

1. `decisions-scout` on the issue number. Its brief (~2k tokens) is the spec.
2. `issue-worker` (background, worktree-isolated) with this prompt:

   ```
   Implement #<n>. Chain mode: /run-chain is the root.
   Spec (decisions-scout brief):
   <brief>
   Read only the §N / doc sections the brief cites.
   Other workers run beside you: <#m, … | none>. Run non-`none`-group
   validate scripts under the shared lock (your agent file, § Verifying);
   `make lint` locks itself.
   Commit, but don't push or ship; the root ships after its audit.
   End with the hand-back block from your agent file, Noticed section included.
   ```

Note each worker's agent ID in the ledger; step 3 needs it to send work back.
When a worker finishes, start the next queued issue before auditing, so the
slot doesn't sit idle.

## 3. Audit "Done when"

Check the worker's claims against the branch, not against its prose. The
mechanical checks are the shared audit (#272), which `/review-work` and
`/ship-issue` run too:

    .claude/skills/review-work/audit.sh <wt>

It prints the issue's Done-when section, the commit message and `--stat`, and
a `FAIL` line per mechanical gap. It passes when all of these hold:

- `audit.sh` exits 0;
- every Done-when item maps to a `- [x]` line in the hand-back, and the files
  in `--stat` back that up;
- every `- [x]` in the commit's Test plan actually ran and passed according to
  the hand-back. A SKIP, "not run", or "left to CI" ticked as done doesn't count;
- the verdict is "ready to ship" and the open questions don't block shipping.

If it falls short, `SendMessage` the same worker **once** with the specific
gaps, then audit again. A second miss **parks** the issue, with the gaps as
the reason. An item that can't be met in a worktree at all (e.g. it needs the
operator) parks it straight away.

A hand-back with verdict `awaiting operator answers` (`/refine-issue` as a
subagent) isn't a miss: ask its open questions with `AskUserQuestion`, then
resume the same worker with the answers via `SendMessage`.

## 4. Ship

    make -C <wt> ship

Put the PR number from its last line in the ledger. That's the whole of
shipping. Leave the worktree in place: `make wt-gc` removes it once the PR
merges.

## 5. File Noticed items as `proposed`

After the last worker hands back, take the Noticed items from every worker
together:

- Drop duplicates across workers, and any that an open issue already covers:
  `gh issue list --state open --search "<keywords>" --limit 5 --json number,title`.
- File the rest with `/file-task`'s shape and labels, adding `proposed`, and
  **never `Ready`**. The body says `Noticed while working on #<n>.` (that's
  what `/next` reads) and gives the `path:line`.
- An item too thin to fill in "Done when" honestly goes under open questions
  in the summary instead, as a candidate for `/refine-issue`.

## 6. Run summary

End with a single report:

- **PRs**, in merge order, each with its check status
  (`gh pr checks <pr> --json bucket --jq '[.[].bucket] | group_by(.) | map("\(length) \(.[0])") | join(", ")'`),
  its base (`main` until #254), and its issue. Merging is the operator's
  `make lgtm`.
- **Parked**, each with its reason.
- **Proposed issues filed**, each with its origin issue.
- **Skips**: queue skips, plus validate checks that SKIPped inside workers.
- **Open questions** from the workers.

Then tell the operator to start a fresh session for anything else, as
`CLAUDE.md` asks.
