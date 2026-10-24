#!/usr/bin/env bash
# The data behind /resume (#289): where one issue or PR stands, in one bounded
# report, so a fresh session can pick up interrupted work without re-deriving
# it over a dozen gh and git calls.
#
#   resume.sh 289     # issue #289, or PR #289: GitHub numbers them together
#
# An issue resolves to its branch eharvey/#<n> and that branch's PR (the open
# one, else the newest), falling back to the PRs GitHub links as closing it. A
# PR resolves to its head branch and the issue it closes (closingIssuesReferences,
# else an eharvey/#<n> head branch; titles aren't trusted). Either way the
# branch resolves to the worktree that has it checked out, at any path,
# /run-chain's agent worktrees included.
#
# Sections: the issue (title, state, labels, open native blocked-by links); the
# PR (state, mergeable, checks, commits); the branch and worktree (clean or
# dirty, ahead/behind origin/main); the review (the latest PR comment starting
# `## Review `, its verdict and gap lines, then every comment after it, each
# truncated); and the mechanical gaps, which are .claude/skills/review-work/
# audit.sh's own output, trimmed to its FAIL lines and the issue's Done when.
# Nothing here re-checks what audit.sh checks.
#
# An open issue with no branch and no PR is "not started". A merged or closed
# PR is reported as landed, and audit.sh isn't run on it.
#
# Read-only: it fetches, and writes nothing else. No commits, comments, labels
# or worktrees.
#
# Exit: 0 a report (gaps included); 2 usage, or no such issue or PR.
# shellcheck disable=SC2016 # the $vars in single quotes are GraphQL's
set -euo pipefail

