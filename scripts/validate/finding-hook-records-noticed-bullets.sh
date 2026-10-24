#! /bin/bash
# Validates GH issue #314: the findings log records what workers notice.
#
# Drives the real scripts against fixture input, never the real log:
#   1. .claude/hooks/finding-noticed.sh (SubagentStop) appends one record per
#      "## Noticed" bullet of an issue-worker hand-back: two bullets -> 2,
#      `none` -> 0, malformed -> 0 with a stderr note, exit 0 every time. A
#      repeated stop is idempotent, another agent type records nothing, and the
#      transcript is read when last_assistant_message is absent.
#   2. .claude/hooks/finding-count.sh (SessionStart) prints the count line only
#      while unfiled findings exist.
#   3. scripts/finding.sh add/list/count/mark-filed, its usage errors, and
#      parallel adds under the lock.
#   4. The default log location is shared across worktrees and survives a
#      worktree's removal, proven in a throwaway repo with its own worktree.
#
# What it can't prove: that Claude Code fires the hooks with this input. That
# is the live check in #314's Test plan (a real issue-worker stop, then the next
# session's count line).

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR" || exit 2
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="the SubagentStop hook records issue-worker Noticed bullets in the shared findings log (#314)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="jq, flock, git"
VALIDATE_DURATION="~3s"

validate_parse_args "$@"

require_cmd jq
require_cmd flock
require_cmd git
check_prereqs

FINDING="$ROOT_DIR/scripts/finding.sh"
NOTICED_HOOK="$ROOT_DIR/.claude/hooks/finding-noticed.sh"
COUNT_HOOK="$ROOT_DIR/.claude/hooks/finding-count.sh"

