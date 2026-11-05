#!/usr/bin/env bash
# Sync a PR's review-state labels with its reviews (#536):
#
#   scripts/review-state.sh <pr> [--dry-run] [--clean-pass] [--head <sha>] [--from <json>] [--save <json>]
#
# docs/Development Conventions.md § Review state labels defines the labels, a
# pass, "addressed", expected passes and the merge gate. This script is the one
# implementation of those rules: `make ship`, /review-work, /address-feedback
# and .github/workflows/review-state.yml all run it, so the labels can't drift
# from the data. The label names and the merge gate live in
# scripts/review-gate.jq, which /next's next.sh reads too. It works the state
# out afresh on every run, never adjusting a count in place, and touches only
# the labels it owns (reviewed-work, review-unaddressed,
# review-passes-left-<n>), and those only when they differ from what the data
# says. An unchanged PR makes no API writes at all, so re-running it adds no
# `labeled`/`unlabeled` timeline events.
#
# One bounded GraphQL call reads the PR's head, base, body, labels, closing
# issues' labels, reviews, review threads and comments, plus the repo's default
# branch and its labels whose name contains "review" (to know which it must
# create: `--add-label` on a missing label fails). Bounds: the last 100 reviews,
# the last 100 threads with the first 50 comments each, the last 100 PR
# comments, 100 PR labels, 20 closing issues with 100 labels each. A PR past
# any of them gets an `incomplete:` line and no label writes: a state worked
# out from part of the data would be wrong, and wrong labels are worse than
# stale ones.
#
# A stacked PR (its base is another issue branch, not the default branch) gets
# no closing issues from GitHub, which links them only on a PR into the
# default branch. For one, a second call reads the issues its body names with
# a closing keyword (`Closes #N`), up to 20, and the response keeps them as
# `.data.bodyIssues`, so a saved fixture carries them too. A named number
# that's a PR, or that has nothing behind it, counts as no issue.
#
# How the data maps onto the rules, since everything is posted as the
# operator's account and the author tells nothing apart:
#   - A /code-review pass is a submitted review that opened a thread (wrote a
#     thread's first comment). A thread reply is its own empty-body COMMENTED
#     review (#515), and a review with only a body (an ordinary web-UI
#     comment) is neither. Empty-body reviews that open threads, back to back
#     on the same commit and each within 60s of the one before, are one pass:
#     a run that posts each finding as its own comment makes one review per
#     finding, seconds apart, while a second run comes minutes later.
#   - A pass is addressed when each thread it opened is resolved or its newest
#     comment is an /address-feedback reply, one starting
#     `<!-- address-feedback:reply -->`. Any later comment (the operator's
#     "still broken", another pass's note) makes the thread unaddressed again.
#   - A clean pass is a PR comment starting with
#     `<!-- review-state:clean-pass <head sha> -->`: an addressed pass with no
#     findings at that commit. On the head it ends the countdown; after a push
#     past it, with 2 expected, one more addressed pass is needed, whatever
#     came before it.
#   - A /review-work verdict is a PR comment starting with
#     `<!-- review-work:verdict ready -->` or `... not-ready -->`, and an
#     /address-feedback reply to it starts with
#     `<!-- address-feedback:verdict-reply <verdict comment node id> -->`.
#     Markers count only at the start of a body, so a comment that quotes one
#     is not a verdict or a reply.
#
# --dry-run prints the state and the label changes and changes nothing.
# --from <json> reads a saved response of the GraphQL call instead of
# fetching, and implies --dry-run, so it never calls gh (the Go tests in
# scripts/review_state_test.go drive it this way). --save <json> writes the
# fetched response there, to make such a fixture. --clean-pass posts the
# clean-pass comment at the PR's head before working the state out, and adds
# the comment it posted to the data rather than re-reading (a read just after
# the post can miss it); under --dry-run it only adds that comment to the data.
# Outside a dry run it needs --head, the commit the clean run reviewed, so it
# never records whatever head GitHub serves. --head <sha> names the commit the
# PR's head must be: GitHub can serve the old head, and a new PR's closing
# issues, for a few seconds after a push (make ship), so it polls a small
# query (up to ~30s) until the head is <sha> and, on a PR into the default
# branch whose body names an issue (not a PR) it closes, the closing issues are
# there, then reads the rest once. If the head never shows, it writes nothing.
# --clean-pass checks the read bounds before it posts, since its comment is
# permanent.
#
# Exit status: 0 synced (or would sync), 1 a gh call failed or the state
# couldn't be trusted (past the bounds, or --head never showed), 2 usage.
# shellcheck disable=SC2016 # the $vars in single quotes are GraphQL's and jq's
set -euo pipefail

