#! /bin/bash
# Validates GH issue #410: /file-findings files fewer issues. A non-bug finding
# whose origin is an open PR, or an open issue nothing has shipped for, becomes
# a comment there, and while open `proposed` issues are at or above
# FINDINGS_PROPOSED_CAP only bugs are filed.
#
# Drives the real .claude/skills/file-findings/plan.sh (the throttle the skill
# runs before filing) against a fixture findings log and a stub `gh`, never
# GitHub or the real log:
#   1. Backlog 18, default cap 15: non-bugs with an open origin (issue and PR)
#      get `comment`; bugs get `file`, even with an open origin; a non-bug
#      whose origin issue has an open PR (a worker's shipped issue), or whose
#      origin is no issue or PR (warned), is planned as no origin; the rest get
#      `held: proposed backlog at N/15`.
#   2. Backlog 13: the count goes up with each `file`, so non-bugs are filed
#      only until the cap is reached, never past it.
#   3. FINDINGS_PROPOSED_CAP raises the cap; a bad value is refused.
#   4. Ids restrict the plan; no ids plans nothing; --all plans everything and
#      takes no ids; an unknown id is refused; either gh read failing stops it
#      with no verdicts; it never writes the log.
#   5. scripts/finding.sh mark-commented records the PR a comment went to apart
#      from filed_as; list shows it and --commented re-surfaces it.
#
# What it can't prove: that a session follows the skill's steps. That is
# #410's real-run check.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR" || exit 2
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="/file-findings comments on an open origin and holds non-bug findings while the proposed backlog is at its cap (#410)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="jq, flock, git, sha256sum"
VALIDATE_DURATION="~3s"

validate_parse_args "$@"

require_cmd jq
require_cmd flock
require_cmd git
require_cmd sha256sum
check_prereqs

FINDING="$ROOT_DIR/scripts/finding.sh"
PLAN="$ROOT_DIR/.claude/skills/file-findings/plan.sh"

WORK_DIR="$(mktemp -d)"
cleanup() {
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

export FINDINGS_LOG="$WORK_DIR/findings.jsonl"
unset FINDINGS_PROPOSED_CAP

# Issues: #10 open, #12 closed, #13 open with an open PR (#14, from
# eharvey/#13). PRs: #11 open, #14. No #9999. These are what plan.sh's two
# list calls return after their --jq, which the stub doesn't run.
echo '[{"number":10,"state":"OPEN","kind":"issue"},{"number":12,"state":"CLOSED","kind":"issue"},
	{"number":13,"state":"OPEN","kind":"issue"}]' >"$WORK_DIR/issue.json"
echo '[{"number":11,"state":"OPEN","headRefName":"eharvey/#99","kind":"PR"},
	{"number":14,"state":"OPEN","headRefName":"eharvey/#13","kind":"PR"}]' >"$WORK_DIR/pr.json"
STUB="$WORK_DIR/stub"
mkdir -p "$STUB"
cat >"$STUB/gh" <<EOF
#!/usr/bin/env bash
[ -z "\${STUB_FAIL:-}" ] || exit 1
case "\$*" in
"issue list --state open --label proposed"*) echo "\$STUB_PROPOSED" ;;
"issue list --state all"* | "pr list --state all"*)
	[ -z "\${STUB_FAIL_STATES:-}" ] || exit 1
	cat "$WORK_DIR/\$1.json" ;;
*) echo "stub gh: unexpected \$*" >&2; exit 9 ;;
esac
EOF
chmod +x "$STUB/gh"
export PATH="$STUB:$PATH"

add() { "$FINDING" add --branch main "$@"; }
OPEN_ISSUE=$(add --kind drift --where a.md:1 --origin 10 "drift, origin an open issue")
OPEN_PR=$(add --kind follow-up --where b.sh:2 --origin 11 "follow-up, origin an open PR")
BUG_CLOSED=$(add --kind bug --where c.go:3 --origin 12 "bug, origin closed")
FU_CLOSED=$(add --kind follow-up --where d.sh:4 --origin 12 "follow-up, origin closed")
DRIFT=$(add --kind drift --where e.md:5 "drift, no origin")
BUG=$(add --kind bug --where f.go:6 "bug, no origin")
FU_SHIPPED=$(add --kind follow-up --where g.sh:7 --origin 13 "follow-up, origin open but shipped")
BUG_OPEN=$(add --kind bug --where h.go:8 --origin 10 "bug, origin an open issue")
FU_GONE=$(add --kind follow-up --where i.sh:9 --origin 9999 "follow-up, origin no issue or PR")

# verdict <plan output> <id> — that record's verdict column.
verdict() { awk -F'\t' -v id="$2" '$1 == id { print $2 }' <<<"$1"; }

# --- 1. At the cap ------------------------------------------------------------