WORK_DIR="$(mktemp -d)"
cleanup() {
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# Every check below runs against a log in WORK_DIR, never the real one.
export FINDINGS_LOG="$WORK_DIR/findings.jsonl"

records() {
	if [ -s "$FINDINGS_LOG" ]; then wc -l <"$FINDINGS_LOG" | tr -d ' '; else echo 0; fi
}

# run_hook <agent_type> <message> [agent_id] — pipes SubagentStop input into
# the hook; sets HOOK_RC and HOOK_ERR.
run_hook() {
	local input
	input=$(jq -n --arg t "$1" --arg m "$2" --arg a "${3:-agent-1}" \
		'{session_id: "sess-1", hook_event_name: "SubagentStop", stop_hook_active: false,
		  agent_id: $a, agent_type: $t, last_assistant_message: $m}')
	HOOK_ERR=$("$NOTICED_HOOK" <<<"$input" 2>&1 >/dev/null)
	HOOK_RC=$?
}

# shellcheck disable=SC2016 # literal backticks, as a worker writes them
HANDBACK_TWO='## Hand-back #901
verdict: ready to ship
branch: eharvey/#901 @ abc1234   worktree: /tmp/wt
checks: fmt ok

## Noticed
- docs/Development Conventions.md:42 — the CI section still names the old
  workflow file (drift)
- `internal/config/config.go:10` — Parse ignores an empty file (bug)'

HANDBACK_NONE='## Hand-back #902
verdict: ready to ship
branch: eharvey/#902 @ abc1234   worktree: /tmp/wt

## Noticed
none'

HANDBACK_MALFORMED='## Hand-back #903
verdict: ready to ship
branch: eharvey/#903 @ abc1234   worktree: /tmp/wt

## Noticed
- just some prose with no location or kind'

# --- 1. SubagentStop hook ---------------------------------------------------

run_hook issue-worker "$HANDBACK_TWO"
check_eq "two Noticed bullets: hook exits 0" 0 "$HOOK_RC"
check_eq "two Noticed bullets: two records" 2 "$(records)"
check_json "records carry kind, where, text, origin, branch, session, filed_as=null" \
	"$(jq -sc '.' "$FINDINGS_LOG")" \
	'.[0].kind == "drift" and .[0].where == "docs/Development Conventions.md:42"
	 and .[0].text == "the CI section still names the old workflow file"
	 and .[1].kind == "bug" and .[1].where == "internal/config/config.go:10"
	 and .[1].text == "Parse ignores an empty file"
	 and all(.[]; .origin == 901 and .branch == "eharvey/#901" and .session == "agent-1"
	   and .filed_as == null and (.id | length) > 0 and (.ts | length) > 0)'

run_hook issue-worker "$HANDBACK_TWO"
check_eq "a repeated stop is idempotent: exit 0" 0 "$HOOK_RC"
check_eq "a repeated stop is idempotent: still two records" 2 "$(records)"

: >"$FINDINGS_LOG"
run_hook issue-worker "$HANDBACK_NONE"
check_eq "Noticed 'none': exit 0" 0 "$HOOK_RC"
check_eq "Noticed 'none': no records" 0 "$(records)"
check_eq "Noticed 'none': nothing on stderr" "" "$HOOK_ERR"

run_hook issue-worker "$HANDBACK_MALFORMED"
check_eq "malformed bullet: exit 0" 0 "$HOOK_RC"
check_eq "malformed bullet: no records" 0 "$(records)"
check "malformed bullet: stderr says why" grep -q 'skipped' <<<"$HOOK_ERR"

run_hook issue-worker '## Hand-back #904
verdict: not ready: tests fail'
check_eq "no Noticed section: exit 0" 0 "$HOOK_RC"
check_eq "no Noticed section: no records" 0 "$(records)"
check "no Noticed section: stderr says why" grep -q "no '## Noticed' section" <<<"$HOOK_ERR"

HOOK_ERR=$("$NOTICED_HOOK" <<<'not json {' 2>&1 >/dev/null)
HOOK_RC=$?
check_eq "non-JSON input: exit 0" 0 "$HOOK_RC"
check_eq "non-JSON input: no records" 0 "$(records)"

run_hook Explore "$HANDBACK_TWO"
check_eq "another agent type: exit 0" 0 "$HOOK_RC"
check_eq "another agent type: no records" 0 "$(records)"

# Without last_assistant_message, the last assistant message in the subagent's
# own transcript is the hand-back.
TRANSCRIPT="$WORK_DIR/agent.jsonl"
{
	jq -cn '{type: "user", message: {role: "user", content: "Implement #901"}}'
	jq -cn '{type: "assistant", message: {role: "assistant", content: [{type: "text", text: "working on it"}]}}'
	jq -cn --arg m "$HANDBACK_TWO" '{type: "assistant", message: {role: "assistant", content: [{type: "text", text: $m}]}}'
} >"$TRANSCRIPT"
HOOK_ERR=$(jq -n --arg p "$TRANSCRIPT" \
	'{session_id: "sess-1", hook_event_name: "SubagentStop", agent_id: "agent-2",
	  agent_type: "issue-worker", agent_transcript_path: $p}' | "$NOTICED_HOOK" 2>&1 >/dev/null)
HOOK_RC=$?
check_eq "transcript fallback: exit 0" 0 "$HOOK_RC"
check_eq "transcript fallback: two records" 2 "$(records)"

# --- 2. SessionStart hook ---------------------------------------------------

# shellcheck disable=SC2016 # literal backticks in the expected line
check_eq "SessionStart with 2 unfiled: the count line" \
	'2 unfiled findings; `/file-findings` to file them' "$("$COUNT_HOOK" </dev/null)"

for id in $(jq -r '.id' "$FINDINGS_LOG"); do "$FINDING" mark-filed "$id" 123 >/dev/null; done
check_eq "SessionStart with none unfiled: silent" "" "$("$COUNT_HOOK" </dev/null)"

: >"$FINDINGS_LOG"
check_eq "SessionStart with an empty log: silent" "" "$("$COUNT_HOOK" </dev/null)"
rm -f "$FINDINGS_LOG"
check_eq "SessionStart with no log at all: silent" "" "$("$COUNT_HOOK" </dev/null)"

# --- 3. finding.sh ----------------------------------------------------------

ID1=$("$FINDING" add --kind follow-up --where scripts/x.sh:3 --origin '#55' "first")
ID2=$("$FINDING" add --kind bug --where scripts/y.sh:9 --origin 56 "second")
check_eq "add prints an id and appends a record" 2 "$(records)"
check_eq "list: one line per finding" 2 "$("$FINDING" list | wc -l | tr -d ' ')"
check "list line shows id, kind, where, text, origin, state" \
	grep -qx "$ID1  follow-up  scripts/x.sh:3  first  from #55  unfiled" <<<"$("$FINDING" list)"
"$FINDING" mark-filed "$ID1" '#77' >/dev/null
check_json "mark-filed sets filed_as on that record only" "$(jq -sc '.' "$FINDINGS_LOG")" \
	'.[0].filed_as == 77 and .[1].filed_as == null'
check_eq "list --unfiled: only the unfiled one" "$ID2" "$("$FINDING" list --unfiled | awk '{print $1}')"
check_eq "count / count --unfiled" "2 1" "$("$FINDING" count) $("$FINDING" count --unfiled)"
check "list --limit bounds the output" test "$("$FINDING" list --limit 1 2>/dev/null | wc -l)" -eq 1

"$FINDING" add --kind nonsense --where a:1 "x" >/dev/null 2>&1
check_eq "add rejects an unknown kind" 1 "$?"
"$FINDING" add --kind bug "no where" >/dev/null 2>&1
check_eq "add requires --where" 1 "$?"
"$FINDING" mark-filed no-such-id 5 >/dev/null 2>&1
check_eq "mark-filed rejects an unknown id" 1 "$?"
"$FINDING" mark-filed "$ID2" notanumber >/dev/null 2>&1
check_eq "mark-filed rejects a non-number issue" 1 "$?"
"$FINDING" >/dev/null 2>&1
check_eq "no subcommand prints usage, exit 2" 2 "$?"
check_eq "rejected calls left the log alone" 2 "$(records)"

for i in $(seq 1 20); do "$FINDING" add --kind bug --where "p.go:$i" "parallel $i" >/dev/null & done
wait
check_eq "20 parallel adds: 22 records" 22 "$(records)"
check "20 parallel adds: every line is valid JSON" jq -e . "$FINDINGS_LOG"

# --- 4. One log across worktrees --------------------------------------------

unset FINDINGS_LOG
REPO="$WORK_DIR/repo"
mkdir -p "$REPO/scripts"
cp "$FINDING" "$REPO/scripts/finding.sh"
git -C "$REPO" init -q -b main
git -C "$REPO" add scripts/finding.sh
git -C "$REPO" -c user.name=v -c user.email=v@example.invalid commit -q -m init
git -C "$REPO" worktree add -q -b 'eharvey/#314' "$WORK_DIR/wt"
WT_ID=$(cd "$WORK_DIR/wt" && ./scripts/finding.sh add --kind drift --where a.md:1 "from the worktree")
check "add from a worktree writes to the shared git dir" test -s "$REPO/.git/claude-findings.jsonl"
check "the main checkout lists it" grep -q "^$WT_ID .*from #314" <<<"$(cd "$REPO" && ./scripts/finding.sh list)"
git -C "$REPO" worktree remove "$WORK_DIR/wt"
check "it survives the worktree's removal" grep -q "^$WT_ID " <<<"$(cd "$REPO" && ./scripts/finding.sh list)"

summary