usage() {
	echo "usage: review-state.sh <pr> [--dry-run] [--clean-pass] [--head <sha>] [--from <json>] [--save <json>]" >&2
	exit 2
}

pr="" dry=0 clean=0 from="" save="" want_head=""
while [ $# -gt 0 ]; do
	case $1 in
	--dry-run) dry=1; shift ;;
	--clean-pass) clean=1; shift ;;
	--head) [ $# -ge 2 ] || usage; want_head=$2; shift 2 ;;
	--from) [ $# -ge 2 ] || usage; from=$2; dry=1; shift 2 ;;
	--save) [ $# -ge 2 ] || usage; save=$2; shift 2 ;;
	-*) usage ;;
	*) [ -z "$pr" ] || usage; pr=$1; shift ;;
	esac
done
[[ $pr =~ ^[0-9]+$ ]] || usage
if [ "$clean" -eq 1 ] && [ "$dry" -eq 0 ] && [ -z "$want_head" ]; then
	echo "review-state: --clean-pass needs --head <sha>, the commit the clean /code-review run reviewed" >&2
	exit 2
fi
lib=$(dirname "${BASH_SOURCE[0]}")

# The labels this script owns. Their descriptions point at the one place the
# rules are written down.
label_color() {
	case $1 in
	reviewed-work) echo 0E8A16 ;;
	review-unaddressed) echo D93F0B ;;
	*) echo 1D76DB ;;
	esac
}
label_desc() {
	case $1 in
	reviewed-work) echo "A /review-work verdict is posted (Development Conventions § Review state labels)" ;;
	review-unaddressed) echo "A /code-review pass or /review-work verdict awaits /address-feedback" ;;
	review-passes-left-*) echo "/code-review passes left before make lgtm: ${1#review-passes-left-}" ;;
	esac
}

# Newest items win at every bound (`last:`), except a thread's comments: the
# first one is the thread's opener, which says which pass the thread belongs to.
query='query($owner:String!,$name:String!,$n:Int!){repository(owner:$owner,name:$name){
  defaultBranchRef{name}
  labels(first:100,query:"review"){nodes{name}}
  pullRequest(number:$n){number state headRefOid baseRefName body
    labels(first:100){pageInfo{hasNextPage} nodes{name}}
    closingIssuesReferences(first:20){pageInfo{hasNextPage}
      nodes{number labels(first:100){pageInfo{hasNextPage} nodes{name}}}}
    reviews(last:100){pageInfo{hasPreviousPage} nodes{id state body submittedAt commit{oid}}}
    reviewThreads(last:100){pageInfo{hasPreviousPage} nodes{id isResolved
      comments(first:50){pageInfo{hasNextPage} nodes{createdAt body pullRequestReview{id}}}}}
    comments(last:100){pageInfo{hasPreviousPage} nodes{id createdAt body}}}}}'

# What --head polls until the push shows: only the fields settled reads, so
# the retries don't re-read every review and thread.
head_query='query($owner:String!,$name:String!,$n:Int!){repository(owner:$owner,name:$name){
  defaultBranchRef{name}
  pullRequest(number:$n){headRefOid baseRefName body closingIssuesReferences(first:1){nodes{number}}}}}'

# GitHub's closing keywords followed by an issue number; the number is the
# one capture. Any whitespace may follow the keyword, as GitHub (and
# scripts/issue-block.sh) accept. settled, the body-issue lookup and the
# derivation all use it.
closes_re='(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#([0-9]+)'

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

