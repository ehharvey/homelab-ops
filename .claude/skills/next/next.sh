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
# planned, a cycle, and dependency prose that has no matching link.
# shellcheck disable=SC2016 # the $vars in single quotes are GraphQL's and jq's
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

gh api graphql --paginate -F owner="${repo%/*}" -F name="${repo#*/}" -f query='
query($owner: String!, $name: String!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, first: 100, after: $endCursor) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title body createdAt
        labels(first: 20) { nodes { name } }
        blockedBy(first: 50) { nodes { number state stateReason } }
        blocking(first: 50) { nodes { number state } }
      }
    }
  }
}' | jq -s '[.[].data.repository.issues.nodes[]]' >"$tmp/open.json"

# Every issue's and PR's state, so a blocker can be shown open or closed.
{
	gh issue list --state all --limit 5000 --json number,state
	gh pr list --state all --limit 5000 --json number,state
} | jq -s 'add | map({key: (.number | tostring), value: .state}) | from_entries' >"$tmp/states.json"

gh pr list --state open --json number,title,headRefName,isDraft,autoMergeRequest,statusCheckRollup \
	>"$tmp/prs.json"

# Roadmap items as TSV: section, depth, checked, text (continuation lines joined).
awk '
function flush() { if (text != "") print section "\t" depth "\t" checked "\t" text; text = "" }
/^## / { flush(); section = substr($0, 4); next }
/^[ \t]*- \[[ xX]\] / {
	flush()
	match($0, /^[ \t]*/); depth = int(RLENGTH / 2)
	checked = ($0 ~ /- \[[xX]\]/) ? "x" : " "
	text = $0; sub(/^[ \t]*- \[[ xX]\] /, "", text); next
}
/^[ \t]*$/ || /^[^ \t]/ { flush(); next }
{ if (text != "") { line = $0; sub(/^[ \t]+/, "", line); text = text " " line } }
END { flush() }
' docs/Roadmap.md >"$tmp/roadmap.tsv"

# Issues the Roadmap marks paused, so a paused blocker reads differently from a live one.
jq -R -s '[split("\n")[] | split("\t") | select(.[2] == " " and (.[3] // "" | startswith("*(paused")))
  | .[3] | scan("see #[0-9]+(?:, #[0-9]+)*") | scan("#([0-9]+)") | .[0] | tonumber] | unique' \
	"$tmp/roadmap.tsv" >"$tmp/paused.json"

common='
def short: sub("^\\[[A-Za-z ]+\\]: *"; "") | if length > 90 then .[:89] + "…" else . end;
def st($n): $states[0][$n | tostring] // "?";
def blockers: [.blockedBy.nodes[] | select(.state == "OPEN") | .number];
def done_deps: [.blockedBy.nodes[] | select(.state != "OPEN") | .number];
def unblocks: [.blocking.nodes[] | select(.state == "OPEN") | .number];
def ind: [range(.depth)] | map("  ") | add // "";
def tag($n): if ($paused[0] | index($n)) then "#\($n) (paused)" else "#\($n)" end;
def prfor($n): [$prs[0][] | select(.headRefName == "eharvey/#\($n)") | .number];
'
# Every filter gets the shared defs and all three inputs, since a jq def that
# names an unbound $var fails to compile even when it's never called.
q() { jq --slurpfile states "$tmp/states.json" --slurpfile prs "$tmp/prs.json" \
	--slurpfile open "$tmp/open.json" --slurpfile paused "$tmp/paused.json" "$@"; }

echo "== Ready issues (blockers: native blocked-by links) =="
q -r "$common"'
  .[] | select(any(.labels.nodes[]; .name == "Ready"))
  | blockers as $open | done_deps as $done | unblocks as $unblocks
  | ([.labels.nodes[].name | select(startswith("phase-"))] | join(",")) as $phase
  | (if ($open | length) > 0 then "BLOCKED" else "clear  " end) as $tag
  | "#\(.number) \($tag) [\(if $phase == "" then "no phase" else $phase end)] \(.title | short)",
    (if ($open | length) > 0 then "      blocked by: \($open | map(tag(.)) | join(" "))" else empty end),
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

# Dependency prose with no matching link, for issues filed or edited outside
# /file-task. Phrasings are #234's keyword set plus the ones its review found
# in Roadmap phase lines ("needed by", "follows", "should land before" …).
# Only the #N list right after the phrase counts, quoted text is skipped (that's
# a mention, not a dependency), and only open targets count: prose about a
# closed issue can't make /next wrong.
echo "== Dependency inconsistencies =="
q -r "$common"'
  (map({key: (.number | tostring), value: [blockers[]]}) | from_entries) as $g
  | def reach($s): {seen: [], f: $g[$s | tostring]}
      | until(.f | length == 0; (.f - .seen) as $new | .seen += $new | .f = ([$new[] | $g[tostring][]?] | unique))
      | .seen;
    def prose($re): [(.body // "") | gsub("\"[^\"\n]*\"|“[^”\n]*”|`[^`\n]*`"; "")
        | scan("(?i)(?:" + $re + ") *(#[0-9]+(?:(?:,| and| or|/) *#[0-9]+)*)") | .[0]
        | scan("#([0-9]+)") | .[0] | tonumber]
      | unique | map(select(st(.) == "OPEN"));
  (map(.number) | map(select(. as $n | reach($n) | index($n)))) as $cyc
  | [ (.[] | select(any(.labels.nodes[]; .name == "Ready")) | select(any(.labels.nodes[]; .name == "proposed"))
       | "#\(.number) has both Ready and proposed; triage drops proposed (until then /run-chain skips it)"),
      (.[] | select(any(.labels.nodes[]; .name == "Ready")) | select(blockers | length > 0)
       | "#\(.number) is Ready but blocked by open \(blockers | map(tag(.)) | join(" "))"),
      (.[] | . as $i | .blockedBy.nodes[] | select(.stateReason == "NOT_PLANNED" or .stateReason == "DUPLICATE")
       | "#\($i.number) is blocked by #\(.number), closed as \(.stateReason | ascii_downcase | sub("_"; " ")); drop the link or rethink #\($i.number)"),
      ($cyc | map(. as $n | [$n] + ([reach($n)[] | select(. as $m | $cyc | index($m)) | select(reach(.) | index($n))]) | unique) | unique[]
       | "cycle: \(map("#\(.)") | join(" ↔ "))"),
      (.[] | . as $i
       | ((prose("depends on|blocked (?:by|on)|waits? on|needs|follows|follow-up to|(?:lands?|land it|sequence it|any time) after")
           - [.blockedBy.nodes[].number] - [$i.number]) | map("blocked by #\(.)")) as $by
       | ((prose("(?:un)?blocks|prerequisite (?:for|of)|needed by|(?:should |must )?(?:lands?|be done|done|merged?) before")
           - [.blocking.nodes[].number] - [$i.number]) | map("blocking #\(.)")) as $bl
       | select(($by + $bl) | length > 0)
       | "#\($i.number) prose says \($by + $bl | join(", ")), with no link") ]
  | if length == 0 then "(none)" else .[] end
' "$tmp/open.json"
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
