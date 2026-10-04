---
name: refine-issue
description: Flesh out an existing, under-specified GitHub issue toward Ready. Classifies it, finds its constraints and blockers, asks the operator the open design questions, rewrites the body in its template's shape, fixes labels and links, and reports each change with its revert command and a Ready verdict. Use for "refine issue #n", "flesh out #n", "fill in #n", "groom #n", "make #n ready".
argument-hint: "<issue number>"
---

# /refine-issue <n>

`/file-task` shapes a *new* issue; this takes an existing stub to implementable
(#274). It adds no rules of its own. Body shape, labels and the Ready criteria
are `/file-task`'s (`.claude/skills/file-task/SKILL.md`), context rules are
`CLAUDE.md` § Keeping context lean and current, and rewriting a body is
`docs/Development Conventions.md` § Supersede in place.

Two things are the operator's, never yours: **never add `Ready`** to the issue
you refine and **never remove `proposed`**. And **never invent an answer** to
a design question.

## 1. Read and classify

Save the old body first; it's the body's revert (step 8).

    gh issue view <n> --json title,labels,body --jq '.title, ([.labels[].name]|join(",")), .body' | head -150
    gh issue view <n> --json body --jq .body > <scratch>/<n>-body.orig.md
    gh issue view <n> --json comments --jq '.comments[] | "\(.createdAt[:10]) \(.author.login): \(.body[:800])"' | tail -60
    gh api graphql -F owner='{owner}' -F repo='{repo}' -F n=<n> -f query='query($owner:String!,$repo:String!,$n:Int!){repository(owner:$owner,name:$repo){issue(number:$n){blockedBy(first:20){nodes{number state}} blocking(first:20){nodes{number state}}}}}' --jq '.data.repository.issue'

Read the comments: a prerequisite is often settled there and nowhere else.
Classify it as a task (`roadmap_task.yml`), a decision (`design_decision.yml`),
a bug (`bug_report.yml`), or an umbrella of separately shippable tasks. Retitle
with the template's prefix (`[Task]: `, `[Decision]: `, `[Bug]: `) if it lacks
one; GitHub's timeline keeps the old title.

## 2. Constraints

- `decisions-scout` on the issue for the binding `§N` and doc sections. Don't
  read `docs/Decisions.md` yourself.
- Go code: the gopls MCP tools (`go_search`, `go_symbol_references`). Grep only
  YAML, workflows, Markdown and string literals.
- Incus/IncusOS behaviour: the built-in `Explore` agent in the module cache.

Name what the change touches (paths, symbols, workflow files) in the body.

## 3. Dependencies

- **In the repo:** `gh issue list --state open --search "<keywords>" --limit 10 --json number,title`,
  then native links: `gh issue edit <n> --add-blocked-by <m>` / `--add-blocking <m>`.
- **Outside it** (another repo, a service, an operator action): a link can't
  express these, so they go in `### What needs to be built` as a
  `**Prerequisites:**` `- [ ]` list. Check off the satisfied ones, citing the
  evidence (the comment's date and URL, or a command and its output).

## 4. Design questions

Ask the operator every question whose answer changes what gets built, with the
options you found and their tradeoffs. A question too big to answer inline
becomes a `design_decision.yml`-shaped issue via `/file-task`, linked with
`--add-blocked-by`.

## 5. Umbrellas

Propose the split (one child per shippable piece, in order) and ask before
filing. File the children via `/file-task`, ordered with `--blocked-by`, and
list them in the umbrella's body. (#236 moves this to native sub-issues.)

## 6. Rewrite

The matching template's sections, per `/file-task` § Body, written to a
scratch file: `gh issue edit <n> --title "<title>" --body-file <file>`. The
body states the current spec. Don't post the old body as a comment; the edit
history keeps it. Post a short dated note only when the *scope* changed, not
for a reshape.

## 7. Labels

`/file-task` § Labels and milestone, which also says what the issues you file
get. Add the classification's label, drop the ones it contradicts
(`enhancement` once it's a `task`), and keep the rest (`later`, `phase-<n>`).

## 8. Report

- Every change with the command that reverts it: title
  (`gh issue edit <n> --title "<old>"`), body (`--body-file <scratch>/<n>-body.orig.md`),
  labels (`--add-label` / `--remove-label`), links (`--remove-blocked-by <m>` / `--remove-blocking <m>`),
  issues filed (`gh issue close <m> --reason "not planned"`), and a comment
  (`gh issue comment <n> --delete-last --yes`).
- The questions answered, with their answers, and those still open.
- The Ready verdict, criterion by criterion against the Ready criteria in
  `/file-task` § Labels and milestone. Adding `Ready` stays the operator's call.
- If the issue belongs on `docs/Roadmap.md` and isn't there, say so. Don't
  edit the Roadmap here.

## As a subagent

Under `/run-chain`, you can't ask the operator or spawn `decisions-scout`:

- The handoff carries the scout brief; it is step 2.
- Do steps 1–5 read-only, planning the links and children without setting
  or filing them, and **stop before any edit**. Hand back with verdict
  `awaiting operator answers`, the questions under `open questions`, and the
  exact planned title, body, labels and links.
- The root asks them with `AskUserQuestion` and resumes you via `SendMessage`
  with the answers. Then apply steps 3 and 5–7 and hand back the step 8 report.
