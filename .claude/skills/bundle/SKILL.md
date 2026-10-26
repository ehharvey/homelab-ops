---
name: bundle
description: Group open `small` issues in the same area into one unit of work. Lists the ones not in flight, proposes groups of 2–5 for the operator to confirm, then files a parent `[Task]: Bundle:` issue per confirmed group via /file-task with the children attached as native sub-issues. Never files before the operator confirms, never adds `Ready`. Use for "bundle the small issues", "/bundle", "group the small ones".
---

# /bundle

Every issue pays a roughly fixed pipeline cost (scout, worker, lint, review,
ship), so a 10-line fix costs about what a 100-line one does (#409). A bundle
pays it once for several `small` issues in one area. `small`, what a bundle
is and how it ships are `docs/Development Conventions.md` § Sizing and
bundles; this skill is the order to apply them in.

**File nothing until the operator confirms the groups.** **Never add `Ready`**,
to the parent or anyone else.

## 1. List

    .claude/skills/bundle/bundle.sh list

It prints the open `small` issues that are free to bundle, each with its
`Ready`/`proposed` status, labels, `### Doc references` paths and any decision
it cites, then the ones it left out (in flight, already bundled, an umbrella's
sub-issue, a parent) and why. Its header says how. Don't re-derive it with your own `gh` calls.

## 2. Propose

Group by area: the shared paths in `refs:` first, then the title's subject
(skills, validate, devcontainer, CI, docs…). A group is 2–5 issues a single
worker could do in one sitting. Leave an issue out rather than force it into a
group; one left on its own still ships alone.

Show each proposed group in a few lines: its issues, the shared area, and
whether the whole still fits `small`. Ask the operator to confirm, edit or
drop each group, with `AskUserQuestion` when you can. As a subagent, hand back
with verdict `awaiting operator answers` and the groups as the open question.

## 3. File the parent

For each confirmed group, get what the parent's body needs:

    .claude/skills/bundle/bundle.sh show <n> <n> ...

and file it through `/file-task` (`.claude/skills/file-task/SKILL.md`), with
these specifics:

- **Title:** `[Task]: Bundle: <area summary>`. The `Bundle:` prefix is what
  `queue.sh` and `bundle.sh` recognise: a bundle's children don't run on
  their own, while an umbrella's sub-issues still do.
- **What needs to be built:** one line per child (`#<n>: <what>`), then: "Ships
  as one branch and one commit, `eharvey/#<parent>`, closing every child
  (`docs/Development Conventions.md` § Sizing and bundles)."
- **Done when:** the union of the children's items, each prefixed with its
  child (`(#<n>)`), unchanged otherwise.
- **Doc references:** the union of the children's, including every `§N` or
  `docs/Decisions.md` mention. They decide the bundle's `/run-chain` lane.
- **Labels:** `task`, plus the children's area labels (`ai-optimization`,
  `documentation`, `phase-<n>`). `small` only if the bundle still fits the
  definition. Never `Ready`, never `proposed` (the operator asked for it).
- **Links:** none of its own; the children keep theirs, and their labels.
  `queue.sh` counts the children's blockers as the parent's.

## 4. Attach the children

    .claude/skills/bundle/bundle.sh attach <parent> <n> <n> ...

It checks every child first and attaches none if one already has a parent
other than a closed bundle. It then adds each as a native sub-issue (the
script resolves the numeric id the endpoint wants) and lists the parent's
sub-issues. Every child must appear there; it exits 1 if a POST failed.

## 5. Report

Each parent's URL, its children, its labels, and whether `small` stayed on. For
`Ready`, say whether every child is `Ready`: if so, the operator may add it to
the parent; if not, name the ones that aren't. Then list what you left
unbundled. Revert a parent with
`gh issue close <parent> --reason "not planned"`: a closed parent no longer
holds its children back, and a later `attach` moves them out of it.
