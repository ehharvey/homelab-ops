#!/usr/bin/env bash
# The throttle /file-findings applies before filing anything (#410): which
# findings become a comment on their open origin, which get filed, and which
# are held under the `proposed` backlog cap. It decides; it changes nothing.
#
#   .claude/skills/file-findings/plan.sh <id> ... | --all
#
# Pass the ids the skill would act on: what's left after merging within-log
# duplicates and holding back thin records, one id per group (a group becomes
# one issue, so it counts once against the cap). No ids plans nothing; --all
# plans every unfiled record.
#
# Prints the backlog line, then one tab-separated line per record, in log order:
#   <id>  <verdict>  <kind>  <where>  <text>
# where the verdict is one of
#   comment #N (open issue|PR)  not a bug, and the origin is an open PR, or an
#                               open issue nothing has shipped for yet (no open
#                               PR from eharvey/#N): comment there instead of
#                               filing, then `finding.sh mark-commented <id> N`
#   file                        file it as a `proposed` issue
#   held: proposed backlog at N/cap
#                               not a bug, and open `proposed` issues are at or
#                               above the cap: it stays unfiled for a later run
#
# Bugs are always filed: never held, and never only a comment, which nothing
# tracks once its issue closes. An origin whose work has shipped gets no
# comment either: at /run-chain § 5 a worker's findings carry its own issue,
# still open behind its PR, and a comment there is buried when the PR's
# `Closes #N` closes it. Those are filed or held like findings with no origin.
#
# The cap is FINDINGS_PROPOSED_CAP (default 15), an operator-changeable default.
# The open `proposed` count is read once, then goes up by one for every `file`
# verdict, in order, so one run can't overshoot the cap. Every issue's and PR's
# state comes from two list calls, as in next.sh; an origin they don't hold
# (deleted, transferred, a typo) is warned about and planned as no origin.
set -euo pipefail

usage() {
	sed -n '6p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

die() {
	echo "plan.sh: $*" >&2
	exit 1
}

all=0
for a in "$@"; do
	case "$a" in --all) all=1 ;; -*) usage ;; esac
done
[ "$all" = 0 ] || [ $# -eq 1 ] || usage

cap="${FINDINGS_PROPOSED_CAP:-15}"
[[ "$cap" =~ ^[0-9]+$ ]] || die "FINDINGS_PROPOSED_CAP must be a number (got '$cap')"

finding="$(cd "$(dirname "$0")/../../.." && pwd)/scripts/finding.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

"$finding" list --unfiled --json --limit 100000 | jq -s . >"$tmp/unfiled.json"
if [ "$all" = 1 ]; then
	jq 'map(.id)' "$tmp/unfiled.json" >"$tmp/want.json"
else
	printf '%s\n' "$@" | jq -R 'select(. != "")' | jq -s . >"$tmp/want.json"
fi

jq -r --slurpfile want "$tmp/want.json" \
	'$want[0] - map(.id) | .[] | "plan.sh: \(.) is not an unfiled finding"' \
	"$tmp/unfiled.json" >"$tmp/missing"
if [ -s "$tmp/missing" ]; then
	cat "$tmp/missing" >&2
	exit 1
fi
jq --slurpfile want "$tmp/want.json" \
	'map(select(.id as $id | $want[0] | index($id)))' \
	"$tmp/unfiled.json" >"$tmp/records.json"

proposed=$(gh issue list --state open --label proposed --limit 1000 --json number --jq length) ||
	die "could not count open proposed issues; GitHub unreachable?"

# Every issue's and PR's state; the issues list holds no PRs.
if ! gh issue list --state all --limit 5000 --json number,state --jq 'map(. + {kind: "issue"})' >"$tmp/issues.json" ||
	! gh pr list --state all --limit 5000 --json number,state,headRefName --jq 'map(. + {kind: "PR"})' >"$tmp/prs.json"; then
	die "could not read issue and PR states; GitHub unreachable?"
fi
jq -s 'add' "$tmp/issues.json" "$tmp/prs.json" >"$tmp/states.json"

jq -r --slurpfile states "$tmp/states.json" \
	'[.[].origin | numbers] | unique - [$states[0][].number] | .[]' "$tmp/records.json" |
	while read -r o; do
		echo "plan.sh: origin #$o is no issue or PR here; planned as no origin" >&2
	done

echo "proposed backlog: $proposed/$cap (FINDINGS_PROPOSED_CAP)"
jq -r --argjson n "$proposed" --argjson cap "$cap" --slurpfile states "$tmp/states.json" '
  ($states[0] | map({key: (.number | tostring), value: .}) | from_entries) as $by
  | [$states[0][] | select(.kind == "PR" and .state == "OPEN") | .headRefName
      | capture("^eharvey/#(?<n>[0-9]+)$").n | tonumber] as $shipped
  | reduce .[] as $r ({n: $n, out: []};
    ($by[$r.origin | tostring] // null) as $o
    | if $r.kind == "bug" then
        .out += [[$r, "file"]] | .n += 1
      elif $o != null and $o.state == "OPEN"
          and ($o.kind == "PR" or (any($shipped[]; . == $r.origin) | not)) then
        .out += [[$r, "comment #\($r.origin) (open \($o.kind))"]]
      elif .n < $cap then
        .out += [[$r, "file"]] | .n += 1
      else
        .out += [[$r, "held: proposed backlog at \(.n)/\($cap)"]]
      end)
  | .out[] | [.[0].id, .[1], .[0].kind, .[0].where, .[0].text] | join("\t")
' "$tmp/records.json"
