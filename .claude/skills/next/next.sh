#!/usr/bin/env bash
# The data behind /next (#222): Ready issues with what blocks them, the current
# Roadmap phase's unchecked items, proposed issues awaiting triage, open PRs
# with their checks, and local issue branches. Everything is filtered down
# here, so the report stays at a few hundred lines however many issues exist; a
# session reads it once rather than re-deriving it over 10–30 gh calls.
#
# Blockers come from GitHub's native blocked-by links only (#234 moved the
# repo's dependency prose onto them). The inconsistencies section is what keeps
# that honest: a Ready issue with an open blocker, a blocker closed as not
# planned, a cycle, and dependency prose that has no matching link. It also
# flags a Ready issue whose Done-when names no validate script or real-run
# check, by the check queue.sh shares (.claude/skills/file-task/done-when.jq, #410).
# Those checks are scripts/hygiene.sh's `deps` group (#388); this script prints
# them on the data it fetched.
#
# A Ready sub-issue of an open bundle (/bundle, #409) is tagged `bundled`, not
# `clear`: it ships with its parent, so it isn't work to start on its own.
# shellcheck disable=SC2016 # the $vars in single quotes are GraphQL's and jq's
set -euo pipefail

# shellcheck source=SCRIPTDIR/../lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

# --order sorts the Ready section only (#392): keys p0, unblocks, number, as in
# lib.sh; run-chain/queue.sh takes the same flag and default.
usage() {
	echo "usage: next.sh [--order <key>[,<key>...]]   keys: p0, unblocks, number (default $order_default)" >&2
	exit 2
}
order_arg=$order_default
while [ $# -gt 0 ]; do
	case $1 in
	--order) [ $# -ge 2 ] || usage; order_arg=$2; shift 2 ;;
	--order=*) order_arg=${1#--order=}; shift ;;
	*) usage ;;
	esac
done
order=$(order_json "$order_arg") || usage

skills=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$(git rev-parse --show-toplevel)"
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The fetches are lib.sh's, shared with scripts/hygiene.sh, which reads these
# same files for the inconsistencies section (--data).
open_issues_json "$repo" >"$tmp/open.json"
issue_states_json >"$tmp/states.json"

gh pr list --state open --json number,title,headRefName,isDraft,autoMergeRequest,statusCheckRollup \
	>"$tmp/prs.json"

roadmap_tsv >"$tmp/roadmap.tsv"
paused_json <"$tmp/roadmap.tsv" >"$tmp/paused.json"

common='
include "lib";
def short: sub("^\\[[A-Za-z ]+\\]: *"; "") | if length > 90 then .[:89] + "…" else . end;
def st($n): $states[0][$n | tostring] // "?";
def blockers: [.blockedBy.nodes[] | select(.state == "OPEN") | .number];
def done_deps: [.blockedBy.nodes[] | select(.state != "OPEN") | .number];
def ind: [range(.depth)] | map("  ") | add // "";
def tag($n): if ($paused[0] | index($n)) then "#\($n) (paused)" else "#\($n)" end;
def prfor($n): [$prs[0][] | select(.headRefName == "eharvey/#\($n)") | .number];
'
# Every filter gets the shared defs and all three inputs, since a jq def that
# names an unbound $var fails to compile even when it's never called.
q() { jq -L "$skills" --slurpfile states "$tmp/states.json" --slurpfile prs "$tmp/prs.json" \
	--slurpfile open "$tmp/open.json" --slurpfile paused "$tmp/paused.json" "$@"; }

