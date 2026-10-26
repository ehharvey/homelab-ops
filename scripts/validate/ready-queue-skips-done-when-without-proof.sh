#! /bin/bash
# Validates GH issue #410: a Ready issue whose Done-when names neither a
# scripts/validate/ script nor a real-run check is flagged by /next and
# skipped by /run-chain, through one shared check.
#
# Drives the real code against fixtures, never GitHub:
#   1. .claude/skills/file-task/done-when.jq on issue bodies: the task form
#      (`### Done when`), the bug form (a `Done when:` line), continuation
#      lines, plain, numbered and prose items, a fenced `# comment`, the
#      "Real-run check" spellings, and the near misses (a script named outside
#      Done-when, the phrase mid-item, a proof delegated to another issue, no
#      section at all).
#   2. .claude/skills/run-chain/queue.sh, with a stub `gh` serving a fixture
#      issue list, gives the bad issue `skip` with the exact reason, names an
#      open blocker in that reason, and runs the good one.
#   3. .claude/skills/next/next.sh, against the same stub, lists the bad issue
#      under "Dependency inconsistencies" and not the good one.
# Both scripts run in a throwaway repo holding copies of themselves and the
# shared check, so neither touches this checkout's branches or its remote.
#
# What it can't prove: how the live Ready queue reads. That is #410's real-run
# check (run next.sh and queue.sh as they are, and record what they flag).

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR" || exit 2
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="/next flags and /run-chain skips a Ready issue whose Done-when names no validate script or real-run check (#410)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="jq, git"
VALIDATE_DURATION="~3s"

validate_parse_args "$@"

require_cmd jq
require_cmd git
check_prereqs

