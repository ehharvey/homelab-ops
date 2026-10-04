#!/usr/bin/env bash
# Which issues a /run-chain run takes (#253), and why it leaves out the rest.
#
#   .claude/skills/run-chain/queue.sh            # every Ready issue
#   .claude/skills/run-chain/queue.sh 240 257    # only these, if they're Ready
#
# Each candidate gets one verdict:
#   run    Ready, not `proposed`, no open blocker, nothing in flight
#   park   an open blocker is in this run or has an open PR: it can't start
#          until that PR merges, or until stacked PRs exist (#254)
#   skip   anything else, with the reason
#
# `proposed` wins over `Ready`: an issue carrying both hasn't been triaged, and
# the triage gate is the point of the label. Run order is most-unblocking
# first, then issue number; with arguments, it's the order given.
# shellcheck disable=SC2016 # the $vars in single quotes are GraphQL's and jq's
set -euo pipefail

for a in "$@"; do
	[[ $a =~ ^#?[0-9]+$ ]] || {
		echo "usage: queue.sh [<issue> ...]" >&2
		exit 2
	}
done

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
        number title
        labels(first: 20) { nodes { name } }
        blockedBy(first: 50) { nodes { number state } }
        blocking(first: 50) { nodes { number state } }
      }
    }
  }
}' | jq -s '[.[].data.repository.issues.nodes[]]' >"$tmp/open.json"

gh pr list --state open --json number,headRefName >"$tmp/prs.json"

# Local issue branches: an unshipped branch is work to resume by hand, not to
# start over in a fresh worktree.
git for-each-ref --format='%(refname:short)' 'refs/heads/eharvey/*' |
	sed -n 's|^eharvey/#\([0-9][0-9]*\)$|\1|p' | jq -R 'tonumber' | jq -s . >"$tmp/local.json"

printf '%s\n' "$@" | tr -d '#' | jq -R 'select(. != "") | tonumber' | jq -s . >"$tmp/want.json"

jq -r --slurpfile prs "$tmp/prs.json" --slurpfile local "$tmp/local.json" --slurpfile want "$tmp/want.json" '
  def short: sub("^\\[[A-Za-z ]+\\]: *"; "") | if length > 80 then .[:79] + "…" else . end;
  def labels: [.labels.nodes[].name];
  def pr($n): [$prs[0][] | select(.headRefName == "eharvey/#\($n)") | .number] | first;
  def unblocks: [.blocking.nodes[] | select(.state == "OPEN")] | length;

  (map({key: (.number | tostring), value: .}) | from_entries) as $by
  | (if ($want[0] | length) > 0 then $want[0]
     else [.[] | select(labels | index("Ready"))] | sort_by(-unblocks, .number) | map(.number) end) as $cand
  # Pass 1: eligibility, ignoring blockers.
  | [ $cand[] | . as $n | $by[$n | tostring] as $i
      | if $i == null then {n: $n, t: "(not found)", v: "skip", why: "not an open issue"}
        else $i | (labels) as $l | [.blockedBy.nodes[] | select(.state == "OPEN") | .number] as $b
          | {n: $n, t: (.title | short), b: $b} +
          if ($l | index("proposed")) then {v: "skip", why: "proposed: awaiting the operator'"'"'s triage"}
          elif ($l | index("Ready") | not) then {v: "skip", why: "not Ready"}
          elif pr($n) then {v: "skip", why: "in flight: PR #\(pr($n))"}
          elif ($local[0] | index($n)) then {v: "skip", why: "local branch eharvey/#\($n) exists: resume it by hand"}
          elif ($b | length) == 0 then {v: "run"}
          else {v: "?"}
          end
        end ] as $rows0
  # Pass 2: an eligible issue is parked when every open blocker is running,
  # has an open PR, or is itself parked; repeat until nothing changes, so a
  # chain parks in dependency order. Whatever is left is blocked from outside.
  | [$rows0[] | select(.v == "run") | .n] as $run
  | { rows: $rows0, ok: ($run + [$prs[0][] | .headRefName
        | capture("^eharvey/#(?<n>[0-9]+)$").n | tonumber]) }
  | until(([.rows[] | select(.v == "?")] | length) == 0 or (. as $s | [.rows[] | select(.v == "?" and all(.b[]; . as $m | $s.ok | index($m)))] | length) == 0;
      .ok as $ok
      | ([.rows[] | select(.v == "?" and all(.b[]; . as $m | $ok | index($m))) | .n]) as $new
      | .rows |= map(if (.n as $n | $new | index($n)) then .v = "park" | .why = "waits on \(.b | map(. as $m | "#\($m)" + (pr($m) as $p | if $p then " (PR #\($p))" elif ($run | index($m)) then " (this run)" else " (parked)" end)) | join(", ")) to merge; stacked PRs are #254" else . end)
      | .ok += $new)
  | .rows | map(if .v == "?" then .v = "skip" | .why = "blocked by open \(.b | map("#\(.)") | join(", ")), outside this run" else . end)
  | "== Run (in this order) ==",
    ([.[] | select(.v == "run") | "#\(.n)  \(.t)"] | if length == 0 then "(none)" else .[] end),
    "",
    "== Parked ==",
    ([.[] | select(.v == "park") | "#\(.n)  \(.t)\n      \(.why)"] | if length == 0 then "(none)" else .[] end),
    "",
    "== Skipped ==",
    ([.[] | select(.v == "skip") | "#\(.n)  \(.t)\n      \(.why)"] | if length == 0 then "(none)" else .[] end)
' "$tmp/open.json"