echo "== Ready issues, by --order $order_arg (blockers: native blocked-by links) =="
q -r --argjson order "$order" "$common"'
  [.[] | select(any(.labels.nodes[]; .name == "Ready"))] | ready_order($order) | .[]
  | blockers as $open | done_deps as $done | unblocks as $unblocks
  | ([.labels.nodes[].name | select(startswith("phase-"))] | join(",")) as $phase
  | (if ($open | length) > 0 then "BLOCKED" elif bundled then "bundled" else "clear  " end) as $tag
  | "#\(.number) \($tag) [\(if $phase == "" then "no phase" else $phase end)\(if is_p0 then ",p0" else "" end)] \(.title | short)",
    (if ($open | length) > 0 then "      blocked by: \($open | map(tag(.)) | join(" "))" else empty end),
    (if bundled then "      ships with: bundle #\(.parent.number), not on its own" else empty end),
    (if ($done | length) > 0 then "      done deps:  \($done | map("#\(.)") | join(" "))" else empty end),
    (if ($unblocks | length) > 0 then "      unblocks:   \($unblocks | map("#\(.)") | join(" "))" else empty end),
    (prfor(.number) | if length > 0 then "      in flight:  PR \(map("#\(.)") | join(" "))" else empty end)
' "$tmp/open.json"
echo

# Issues Claude filed on its own (/run-chain's "Noticed" items, #253) carry
# `proposed` and never `Ready`; the operator triages them. The origin is the
# "Noticed while working on #N" line /file-task writes into the body.
echo "== Proposed — awaiting triage (add Ready and drop proposed, or close) =="
q -r "$common"'
  [.[] | select(any(.labels.nodes[]; .name == "proposed"))] | sort_by(.number)
  | if length == 0 then "(none)" else .[]
    | ((now - (.createdAt | fromdateiso8601)) / 86400 | floor) as $age
    | ([(.body // "") | scan("(?i)noticed while working on #([0-9]+)") | .[0]] | first) as $from
    | "#\(.number) \($age)d\(if $from then " from #\($from)" else "" end) \(.title | short)"
  end
' "$tmp/open.json"
echo

# The checks live in scripts/hygiene.sh (#388), which the nightly hygiene.yml
# also runs; --data hands it the files fetched above. It exits 1 when it finds
# something, which here is a report line, not a failure.
echo "== Dependency inconsistencies =="
rc=0
./scripts/hygiene.sh --summary --group deps --data "$tmp" || rc=$?
[ "$rc" -le 1 ] || exit "$rc"
echo

q -R -s -r "$common"'
  ($open[0] | map({key: (.number | tostring), value: [.labels.nodes[].name]}) | from_entries) as $labels
  | [split("\n")[] | select(. != "") | split("\t") | {section: .[0], depth: (.[1] | tonumber), checked: (.[2] == "x"), text: .[3]}] as $items
  | ([$items[] | select(.section | startswith("Phase")) | select(.checked | not) | .section] | first) as $cur
  | "== Current Roadmap phase: \($cur // "none, every Phase item is checked") — unchecked items (docs/Roadmap.md) ==",
    ($items[] | select(.section == $cur and (.checked | not))
     | (.text | [scan("see #[0-9]+(?:, #[0-9]+)*") | scan("#([0-9]+)") | .[0]] | unique) as $refs
     | ($refs | map(. as $r | "#\($r) \(st($r) | ascii_downcase)\(if ($labels[$r] // [] | index("Ready")) then ", Ready" else "" end)\(if st($r) == "CLOSED" then " — check off?" else "" end)") | join("; ")) as $refstr
     | "\(ind)- \(.text | sub("\\*\\(paused, §[0-9]+\\)\\* *"; "[paused] ") | if length > 110 then .[:109] + "…" else . end)",
       (if $refstr != "" then "\(ind)    → \($refstr)" else empty end)),
    "",
    "== Other sections with unchecked items ==",
    ($items | map(select(.checked | not)) | group_by(.section)[] | select(.[0].section != $cur)
     | "  \(.[0].section): \(length) unchecked")
' "$tmp/roadmap.tsv"
echo

cur_label=$(awk -F'\t' '$1 ~ /^Phase/ && $3 == " " { print $1; exit }' "$tmp/roadmap.tsv" |
	sed -nE 's/^Phase ([0-9.]+).*/phase-\1/p')
if [ -n "$cur_label" ]; then
	echo "== Open $cur_label issues not yet Ready (design unsettled) =="
	q -r --arg l "$cur_label" "$common"'
	  .[] | select(any(.labels.nodes[]; .name == $l) and all(.labels.nodes[]; .name != "Ready"))
	  | "#\(.number) \(.title | short)"
	' "$tmp/open.json"
	echo
fi

echo "== Open PRs =="
jq -r '
  def bucket: (.conclusion // .state // "") as $c
    | if $c == "SUCCESS" or $c == "SKIPPED" or $c == "NEUTRAL" then "pass"
      elif $c == "" or $c == "PENDING" or $c == "EXPECTED" then "pending"
      else "fail" end;
  if length == 0 then "(none)" else .[] |
    ([.statusCheckRollup[] | {name: (.name // .context), b: bucket}]) as $c
    | "#\(.number) \(.headRefName)\(if .isDraft then " [draft]" else "" end) — \(.title | if length > 70 then .[:69] + "…" else . end)",
      "      checks: \([$c[] | select(.b == "pass")] | length) pass, \([$c[] | select(.b == "pending")] | length) pending, \([$c[] | select(.b == "fail")] | length) fail\([$c[] | select(.b == "fail") | .name] | if length > 0 then " (" + join(", ") + ")" else "" end); auto-merge \(if .autoMergeRequest then "on" else "off (make lgtm is the operator'"'"'s call)" end)"
  end
' "$tmp/prs.json"
echo

echo "== Local issue branches (vs origin/main) =="
git fetch -q origin main 2>/dev/null || echo "(fetch failed; ahead counts may be stale)"
stale=0 other=0
while read -r br; do
	n=${br#eharvey/#}
	[ "$n" = "$br" ] && { other=$((other + 1)); continue; }
	state=$(jq -r --arg n "$n" '.[$n] // "?"' "$tmp/states.json")
	[ "$state" = "CLOSED" ] && { stale=$((stale + 1)); continue; }
	ahead=$(git rev-list --count "origin/main..$br" 2>/dev/null || echo "?")
	pr=$(jq -r --arg b "$br" '[.[] | select(.headRefName == $b) | "PR #\(.number)"] | first // "no PR"' "$tmp/prs.json")
	echo "$br: $ahead ahead, issue $(echo "$state" | tr '[:upper:]' '[:lower:]'), $pr"
done < <(git for-each-ref --format='%(refname:short)' 'refs/heads/eharvey/*')
# main is rebase-only, so a merged branch still counts as "ahead": the issue's
# state is the only reliable merged signal.
echo "($stale branches for closed issues, $other not named for an issue; not listed)"
