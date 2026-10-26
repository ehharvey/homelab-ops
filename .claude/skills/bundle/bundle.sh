#!/usr/bin/env bash
# The gh calls behind /bundle (#409). Grouping is the skill's judgment and the
# operator's OK; this only fetches and attaches.
#
#   bundle.sh list                     # open `small` issues free to bundle
#   bundle.sh show <n> ...             # each one's labels, Done when, Doc references
#   bundle.sh attach <parent> <n> ...  # make each <n> a native sub-issue of <parent>
#
# `list` leaves out an issue that's in flight (an open PR or a local branch
# eharvey/#<n>), a bundle parent itself, or a sub-issue of anything but a
# closed bundle, and says which it left out and why. GitHub gives an issue one
# parent, so an umbrella's sub-issue can't join a bundle without leaving the
# umbrella; that's the operator's call, made on GitHub. A closed bundle is a
# reverted one, and `attach` moves its children. `list` prints each candidate's
# `### Doc references` paths and any decision it cites (`cites` in ../lib.jq),
# since a parent carries those over and they decide its /run-chain lane.
#
# `attach` checks every child before it changes anything: it refuses, attaching
# none, when one has a parent other than a closed bundle. It then uses REST
# `POST /repos/{owner}/{repo}/issues/{parent}/sub_issues`, with
# `replace_parent` for a closed bundle's child. `sub_issue_id` is the child's
# numeric id (GraphQL's databaseId), not its number: the endpoint rejects a
# number. A failed POST doesn't stop the rest. It ends by listing the parent's
# sub-issues, which is the check that they're attached, and exits 1 if any
# POST failed.
# shellcheck disable=SC2016 # the $vars in single quotes are GraphQL's and jq's
set -euo pipefail

usage() {
	echo "usage: bundle.sh list | show <n> ... | attach <parent> <n> ..." >&2
	exit 2
}
num() { [[ $1 =~ ^#?[0-9]+$ ]] || usage; }

cmd=${1:-}
shift || true
case $cmd in
list) (($# == 0)) || usage ;;
show) (($# >= 1)) || usage ;;
attach) (($# >= 2)) || usage ;;
*) usage ;;
esac
for a in "$@"; do num "$a"; done

skills=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$(git rev-parse --show-toplevel)"
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

# A body's `### <heading>` section, up to the next one. Its paths: backticked
# spans that look like one (a `/` or a file extension; a `:<line>` suffix is
# dropped), then unquoted paths. Only a docs/ name may hold spaces
# (`docs/Out of Scope.md`), so backticked prose isn't a path; unquoted, its
# words after the first are capitalised or a short connector like `of`.
# URLs are dropped first, so a host isn't read as a directory.
jq_defs='
  include "lib";
  def section($h): (.body // "") | [capture("### " + $h + "[ \\t]*\\r?\\n(?<s>[\\s\\S]*?)(?=\\r?\\n### |\\z)").s] | first // "";
  def paths: section("Doc references") | gsub("https?://[^\\s)>`]+"; "") as $s
    | "docs/[\\w-]+(?: (?:[A-Z][\\w-]*|of|and|the|to|for|in))*\\.md" as $doc
    | [ ($s | scan("`([^`\\n]+)`") | .[0] | sub(":[0-9]+(?:-[0-9]+)?$"; "")
          | select(test("^docs/[\\w -]+\\.md$") or (test("^[\\w.][\\w./-]*$") and test("/|\\.(md|sh|go|ya?ml|json)$")))),
        ($s | gsub("`[^`\\n]*`"; " ") | scan($doc), (gsub($doc; " ") | scan("[\\w.-]+(?:/[\\w.-]+)+/?|[\\w-]+\\.(?:md|sh|go|ya?ml|json)"))) ]
    | unique;
'

case $cmd in
list)
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	gh api graphql --paginate -F owner="${repo%/*}" -F name="${repo#*/}" -f query='
query($owner: String!, $name: String!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, labels: ["small"], first: 100, after: $endCursor) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title body
        labels(first: 20) { nodes { name } }
        parent { number state title }
        subIssues(first: 1) { totalCount }
      }
    }
  }
}' | jq -s '[.[].data.repository.issues.nodes[]]' >"$tmp/small.json"
	gh pr list --state open --json number,headRefName >"$tmp/prs.json"
	git for-each-ref --format='%(refname:short)' 'refs/heads/eharvey/*' |
		sed -n 's|^eharvey/#\([0-9][0-9]*\)$|\1|p' | jq -R 'tonumber' | jq -s . >"$tmp/local.json"

	jq -r -L "$skills" --slurpfile prs "$tmp/prs.json" --slurpfile local "$tmp/local.json" "$jq_defs"'
	  def labels: [.labels.nodes[].name];
	  def pr($n): [$prs[0][] | select(.headRefName == "eharvey/#\($n)") | .number] | first;
	  def why: .number as $n
	    | if pr($n) then "in flight: PR #\(pr($n))"
	      elif ($local[0] | index($n)) then "in flight: local branch eharvey/#\($n)"
	      elif bundled then "already in bundle #\(.parent.number)"
	      elif .parent != null and (.parent.state != "CLOSED" or (.parent.title | bundle_title | not))
	        then "sub-issue of #\(.parent.number); an issue has one parent, so detach it there first"
	      elif (.title | bundle_title) or .subIssues.totalCount > 0 then "a parent issue itself"
	      else null end;
	  sort_by(.number)
	  | ([.[] | select(why == null)]) as $free
	  | "== Free to bundle (\($free | length)) ==",
	    ($free | if length == 0 then "(none)" else .[] |
	      "#\(.number)  [\(if (labels | index("proposed")) then "proposed" elif (labels | index("Ready")) then "Ready" else "not Ready" end)]  \(.title | sub("^\\[[A-Za-z ]+\\]: *"; ""))",
	      "      labels: \(labels - ["small", "Ready", "proposed"] | join(","))",
	      "      refs:   \(paths | if length == 0 then "(none)" else .[:8] | join(" ") end)",
	      "      cites:  \(cites | if length == 0 then "none" else join(" ") end)" end),
	    "",
	    "== Left out ==",
	    ([.[] | select(why != null) | "#\(.number)  \(why)"] | if length == 0 then "(none)" else .[] end)
	' "$tmp/small.json"
	;;
show)
	for a in "$@"; do
		gh issue view "${a#\#}" --json number,title,body,labels | jq -r -L "$skills" "$jq_defs"'
		  "== #\(.number)  \(.title) ==",
		  "labels: \([.labels[].name] | join(","))",
		  "cites:  \(cites | if length == 0 then "none" else join(" ") end)",
		  "-- Done when --", (section("Done when") | split("\n")[:40] | .[]),
		  "-- Doc references --", (section("Doc references") | split("\n")[:20] | .[]),
		  ""'
	done
	;;
