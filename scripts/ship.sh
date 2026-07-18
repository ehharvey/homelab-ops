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
# Conventions.md). Nothing gets retyped into a web form.
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

git push -u origin "$branch"

# Re-running ship on an existing PR (e.g. after a force-push) must not fail.
if gh pr view --json number >/dev/null 2>&1; then
	echo "ship: PR already open for $branch."
else
	gh pr create --fill
fi

gh pr view --json number,url --jq '"ship: #\(.number) open — \(.url)"'
echo "ship: review it, then \`make lgtm\` to merge."