usage() {
	echo "usage: resume.sh <issue | PR number>" >&2
	exit 2
}
[[ $# -eq 1 && $1 =~ ^#?[0-9]+$ ]] || usage
num=${1#\#}

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# bound and worktree_of.
# shellcheck source=SCRIPTDIR/../lib.sh
. "$here/../lib.sh"

cd "$(git rev-parse --show-toplevel)"
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

# Each line cut to at most $1 characters.
clip() { awk -v max="$1" '{ if (length($0) > max) print substr($0, 1, max - 1) "…"; else print }'; }

# GitHub numbers issues and PRs together, and its issues endpoint answers for both.
kind=$(gh api "repos/$repo/issues/$num" --jq 'if .pull_request then "pr" else "issue" end' 2>/dev/null) ||
	{
		echo "resume: no issue or PR #$num" >&2
		exit 2
	}

issue="" pr=""
if [[ $kind == pr ]]; then
	pr=$num
	issue=$(gh pr view "$pr" --json closingIssuesReferences,headRefName \
		--jq '.closingIssuesReferences[0].number // (.headRefName | capture("^eharvey/#(?<n>[0-9]+)$").n) // empty')
else
	issue=$num
fi

issue_json=""
if [[ -n $issue ]]; then
	issue_json=$(gh api graphql -F owner="${repo%/*}" -F name="${repo#*/}" -F n="$issue" -f query='
	query($owner: String!, $name: String!, $n: Int!) {
	  repository(owner: $owner, name: $name) {
	    issue(number: $n) {
	      title state stateReason
	      labels(first: 20) { nodes { name } }
	      blockedBy(first: 50) { nodes { number state title } }
	      closedByPullRequestsReferences(first: 20, includeClosedPrs: true) { nodes { number state } }
	    }
	  }
	}' --jq .data.repository.issue)
fi

# An issue's PR: the open one, else the newest. By head branch first, then by
# GitHub's closing links (a PR from a branch with another name).
if [[ $kind == issue ]]; then
	pick='(map(select(.state == "OPEN")) + sort_by(-.number))[0].number // empty'
	pr=$(gh pr list --head "eharvey/#$issue" --state all --limit 20 --json number,state --jq "$pick")
	[[ -n $pr ]] || pr=$(jq -r ".closedByPullRequestsReferences.nodes | $pick" <<<"$issue_json")
fi

# == Issue ==
if [[ -n $issue ]]; then
	jq -r --arg n "$issue" '
		def cut($m): if length > $m then .[:$m - 1] + "…" else . end;
		"== Issue #\($n) [\(.state | ascii_downcase)\(if .state == "CLOSED" and .stateReason then ", " + (.stateReason | ascii_downcase) else "" end)] \(.title | cut(80)) ==",
		"labels:   \([.labels.nodes[].name] | if length == 0 then "none" else join(", ") end)",
		"blockers: \([.blockedBy.nodes[] | select(.state == "OPEN") | "#\(.number) \(.title | cut(40))"] | if length == 0 then "none open" else join("; ") end)"
	' <<<"$issue_json"
else
	echo "== Issue: none found (PR #$pr closes no issue, and its branch isn't eharvey/#<n>) =="
fi

# The PR's facts, and its branch.
branch="eharvey/#$issue" state=""
if [[ -n $pr ]]; then
	info=$(gh pr view "$pr" --json state,headRefName,baseRefName,mergeable,mergedAt,closedAt,mergeCommit,commits,title \
		--jq '[.state, .headRefName, .baseRefName, .mergeable, (.mergedAt // "-"), (.closedAt // "-"),
		       (.mergeCommit.oid // "-" | .[:7]), (.commits | length), (.title | .[:70])] | @tsv')
	IFS=$'\t' read -r state branch base mergeable merged_at closed_at merge_sha commits title <<<"$info"
fi

# The branch's head: the local branch, else origin's.
git fetch -q origin main 2>/dev/null || echo "(fetch of main failed; ahead/behind may be stale)"
head=""
if git rev-parse -q --verify "refs/heads/$branch^{commit}" >/dev/null; then
	head=$(git rev-parse "refs/heads/$branch")
elif git fetch -q origin "refs/heads/$branch" 2>/dev/null; then
	head=$(git rev-parse FETCH_HEAD)
fi

if [[ -z $pr && -z $head ]]; then
	echo
	if [[ $(jq -r .state <<<"$issue_json") == OPEN ]]; then
		echo "not started: no branch $branch and no PR."
		echo "start: make wt N=$issue, then a decisions-scout brief (CLAUDE.md § Subagents). /next for the wider queue."
	else
		echo "no branch $branch and no PR; the issue is closed."
	fi
	exit 0
fi

# == PR ==
echo
if [[ -z $pr ]]; then
	echo "== PR: none =="
	echo "not shipped, so no CI checks; /ship-issue ships it once the gaps below are closed"
else
	echo "== PR #$pr [${state,,}] $title =="
	case $state in
	MERGED) echo "already landed: merged ${merged_at%%T*} as $merge_sha; nothing to resume" ;;
	CLOSED) echo "closed unmerged ${closed_at%%T*}; nothing to resume" ;;
	*)
		# gh pr checks exits non-zero while a check fails or is pending; its output is what counts.
		checks=$(gh pr checks "$pr" --json name,bucket 2>/dev/null || true)
		summary=$(jq -r '
			def n($b): [.[] | select(.bucket | IN($b[]))];
			if length == 0 then "none reported" else
			"\(n(["pass", "skipping"]) | length) pass, \(n(["pending"]) | length) pending, \(n(["fail", "cancel"]) | length) fail\(n(["fail", "cancel"]) | map(.name) | if length > 0 then " (" + join(", ") + ")" else "" end)"
			end' <<<"${checks:-[]}" 2>/dev/null || echo "none reported")
		case $mergeable in MERGEABLE) mergeable=yes ;; CONFLICTING) mergeable="no, conflicts" ;; *) mergeable=${mergeable,,} ;; esac
		echo "base $base, mergeable: $mergeable, $commits commit(s), checks: $summary"
		;;
	esac
fi

# == Branch and worktree ==
echo
echo "== Branch $branch =="
wt=$(worktree_of "$branch")
if [[ -z $wt ]]; then
	echo "worktree: none"
elif dirty=$(git -C "$wt" status --porcelain | wc -l) && [[ $dirty -eq 0 ]]; then
	echo "worktree: $wt (clean)"
else
	echo "worktree: $wt (dirty: $dirty uncommitted path(s))"
fi
if [[ $state == MERGED || $state == CLOSED ]]; then
	# main is rebase-only, so a landed branch still counts as ahead: don't print it.
	[[ -z $wt ]] || echo "make wt-gc removes the worktree once nothing in it can be lost"
	exit 0
elif [[ -z $head ]]; then
	echo "commits:  no $branch locally or on origin"
else
	src="local branch"
	git rev-parse -q --verify "refs/heads/$branch" >/dev/null || src="no local branch; origin's"
	counts=$(git rev-list --left-right --count "origin/main...$head" 2>/dev/null) || counts="? ?"
	read -r behind ahead <<<"$counts"
	echo "vs main:  $ahead ahead, $behind behind origin/main @ $(git rev-parse --short origin/main) ($src)"
fi

# == Review ==
if [[ -n $pr ]]; then
	echo
	echo "== Review =="
	# The latest comment starting "## Review " (its body as one JSON line), then
	# every comment after it as author, date and the text cut to ~3 lines.
	reviews=$(gh pr view "$pr" --json comments --jq '
		.comments as $c
		| ([$c | to_entries[] | select(.value.body | test("^## Review ")) | .key] | last) as $i
		| if $i == null then "none" else
			({who: $c[$i].author.login, when: $c[$i].createdAt[:10], body: $c[$i].body} | @json),
			($c[$i + 1:][] | [.author.login, .createdAt[:10],
				(.body | gsub("\\s+"; " ") | if length > 270 then .[:269] + "…" else . end)] | @tsv)
		  end')
	if [[ $reviews == none ]]; then
		echo "no review yet"
	else
		latest=$(head -1 <<<"$reviews")
		body=$(jq -r .body <<<"$latest" | tr -d '\r')
		echo "$(head -1 <<<"$body" | sed 's/^## //') ($(jq -r '"\(.who) \(.when)"' <<<"$latest"))" | clip 110
		# The gap lines: the list under "gaps:", without continuation lines.
		awk '
			!p && /^(\*\*)?gaps:/ { p = 1; sub(/^(\*\*)?gaps:(\*\*)?[[:space:]]*/, ""); if ($0 != "") print "- " $0; next }
			p && /^- / { print; next }
			p && (/^[[:space:]]/ || NF == 0) { next }
			p { exit }' <<<"$body" | clip 110 | bound 8
		after=$(tail -n +2 <<<"$reviews")
		if [[ -z $after ]]; then
			echo "no comments after it"
		else
			echo "comments after it: $(wc -l <<<"$after")"
			while IFS=$'\t' read -r who when text; do
				printf '%s %s: %s\n' "$who" "$when" "$text" | fold -s -w 108 | sed '1s/^/  /; 2,$s/^/    /; s/ *$//'
			done <<<"$after"
		fi
	fi
fi

# == Mechanical gaps: audit.sh as is, trimmed ==
target=${pr:-$branch}
rc=0
out=$("$here/../review-work/audit.sh" "$target" 2>&1) || rc=$?
echo
echo "== Mechanical gaps (audit.sh $target, exit $rc) =="
awk '
	/^== / { sec = $0; title = 1; next }
	/^(REFUSED|audit:) / { last = $0; next }
	sec == "== Checks ==" && /^ok / { ok++; next }
	sec == "== Checks ==" && /^(FAIL|n\/a) / { print; cont = 1; next }
	sec == "== Checks ==" && cont && /^        / { print; next }
	{ cont = 0 }
	sec ~ /^== Issue #[0-9]+: Done when ==$/ && NF {
		if (title) { title = 0; next }
		if (++dw <= 10) done[dw] = $0
	}
	END {
		if (ok) printf "ok    %d check(s)\n", ok
		if (dw) {
			print "done when (match each against the Test plan):"
			for (i = 1; i <= dw && i <= 10; i++) print "  " done[i]
			if (dw > 10) printf "  … %d more lines\n", dw - 10
		}
		if (last != "") print last
	}' <<<"$out" | clip 110