attach)
	parent=${1#\#}
	shift
	# One line per child: number, databaseId, and `ok`, `already` (attached
	# here), `replace` (a closed bundle's child) or why it can't be attached.
	plan=$(for a in "$@"; do
		gh api graphql -F owner="${repo%/*}" -F name="${repo#*/}" -F n="${a#\#}" -f query='
query($owner: String!, $name: String!, $n: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $n) { number databaseId parent { number state title } }
  }
}' | jq -r -L "$skills" --argjson p "$parent" 'include "lib";
		  .data.repository.issue
		  | "\(.number) \(.databaseId) " + (
		      if .parent == null then "ok"
		      elif .parent.number == $p then "already"
		      elif .parent.state == "CLOSED" and (.parent.title | bundle_title) then "replace"
		      else "sub-issue of #\(.parent.number), which isn'"'"'t a closed bundle" end)'
	done)
	if grep -qv ' \(ok\|already\|replace\)$' <<<"$plan"; then
		echo "attached nothing:" >&2
		grep -v ' \(ok\|already\|replace\)$' <<<"$plan" | sed 's/^\([0-9]*\) [0-9]* /  #\1 is a /' >&2
		exit 1
	fi
	failed=0
	while read -r n id how; do
		args=(-F sub_issue_id="$id") note=""
		case $how in
		already)
			echo "#$n is already a sub-issue of #$parent"
			continue
			;;
		replace) args+=(-F replace_parent=true) note=" (moved from a closed bundle)" ;;
		esac
		if gh api -X POST "repos/$repo/issues/$parent/sub_issues" "${args[@]}" --silent; then
			echo "attached #$n (id $id) to #$parent$note"
		else
			echo "FAILED to attach #$n" >&2
			failed=1
		fi
	done <<<"$plan"
	echo "== sub-issues of #$parent =="
	gh api "repos/$repo/issues/$parent/sub_issues?per_page=100" --jq '.[] | "#\(.number)  \(.state)  \(.title)"'
	exit "$failed"
	;;
esac
