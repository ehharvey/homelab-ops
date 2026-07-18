#!/usr/bin/env bash
# Approve a PR opened by `make ship` and let it merge (#125).
#
# Enables auto-merge rather than merging outright, so the required checks
# (build-test, lint, docker-smoke, one-commit) still have to pass — this says
# "I'm happy with the diff", not "merge regardless".
#
# Deliberately a local command rather than an `issue_comment` workflow reacting
# to an "lgtm" comment: a merge enabled with GITHUB_TOKEN is attributed to
# github-actions[bot], and GITHUB_TOKEN-driven pushes do not trigger further
# workflow runs — which would silently stop sync-wiki.yml from mirroring
# docs/** to the wiki. Running it from a local `gh` keeps the merge attributed
# to a real user, so push-triggered workflows on main keep firing.
#
# Usage:
#   make lgtm            # the PR for the current branch
#   make lgtm PR=123     # a PR opened some other way
set -euo pipefail

pr="${1:-}"

if [ -z "$pr" ]; then
	branch=$(git branch --show-current)
	if [ "$branch" = "main" ]; then
		echo "lgtm: on main — check out the PR's branch, or pass PR=<number>." >&2
		exit 1
	fi
	pr=$(gh pr view --json number --jq .number 2>/dev/null) || {
		echo "lgtm: no PR open for $branch — run \`make ship\` first." >&2
		exit 1
	}
fi

state=$(gh pr view "$pr" --json state --jq .state)
if [ "$state" != "OPEN" ]; then
	echo "lgtm: PR #$pr is $state, nothing to do." >&2
	exit 1
fi

gh pr merge "$pr" --auto --rebase --delete-branch

gh pr view "$pr" --json number,url --jq '"lgtm: #\(.number) queued for auto-merge — \(.url)"'