repo=""
# fetch <query> <out>
fetch() {
	gh api graphql -F owner="${repo%%/*}" -F name="${repo#*/}" -F n="$pr" -f query="$1" >"$2"
}

# named_issues <response>: the numbers <response>'s PR body names with a
# closing keyword (up to 20), looked up into $tmp/named.json, keyed by alias.
# A number that's a PR reads as {}. One with nothing behind it (a typo, a
# deleted or transferred issue) reads as null: GitHub answers it with a
# NOT_FOUND error beside the others' data and gh exits 1, which is no reason
# to abandon the sync. Any other error is a failed call.
named_issues() {
	local named q n rc=0
	echo '{}' >"$tmp/named.json"
	mapfile -t named < <(jq -r --arg re "$closes_re" \
		'[(.data.repository.pullRequest.body // "") | scan($re)[0] | tonumber] | unique | .[:20][]' "$1")
	[ "${#named[@]}" -gt 0 ] || return 0
	q='query($owner:String!,$name:String!){repository(owner:$owner,name:$name){'
	for n in "${named[@]}"; do
		q+=" i$n: issueOrPullRequest(number:$n){... on Issue{number labels(first:100){pageInfo{hasNextPage} nodes{name}}}}"
	done
	q+='}}'
	gh api graphql -F owner="${repo%%/*}" -F name="${repo#*/}" -f query="$q" \
		>"$tmp/named-raw.json" 2>"$tmp/named.err" || rc=$?
	if [ "$rc" -ne 0 ] && ! jq -e '.data.repository != null and all(.errors[]?; .type == "NOT_FOUND")' \
		"$tmp/named-raw.json" >/dev/null 2>&1; then
		cat "$tmp/named.err" >&2
		return 1
	fi
	jq '.data.repository' "$tmp/named-raw.json" >"$tmp/named.json"
}

# settled: whether $tmp/head.json shows the push the caller made (--head). A
# response that isn't a PR settles at once, so the error below reports it. A
# stacked PR never gets closing issues, so it settles on the head alone. On a
# PR into the default branch it waits for closing issues only while the body
# names an issue ($tmp/named.json): a number that's a PR, or nothing, never
# gets linked.
settled() {
	local rc=0
	jq -e --arg h "$want_head" --slurpfile named "$tmp/named.json" '.data.repository as $r | $r.pullRequest as $p
	  | $p == null or ($p.headRefOid == $h
	      and ($p.baseRefName != $r.defaultBranchRef.name
	        or ($p.closingIssuesReferences.nodes | length) > 0
	        or ([$named[0][] | select(.number? != null)] | length) == 0))' \
		"$tmp/head.json" >/dev/null || rc=$?
	[ "$rc" -ne 1 ]
}

if [ -n "$from" ]; then
	cp "$from" "$tmp/data.json"
else
	repo=${GH_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}
	if [ -n "$want_head" ]; then
		fetch "$head_query" "$tmp/head.json"
		named_issues "$tmp/head.json"
		tries=0
		until settled || [ "$tries" -ge 10 ]; do
			sleep 3
			tries=$((tries + 1))
			fetch "$head_query" "$tmp/head.json"
		done
	fi
	fetch "$query" "$tmp/data.json"
	# A stacked PR's closing issues are the ones its body names, kept in the
	# response as .data.bodyIssues so a saved fixture carries them too.
	if jq -e '.data.repository as $r | $r.pullRequest != null
	    and $r.pullRequest.baseRefName != $r.defaultBranchRef.name' "$tmp/data.json" >/dev/null; then
		named_issues "$tmp/data.json"
		jq --slurpfile b "$tmp/named.json" '.data.bodyIssues = $b[0]' "$tmp/data.json" >"$tmp/next.json"
		mv "$tmp/next.json" "$tmp/data.json"
	fi
fi

head=$(jq -r '.data.repository.pullRequest.headRefOid // empty' "$tmp/data.json")
if [ -z "$head" ]; then
	echo "review-state: no PR #$pr in the response" >&2
	exit 1