WORK_DIR="$(mktemp -d)"
cleanup() {
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# --- 1. The shared check ------------------------------------------------------

# ok <body> — prints the check's verdict on one body: true or false.
ok() {
	jq -n -r -L "$ROOT_DIR/.claude/skills" --arg b "$1" 'include "file-task/done-when"; $b | done_when_ok'
}

check_eq "task form: an item names scripts/validate/<name>.sh" true \
	"$(ok $'### Done when\n\n- [ ] It works.\n- [ ] `scripts/validate/foo-does-bar.sh` passes.\n\n### Doc references\n')"
check_eq "task form: an item starts 'Real-run check:'" true \
	"$(ok $'### Done when\n\n- [ ] Real-run check: run it and paste the output.\n')"
check_eq "task form: bold '**Real-run check:**'" true \
	"$(ok $'### Done when\n\n- [ ] **Real-run check:** run it.\n')"
check_eq "task form: 'Real-run check (no pipeline):' (pre-#410 spelling)" true \
	"$(ok $'### Done when\n\n- [x] Real-run check (no pipeline): run it.\n')"
check_eq "task form: the script is named on a continuation line" true \
	"$(ok $'### Done when\n\n- [ ] It works, proven by\n  `./scripts/validate/new-one.sh`, which this issue adds.\n')"
check_eq "bug form: a 'Done when:' line inside Expected behavior" true \
	"$(ok $'### Expected behavior\n\nIt works.\n\nDone when:\n\n- [ ] Real-run check: reload and see.\n\n### Steps to reproduce\n')"
check_eq "a plain bullet names the script" true \
	"$(ok $'### Done when\n\n- `scripts/validate/a.sh` passes.\n')"
check_eq "a numbered item starts 'Real-run check:'" true \
	"$(ok $'### Done when\n\n1. Unit tests pass.\n2. Real-run check: run it.\n')"
check_eq "a prose paragraph under the heading names the script" true \
	"$(ok $'### Done when\n\nIt is proven by\n`scripts/validate/a.sh`.\n')"
check_eq "a fenced '# comment' doesn't end the section" true \
	"$(ok $'### Done when\n\n- [ ] It runs:\n\n```sh\n# set up first\nmake up\n```\n\n- [ ] `scripts/validate/a.sh` passes.\n')"
check_eq "no qualifying item" false \
	"$(ok $'### Done when\n\n- [ ] Unit tests pass.\n- [ ] make lint passes.\n')"
check_eq "a script named only outside Done-when" false \
	"$(ok $'### What needs to be built\n\n- [ ] add scripts/validate/x.sh\n\n### Done when\n\n- [ ] Unit tests pass.\n')"
check_eq "a proof delegated to another issue's script" false \
	"$(ok $'### Done when\n\n- [ ] Unit tests pass.\n- [ ] Proven for real by #184\'s 3-VM script. This issue ships on its unit tests.\n')"
check_eq "the phrase mid-item, not leading it" false \
	"$(ok $'### Done when\n\n- [ ] Tests pass; a real-run check: later.\n')"
check_eq "bug form: a list after the Done-when paragraph doesn't count" false \
	"$(ok $'Done when:\n\n- [ ] It works.\n\nLater notes:\n\n- [ ] scripts/validate/y.sh\n')"
check_eq "no Done-when section" false "$(ok $'### What needs to be built\n\nStuff.\n')"
check_eq "empty body" false "$(ok '')"
check_eq "null body" false \
	"$(jq -n -r -L "$ROOT_DIR/.claude/skills" 'include "file-task/done-when"; null | done_when_ok')"

# --- 2 and 3. queue.sh and next.sh against a stub gh -------------------------

REPO="$WORK_DIR/repo"
mkdir -p "$REPO/.claude/skills/file-task" "$REPO/.claude/skills/next" \
	"$REPO/.claude/skills/run-chain" "$REPO/docs"
cp .claude/skills/lib.jq "$REPO/.claude/skills/"
cp .claude/skills/file-task/done-when.jq "$REPO/.claude/skills/file-task/"
cp .claude/skills/next/next.sh "$REPO/.claude/skills/next/"
cp .claude/skills/run-chain/queue.sh "$REPO/.claude/skills/run-chain/"
printf '# Roadmap\n\n## Phase 1: Fixture\n\n- [ ] an item\n' >"$REPO/docs/Roadmap.md"
git -C "$REPO" init -q -b main

# Four open Ready issues: #1 proves itself, #2 doesn't, #3 doesn't but is also
# `proposed` (that reason wins, as queue.sh's header says), #4 doesn't and is
# blocked by #1 (the reason names the blocker too).
GOOD=$'### Done when\n\n- [ ] Real-run check: run it.\n'
BAD=$'### Done when\n\n- [ ] Unit tests pass.\n'
jq -n --arg good "$GOOD" --arg bad "$BAD" '
  def issue($n; $title; $body; $labels; $blockers): {number: $n, title: $title, body: $body,
    createdAt: "2026-01-01T00:00:00Z", labels: {nodes: ($labels | map({name: .}))},
    blockedBy: {nodes: ($blockers | map({number: ., state: "OPEN"}))}, blocking: {nodes: []}};
  def issue($n; $title; $body; $labels): issue($n; $title; $body; $labels; []);
  {data: {repository: {issues: {pageInfo: {hasNextPage: false, endCursor: null}, nodes: [
    issue(1; "[Task]: good"; $good; ["task", "Ready"]),
    issue(2; "[Task]: bad"; $bad; ["task", "Ready"]),
    issue(3; "[Task]: bad and proposed"; $bad; ["task", "Ready", "proposed"]),
    issue(4; "[Task]: bad and blocked"; $bad; ["task", "Ready"]; [1])]}}}}' \
	>"$WORK_DIR/graphql.json"

STUB="$WORK_DIR/stub"
mkdir -p "$STUB"
cat >"$STUB/gh" <<EOF
#!/usr/bin/env bash
case "\$1 \$2" in
"repo view") echo fixture/repo ;;
"api graphql") cat "$WORK_DIR/graphql.json" ;;
"issue list" | "pr list") echo '[]' ;;
*) echo "stub gh: unexpected \$*" >&2; exit 9 ;;
esac
EOF
chmod +x "$STUB/gh"

QUEUE=$(cd "$REPO" && PATH="$STUB:$PATH" ./.claude/skills/run-chain/queue.sh 2>&1)
check_eq "queue.sh exits 0" 0 "$?"
check "queue.sh runs the issue whose Done-when has a real-run check" \
	grep -qx '#1  good' <<<"$(sed -n '/== Run/,/^$/p' <<<"$QUEUE")"
check "queue.sh skips the one without, with the exact reason" \
	grep -qx '      Done-when names no validate script or real-run check' \
	<<<"$(sed -n '/^#2  bad$/,/^#/p' <<<"$QUEUE")"
check "queue.sh names the open blocker of an issue it skips for its Done-when" \
	grep -qx '      Done-when names no validate script or real-run check; also blocked by #1' \
	<<<"$(sed -n '/^#4  bad and blocked$/,/^#/p' <<<"$QUEUE")"
check "queue.sh still gives a proposed issue the proposed reason" \
	grep -q 'proposed: awaiting' <<<"$(sed -n '/^#3 /,+1p' <<<"$QUEUE")"

NEXT=$(cd "$REPO" && PATH="$STUB:$PATH" ./.claude/skills/next/next.sh 2>&1)
check_eq "next.sh exits 0" 0 "$?"
INCONS=$(sed -n '/== Dependency inconsistencies ==/,/^$/p' <<<"$NEXT")
check "next.sh flags #2 under inconsistencies" \
	grep -q '^#2 is Ready but Done-when names no validate script or real-run check' <<<"$INCONS"
check_eq "next.sh doesn't flag #1" 0 "$(grep -c '^#1 ' <<<"$INCONS")"

summary
