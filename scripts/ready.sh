#!/usr/bin/env bash
# Move issues to Ready in one step (#385): add `Ready` and remove `proposed` in
# a single `gh issue edit`, so the two labels can't drift apart (on 2026-10-25
# four issues got `Ready` but kept `proposed`, and /run-chain skipped them).
#
# This is the one way to triage, from the shell or from /refine-issue on the
# operator's yes. Triage in the web UI is covered by
# .github/workflows/issue-triage.yml instead, which drops `proposed` after the
# fact.
#
# It refuses an issue that /next would flag as an inconsistency once Ready, and
# that /run-chain would skip, leaving its labels untouched:
# - an open native blocked-by link, naming the blockers. They come from the
#   same GraphQL `blockedBy` connection as .claude/skills/next/next.sh.
# - a Done-when that names no validate script or real-run check, by the check
#   next.sh and queue.sh share (.claude/skills/file-task/done-when.jq, #410).
#
# One result line per issue on stdout (refusals on stderr); exits 1 if any
# issue was refused.
#
# Usage:
#   make ready N=310
#   make ready N="310 311"
# shellcheck disable=SC2016 # the $vars in single quotes are GraphQL's
set -euo pipefail

skills=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.claude/skills" && pwd)

if [ "$#" -eq 0 ]; then
	echo 'ready: no issue given — make ready N=<n>, or N="<a> <b>" for several.' >&2
	exit 2
fi

repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

# Prints "<state>␟<labels>␟<open blockers>␟<Done-when gap, or empty>", split
# on the unit separator (\x1f): unlike a tab it isn't IFS whitespace, so `read`
# keeps an empty field instead of merging it into the next.
issue_info() {
	gh api graphql -F owner="${repo%/*}" -F name="${repo#*/}" -F n="$1" -f query='
query($owner: String!, $name: String!, $n: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $n) {
      state body
      labels(first: 50) { nodes { name } }
      blockedBy(first: 50) { nodes { number state stateReason } }
    }
  }
}' | jq -r -L "$skills" 'include "file-task/done-when";
	.data.repository.issue // empty
	| [.state,
	   ([.labels.nodes[].name] | join(", ")),
	   ([.blockedBy.nodes[] | select(.state == "OPEN") | "#\(.number)"] | join(" ")),
	   (if .body | done_when_ok then "" else done_when_gap end)]
	| join("\u001f")'
}

failed=0
for n in "$@"; do
	n=${n#\#}
	case "$n" in
	'' | *[!0-9]*)
		echo "ready: '$n' isn't an issue number." >&2
		failed=1
		continue
		;;
	esac

	# gh's and jq's own errors pass through to stderr: a PR number, an expired
	# token and a broken done-when.jq all fail here, and each says which it is.
	if ! info=$(issue_info "$n"); then
		echo "ready: #$n: couldn't look it up in $repo (error above); labels left alone." >&2
		failed=1
		continue
	fi
	if [ -z "$info" ]; then
		echo "ready: #$n: not an issue in $repo." >&2
		failed=1
		continue
	fi
	IFS=$'\x1f' read -r state labels blockers gap <<<"$info"

	if [ "$state" != "OPEN" ]; then
		echo "ready: #$n is $state; labels left alone." >&2
		failed=1
		continue
	fi
	if [ -n "$blockers" ]; then
		echo "ready: #$n refused: blocked by open $blockers; labels left alone." >&2
		failed=1
		continue
	fi
	if [ -n "$gap" ]; then
		echo "ready: #$n refused: $gap (/file-task Ready criteria); labels left alone." >&2
		failed=1
		continue
	fi

	# The revert undoes only what this edit changes, from the labels as they
	# were: an issue that already had Ready keeps it.
	undo=
	case ", $labels, " in
	*", Ready, "*) ;;
	*) undo=" --remove-label Ready" ;;
	esac
	case ", $labels, " in
	*", proposed, "*) undo="$undo --add-label proposed" ;;
	esac
	if [ -z "$undo" ]; then
		echo "ready: #$n labels: $labels — already Ready, nothing changed"
		continue
	fi

	gh issue edit "$n" --add-label Ready --remove-label proposed >/dev/null

	now=$(gh issue view "$n" --json labels --jq '[.labels[].name] | join(", ")')
	echo "ready: #$n labels: $now — revert: gh issue edit $n$undo"
done

exit "$failed"