fi
if [ -n "$want_head" ] && [ "$head" != "$want_head" ]; then
	echo "review-state: GitHub still shows #$pr's head as ${head:0:12}, not ${want_head:0:12}; nothing written." >&2
	exit 1
fi

# The derivation: Development Conventions § Review state labels, rule by rule.
derive() {
	jq -L "$lib" --arg re "$closes_re" '
  include "review-gate";
  .data as $d | $d.repository as $r | $r.pullRequest as $p | $p.headRefOid as $head
  # The issues the PR closes: the ones GitHub links, or on a stacked PR the ones its
  # body names.
  | ($p.baseRefName != $r.defaultBranchRef.name) as $stacked
  | (if $stacked then [($d.bodyIssues // {})[] | select(.number != null)]
     else $p.closingIssuesReferences.nodes end) as $closing
  | [$closing[] | {number, small: any(.labels.nodes[]; .name == "small")}] as $issues
  # Expected passes: 1 when every issue the PR closes is small, else 2.
  | (if ($issues | length) > 0 and all($issues[]; .small) then 1 else 2 end) as $expected
  | [$p.reviewThreads.nodes[] | {resolved: .isResolved, comments: .comments.nodes,
       opener: (.comments.nodes[0].pullRequestReview.id // null)}] as $threads
  # A verdict: a PR comment (or a review) starting with the verdict marker, or
  # a review whose body opens with the verdict block heading, as verdicts were
  # posted before #536 (#516).
  | def verdict: (.body // "") | (capture("\\A\\s*<!-- review-work:verdict (?<v>ready|not-ready) -->")
      // capture("\\A## Review[^\n]*: (?<v>ready|not ready)[ \t]*(\n|\\z)")) | .v | sub(" "; "-");
  [$p.reviews.nodes[] | select(.state != "PENDING")] as $submitted
  # A pass: submitted reviews, not verdicts, that opened threads, with a run
  # of empty-body ones on one commit, each within 60s of the last, merged into
  # one pass (a null in the reduce ends a run). It is addressed when each
  # thread it opened is resolved or its newest comment is an /address-feedback
  # reply.
  | ([$submitted[] | select([verdict] | length == 0) | . as $rv
      | {id, at: .submittedAt, commit: (.commit.oid // ""), blank: ((.body // "") | test("\\S") | not),
         opened: [$threads[] | select(.opener == $rv.id)]}]
     | sort_by(.at)
     | reduce .[] as $rv ([];
         if ($rv.opened | length) == 0 then . + [null]
         elif $rv.blank and length > 0 and .[length - 1] != null and .[length - 1].blank
           and .[length - 1].commit == $rv.commit
           and ($rv.at | fromdate) - (.[length - 1].last | fromdate) <= 60
         then .[:length - 1] + [.[length - 1] | .ids += [$rv.id] | .opened += $rv.opened | .last = $rv.at]
         else . + [$rv + {ids: [$rv.id], last: $rv.at}] end)
     | [.[] | select(. != null)
        | {kind: "review", id, reviews: (.ids | length), at, commit, findings: (.opened | length),
           addressed: all(.opened[]; .resolved or ((.comments | last).body // ""
             | test("\\A\\s*<!-- address-feedback:reply -->")))}]
    ) as $reviews
  | [$p.comments.nodes[] | (.body | capture("\\A\\s*<!-- review-state:clean-pass (?<sha>[0-9a-f]{40}) -->")) as $m
     | {kind: "clean-pass", id, reviews: 1, at: .createdAt, commit: $m.sha, findings: 0, addressed: true}] as $cleans
  | ($reviews + $cleans | sort_by(.at)) as $passes
  | ([$p.comments.nodes[] | verdict as $v | {id, at: .createdAt, ready: ($v == "ready")}]
     + [$submitted[] | verdict as $v | {id, at: .submittedAt, ready: ($v == "ready")}]
     | sort_by(.at)) as $verdicts0
  | [$p.comments.nodes[] | (.body | capture("\\A\\s*<!-- address-feedback:verdict-reply (?<to>\\S+) -->")) as $m
     | {to: $m.to, at: .createdAt}] as $replies
  # A verdict is addressed when it is ready, an /address-feedback reply names
  # it, or a later verdict replaced it.
  | [$verdicts0[] | . as $v
     | . + {addressed: ($v.ready
         or any($replies[]; .to == $v.id and .at > $v.at)
         or any($verdicts0[]; .at > $v.at))}] as $verdicts
  # Passes left: 0 when the latest pass is clean and on the head. With 2
  # expected, a clean pass at an older commit leaves 1 until an addressed pass
  # follows it: the code pushed since is unreviewed, and the passes before the
  # clean one, however many, say nothing about it. With 1 expected, a push
  # after the one pass is not re-reviewed, clean or not. Otherwise expected
  # minus addressed.
  | ($passes | last) as $latest
  | ($cleans | sort_by(.at) | last) as $lastclean
  | (if $latest != null and $latest.kind == "clean-pass" and $latest.commit == $head then 0
     elif $lastclean != null and $expected > 1
     then [1 - ([$passes[] | select(.at > $lastclean.at and .addressed)] | length), 0] | max
     else [$expected - ([$passes[] | select(.addressed)] | length), 0] | max end) as $left
  | (any($passes[]; .addressed | not) or any($verdicts[]; .addressed | not)) as $unaddressed
  | ([if ($verdicts | length) > 0 then "reviewed-work" else empty end,
      if $unaddressed then "review-unaddressed" else empty end,
      "review-passes-left-\($left)"]) as $want
  | [$p.labels.nodes[].name | select(review_label)] as $have
  | {pr: $p.number, state: $p.state, head: $head, expected: $expected, issues: $issues,
     stacked: (if $stacked then $p.baseRefName else null end),
     passes: $passes, verdicts: $verdicts,
     since_verdict: (($verdicts | last) as $lv
       | if $lv == null then null else [$passes[] | select(.at > $lv.at)] | length end),
     left: $left, want: $want, gate: ($want | meets_merge_gate),
     add: ($want - $have), remove: ($have - $want),
     create: [($want - $have)[] | select(. as $l | all($r.labels.nodes[]; .name != $l))],
     # The bounds the read hit: any one makes the state untrustworthy.
     past: [if $p.reviews.pageInfo.hasPreviousPage then "reviews" else empty end,
       if $p.reviewThreads.pageInfo.hasPreviousPage then "review threads" else empty end,
       if any($p.reviewThreads.nodes[]; .comments.pageInfo.hasNextPage // false) then "thread comments" else empty end,
       if $p.comments.pageInfo.hasPreviousPage then "PR comments" else empty end,
       if $p.labels.pageInfo.hasNextPage // false then "PR labels" else empty end,
       if (if $stacked then [($p.body // "") | scan($re)[0]] | unique | length > 20
           else $p.closingIssuesReferences.pageInfo.hasNextPage // false end)
       then "closing issues" else empty end,
       if any($closing[]; .labels.pageInfo.hasNextPage // false) then "closing issue labels" else empty end]}
' "$tmp/data.json" >"$tmp/state.json"
}

# past_bounds <what>: refuse a state worked out from part of the data.
past_bounds() {
	if jq -e '.past | length > 0' "$tmp/state.json" >/dev/null; then
		echo "review-state: #$pr is past the read bounds ($(jq -r '.past | join(", ")' "$tmp/state.json")); $1." >&2
		exit 1
	fi
}

clean_body="<!-- review-state:clean-pass $head -->
Clean \`/code-review\` pass recorded at ${head:0:12}: it found nothing, so it counts as an addressed pass (\`scripts/review-state.sh --clean-pass\`, docs/Development Conventions.md § Review state labels)."
if [ "$clean" -eq 1 ]; then
	if [ "$dry" -eq 1 ]; then
		echo "dry run: would post a clean-pass comment at ${head:0:12}"
		jq --arg body "$clean_body" '.data.repository.pullRequest.comments.nodes +=
			[{id: "dry-run-clean-pass", createdAt: (now | todate), body: $body}]' \
			"$tmp/data.json" >"$tmp/next.json"
		mv "$tmp/next.json" "$tmp/data.json"
	else
		# The marker is permanent, so check the bounds first: a run that then
		# refused to write labels would still have recorded a clean pass.
		derive
		past_bounds "no clean pass posted, no labels written"
		gh api "repos/$repo/issues/$pr/comments" -f body="$clean_body" \
			--jq '{id: .node_id, createdAt: .created_at, body, url: .html_url}' >"$tmp/posted.json"
		echo "review-state: posted $(jq -r .url "$tmp/posted.json")"
		jq --slurpfile c "$tmp/posted.json" '.data.repository.pullRequest.comments.nodes += [$c[0] | del(.url)]' \
			"$tmp/data.json" >"$tmp/next.json"
		mv "$tmp/next.json" "$tmp/data.json"
	fi
fi
[ -z "$save" ] || cp "$tmp/data.json" "$save"
derive

jq -r '
  def short: .[:7];
  def list: if length == 0 then "(none)" else join(" ") end;
  "review-state #\(.pr) (\(.state), head \(.head | short)): expected /code-review passes: \(.expected) (\(
     if (.issues | length) == 0 then "no closing issue"
     else .issues | map("#\(.number)\(if .small then " small" else " not small" end)") | join(", ") end)\(
     if .stacked == null then "" else "; stacked on \(.stacked), so from the body" end))",
  (.passes[] | "  pass     \(.at) \(.kind) \(.id)\(if .reviews > 1 then " +\(.reviews - 1) more" else "" end) at \(.commit | short): \(
     if .findings == 0 then "clean" else "\(.findings) thread\(if .findings == 1 then "" else "s" end)" end), \(
     if .addressed then "addressed" else "unaddressed" end)"),
  (.verdicts[] | "  verdict  \(.at) \(.id): \(if .ready then "ready" else "not-ready" end), \(
     if .addressed then "addressed" else "unaddressed" end)"),
  (if .since_verdict == null then empty
   else "  since-verdict: \(.since_verdict) (/code-review passes after the latest verdict)" end),
  (if (.past | length) == 0 then empty
   else "incomplete: more \(.past | join(", ")) than one call reads; the labels below are not written" end),
  "labels: \(.want | list)",
  "gate: \(if .gate then "met (make lgtm is the operator'"'"'s call)" else "not met" end)",
  "add: \(.add | list)",
  "remove: \(.remove | list)",
  "create: \(.create | list)"
' "$tmp/state.json"

if [ "$dry" -eq 1 ]; then
	echo "dry run: nothing changed."
	exit 0
fi
past_bounds "no labels written"

# gh_unless <pattern> <gh args>: run gh, counting a failure whose output
# matches <pattern> as success. Another sync (make ship's and the workflow's,
# or two PRs' runs) can create or remove the same label between this run's
# read and its write.
gh_unless() {
	local ok=$1 out
	shift
	if ! out=$(gh "$@" 2>&1); then
		grep -q -- "$ok" <<<"$out" && return 0
		printf '%s\n' "$out" >&2
		return 1
	fi
}

mapfile -t create < <(jq -r '.create[]' "$tmp/state.json")
mapfile -t add < <(jq -r '.add[]' "$tmp/state.json")
mapfile -t remove < <(jq -r '.remove[]' "$tmp/state.json")

for l in "${create[@]}"; do
	gh_unless already_exists api "repos/$repo/labels" -f name="$l" -f color="$(label_color "$l")" \
		-f description="$(label_desc "$l")"
done
if [ "${#add[@]}" -gt 0 ]; then
	args=()
	for l in "${add[@]}"; do args+=(-f "labels[]=$l"); done
	gh api "repos/$repo/issues/$pr/labels" "${args[@]}" --silent
fi
for l in "${remove[@]}"; do
	gh_unless "Label does not exist" api -X DELETE "repos/$repo/issues/$pr/labels/$l"
done
if [ "${#add[@]}" -eq 0 ] && [ "${#remove[@]}" -eq 0 ]; then
	echo "review-state: labels already match; nothing changed."
else
	echo "review-state: synced #$pr."
fi