BEFORE=$(sha256sum "$FINDINGS_LOG")
OUT=$(STUB_PROPOSED=18 "$PLAN" --all 2>"$WORK_DIR/err")
check_eq "backlog 18: exits 0" 0 "$?"
check_eq "backlog 18: reports the backlog" "proposed backlog: 18/15 (FINDINGS_PROPOSED_CAP)" "$(head -1 <<<"$OUT")"
check_eq "open origin issue: comment there" "comment #10 (open issue)" "$(verdict "$OUT" "$OPEN_ISSUE")"
check_eq "open origin PR: comment there" "comment #11 (open PR)" "$(verdict "$OUT" "$OPEN_PR")"
check_eq "bug, closed origin: filed over the cap" file "$(verdict "$OUT" "$BUG_CLOSED")"
check_eq "follow-up, closed origin: held" "held: proposed backlog at 19/15" "$(verdict "$OUT" "$FU_CLOSED")"
check_eq "drift, no origin: held" "held: proposed backlog at 19/15" "$(verdict "$OUT" "$DRIFT")"
check_eq "bug, no origin: filed" file "$(verdict "$OUT" "$BUG")"
check_eq "follow-up, origin issue has an open PR: no comment, held" "held: proposed backlog at 20/15" \
	"$(verdict "$OUT" "$FU_SHIPPED")"
check_eq "bug, open origin: filed, not a comment" file "$(verdict "$OUT" "$BUG_OPEN")"
check_eq "follow-up, unknown origin: planned as no origin" "held: proposed backlog at 21/15" \
	"$(verdict "$OUT" "$FU_GONE")"
check "and warned about" grep -q 'origin #9999 is no issue or PR here' "$WORK_DIR/err"
check_eq "plan.sh never writes the log" "$BEFORE" "$(sha256sum "$FINDINGS_LOG")"

# --- 2. Reaching the cap mid-run ---------------------------------------------

OUT=$(STUB_PROPOSED=13 "$PLAN" --all 2>/dev/null)
check_eq "backlog 13: the bug takes it to 14" file "$(verdict "$OUT" "$BUG_CLOSED")"
check_eq "backlog 13: a follow-up takes it to 15" file "$(verdict "$OUT" "$FU_CLOSED")"
check_eq "backlog 13: the next non-bug is held at the cap" "held: proposed backlog at 15/15" "$(verdict "$OUT" "$DRIFT")"
check_eq "backlog 13: a bug is still filed" file "$(verdict "$OUT" "$BUG")"

# --- 3. The cap is the operator's ---------------------------------------------

OUT=$(STUB_PROPOSED=18 FINDINGS_PROPOSED_CAP=30 "$PLAN" --all 2>/dev/null)
check_eq "cap 30: backlog line" "proposed backlog: 18/30 (FINDINGS_PROPOSED_CAP)" "$(head -1 <<<"$OUT")"
check_eq "cap 30: nothing held" 0 "$(grep -c 'held:' <<<"$OUT")"
check_eq "cap 30: seven filed" 7 "$(awk -F'\t' '$2 == "file"' <<<"$OUT" | wc -l | tr -d ' ')"
STUB_PROPOSED=18 FINDINGS_PROPOSED_CAP=lots "$PLAN" --all >/dev/null 2>&1
check_eq "a non-numeric cap is refused" 1 "$?"

# --- 4. Ids, failures ----------------------------------------------------------

OUT=$(STUB_PROPOSED=18 "$PLAN" "$DRIFT" "$BUG")
check_eq "ids restrict the plan to those records" 2 "$(tail -n +2 <<<"$OUT" | wc -l | tr -d ' ')"
OUT=$(STUB_PROPOSED=18 "$PLAN")
check_eq "no ids plans nothing" "proposed backlog: 18/15 (FINDINGS_PROPOSED_CAP)" "$OUT"
STUB_PROPOSED=18 "$PLAN" --all "$DRIFT" >/dev/null 2>&1
check_eq "--all takes no ids" 2 "$?"
OUT=$(STUB_PROPOSED=18 "$PLAN" no-such-id 2>&1)
check_eq "an unknown id is refused" 1 "$?"
check "and named" grep -q 'no-such-id is not an unfiled finding' <<<"$OUT"
OUT=$(STUB_FAIL=1 "$PLAN" --all 2>/dev/null)
check_eq "gh failing stops the plan" 1 "$?"
check_eq "with no verdicts" "" "$OUT"
OUT=$(STUB_PROPOSED=18 STUB_FAIL_STATES=1 "$PLAN" --all 2>/dev/null)
check_eq "the state lists failing stops it too" 1 "$?"
check_eq "also with no verdicts" "" "$OUT"

# --- 5. Marking a comment ------------------------------------------------------

check "mark-commented takes the PR number a comment went to" "$FINDING" mark-commented "$OPEN_PR" 11
check_json "the record is commented on #11, not filed" \
	"$(jq -c --arg id "$OPEN_PR" 'select(.id == $id)' "$FINDINGS_LOG")" \
	'.commented_on == 11 and .filed_as == null'
check "list says so" grep -q "^$OPEN_PR .*commented on #11\$" <<<"$("$FINDING" list)"
check_eq "list --commented re-surfaces it" "$OPEN_PR" "$("$FINDING" list --commented | awk '{print $1}')"
check_eq "count --unfiled leaves it out" 8 "$("$FINDING" count --unfiled)"
OUT=$(STUB_PROPOSED=18 "$PLAN" --all 2>/dev/null)
check_eq "a commented record drops out of the next plan" "" "$(verdict "$OUT" "$OPEN_PR")"

summary
