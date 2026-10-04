#!/usr/bin/env bash
# Push a finished commit and open its PR (#119). Stops there — `make lgtm`
# merges (#125), so CI runs while you actually read the diff.
#
# The PR is a CI gate, not a review bottleneck: main requires 0 approvals, and
# a real approval gate isn't available to a solo dev anyway (GitHub won't let
# you approve your own PR, so requiring 1 review would deadlock everything).
# The ship/lgtm split is a deliberate speed bump, not an enforced control.
#
# `gh pr create --fill` takes the PR title from the commit subject and the PR
# body verbatim from the commit message body — which is why the ## Plan and
# ## Test plan sections live in the commit message (see docs/Development
# Conventions.md). Nothing gets retyped into a web form. --fill copies only at
# creation: re-shipping an amended commit to an open PR (#273) pushes it and
# leaves the PR's title and body alone (/address-feedback syncs them).
set -euo pipefail

branch=$(git branch --show-current)

if [ "$branch" = "main" ]; then
	echo "ship: on main — branch as eharvey/#<issue> first." >&2
	exit 1
fi

if [ -n "$(git status --porcelain)" ]; then
	echo "ship: working tree is dirty; commit or stash first." >&2
	git status --short >&2
	exit 1
fi

git fetch -q origin main
ahead=$(git rev-list --count FETCH_HEAD..HEAD)

if [ "$ahead" -ne 1 ]; then
	echo "ship: branch is $ahead commits ahead of main; main takes exactly 1." >&2
	[ "$ahead" -gt 1 ] && echo "      git reset --soft FETCH_HEAD && git commit -c HEAD@{1}" >&2
	exit 1
fi

# Look the PR up BEFORE pushing (#273): an amended commit needs a force-push,
# which is only safe against a PR whose head we know. `gh pr view` also returns
# a closed or merged PR for the branch, so only OPEN counts as "already open".
# A failed lookup is treated as "no PR", as before: the plain push below then
# refuses a rewritten commit as non-fast-forward rather than overwriting it.
pr=$(gh pr view --json state,headRefOid --jq '"\(.state) \(.headRefOid)"' 2>/dev/null || true)
state=${pr%% *}
head_oid=${pr##* }

case "$state" in
"")
	git push -u origin "$branch"
	gh pr create --fill
	;;
OPEN)
	# Re-ship to the open PR. The lease names the PR's head SHA explicitly,
	# not a bare --force-with-lease: that compares against origin/<branch>,
	# which a fresh `make wt` worktree may lack and a background fetch may
	# have advanced. If anything reached the branch after the head read
	# above, the push refuses instead of overwriting it.
	if ! git push -u --force-with-lease="refs/heads/$branch:$head_oid" origin "$branch"; then
		echo "ship: push refused — origin/$branch moved past the PR head ${head_oid:0:12}." >&2
		echo "      Fetch it, fold anything new into your commit, and ship again." >&2
		exit 1
	fi
	echo "ship: re-shipped to the open PR for $branch."
	;;
*)
	echo "ship: the PR for $branch is $state, not open; nothing pushed." >&2
	echo "      Reopen it, or branch afresh for new work." >&2
	exit 1
	;;
esac

gh pr view --json number,url --jq '"ship: #\(.number) open — \(.url)"'
echo "ship: review it, then \`make lgtm\` to merge."
