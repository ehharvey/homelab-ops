#! /bin/bash
# Validates GH issue #119 ("Better PR processes"). The done-when criterion is
# that a multi-commit PR *cannot reach main* — which `make test` cannot prove,
# because every moving part lives on GitHub's side: a workflow, a ruleset, and
# a merge attempt. So this drives the real repo end to end:
#
#   1. .githooks/pre-push refuses a 2-commit push locally.
#   2. The `one-commit` check fails on the resulting PR.
#   3. `gh pr merge` is REFUSED while that check is red — this is the step
#      that proves the required_status_checks ruleset rule is real, rather
#      than merely proving the workflow ran and went red advisorily.
#   4. Squashing to 1 commit flips the check green and the PR mergeable.
#
# It also asserts strict_required_status_checks_policy is false: flipping it
# on would make GitHub's "Update branch" button add a merge commit to the
# branch, silently breaking `one-commit` for everyone (see docs/Development
# Conventions.md "Linting & CI").
#
# The throwaway PR is always closed, never merged. Its branch is prefixed
# `validate-119/` so a leaked one is obvious.
#
# BASE_REF selects what the throwaway branch forks from; it must be a ref that
# already carries .github/workflows/pr-shape.yml, since for pull_request events
# GitHub runs the workflow as defined on the PR *head*. Defaults to
# origin/main, i.e. run this after #119 lands. To validate before merging, set
# BASE_REF to the #119 branch itself.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="a multi-commit PR cannot reach main: hook, check, and ruleset all hold (#119)"
VALIDATE_GROUP="github"
VALIDATE_NEEDS="gh authenticated with repo admin (reads rulesets, opens/closes real PRs)"
VALIDATE_DURATION="~3m"

validate_parse_args "$@"

BASE_REF="${BASE_REF:-origin/main}"
BRANCH="validate-119/$(date +%s)"
RULESET_ID=17956515
REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

# This script is deliberately fail-fast, unlike the rest of the suite: it drives
# one real PR through GitHub and every step depends on the last (open the PR →
# watch one-commit go red → confirm merge is refused → squash → watch it go
# green). There is nothing meaningful to accumulate after a failure, so `fail`
# still aborts. It uses the shared recorders purely so the output and exit codes
# match everything else.
pass() { record_pass "$*"; }
fail() {
	record_fail "$*"
	summary
}

cleanup() {
	set +e
	if [ -n "${PR_OPEN:-}" ]; then
		echo "--- teardown: closing PR and deleting $BRANCH"
		gh pr close "$BRANCH" --delete-branch >/dev/null 2>&1
	fi
	git checkout -q "$ORIG_BRANCH" 2>/dev/null
	git branch -qD "$BRANCH" 2>/dev/null
	git push -q origin --delete "$BRANCH" >/dev/null 2>&1
}

ORIG_BRANCH=$(git branch --show-current)
trap cleanup EXIT

[ -z "$(git status --porcelain)" ] || fail "working tree is dirty; this script switches branches"

# --- Preconditions ---------------------------------------------------------
# Read the ruleset BEFORE opening anything. If required_status_checks isn't
# configured, step 3's `gh pr merge` would succeed and land a junk commit on
# main — so refuse to run at all rather than risk it.
echo "=== checking ruleset $RULESET_ID"
rules=$(gh api "repos/$REPO/rulesets/$RULESET_ID")

echo "$rules" | jq -e '.rules[] | select(.type=="required_status_checks")' >/dev/null \
	|| fail "no required_status_checks rule on main — nothing would block a bad merge. Configure it first."

for ctx in one-commit build-test lint docker-smoke; do
	echo "$rules" | jq -e --arg c "$ctx" \
		'.rules[] | select(.type=="required_status_checks")
		 | .parameters.required_status_checks[] | select(.context==$c)' >/dev/null \
		|| fail "'$ctx' is not a required status check"
done
pass "build-test, lint, docker-smoke, one-commit are all required"

strict=$(echo "$rules" | jq -r '.rules[] | select(.type=="required_status_checks")
	| .parameters.strict_required_status_checks_policy')
[ "$strict" = "false" ] \
	|| fail "strict_required_status_checks_policy is $strict; must be false or 'Update branch' adds a merge commit and breaks one-commit"
pass "strict_required_status_checks_policy is false"

echo "$rules" | jq -e '.rules[] | select(.type=="required_linear_history")' >/dev/null \
	|| fail "required_linear_history is not set"
echo "$rules" | jq -e '.rules[] | select(.type=="pull_request")
	| select(.parameters.allowed_merge_methods == ["rebase"])' >/dev/null \
	|| fail "allowed_merge_methods is not exactly [rebase]"
pass "linear history required; rebase is the only merge method"

# --- 1. pre-push hook refuses 2 commits ------------------------------------
echo "=== building a deliberately 2-commit branch off $BASE_REF"
git fetch -q origin
git checkout -q -b "$BRANCH" "$BASE_REF"

for n in 1 2; do
	echo "validate-119 commit $n" >> .validate-119-scratch
	git add .validate-119-scratch
	git commit -qm "chore: validate-119 throwaway commit $n"
done

[ "$(git config core.hooksPath)" = ".githooks" ] \
	|| fail "core.hooksPath is not .githooks — run 'make hooks' first"

if git push -q -u origin "$BRANCH" 2>/dev/null; then
	fail "pre-push hook allowed a 2-commit push"
fi
pass "pre-push hook refused the 2-commit push"

# --no-verify to get the malformed branch to the server on purpose — the
# server-side check is what's actually under test from here on.
git push -q --no-verify -u origin "$BRANCH"

# --- 2. one-commit check fails on the PR -----------------------------------
gh pr create --head "$BRANCH" --base main \
	--title "DO NOT MERGE: validate #119" \
	--body "Throwaway PR opened by scripts/validate/multi-commit-pr-cannot-reach-main.sh. Closed automatically." >/dev/null
PR_OPEN=1
echo "=== opened throwaway PR, waiting for one-commit to report"

# Polls the check run for a SPECIFIC head SHA, not the PR.
#
# `gh pr checks` is PR-level, and right after a force-push it still reports the
# previous run's terminal state — the run for the new head hasn't registered
# yet. Reading that stale FAILURE as authoritative is what made this script
# fail on a branch that was in fact correctly green (#122). Anchoring to the
# SHA makes "no run yet" distinguishable from "run finished", which is the
# whole difference.
await_check() {
	local sha=$1 want=$2 i line status conclusion
	for i in $(seq 1 60); do
		line=$(gh api "repos/$REPO/commits/$sha/check-runs" \
			--jq '.check_runs[] | select(.name=="one-commit") | "\(.status) \(.conclusion)"' \
			2>/dev/null | head -1)
		status=${line%% *}
		conclusion=${line##* }

		if [ "$status" = "completed" ]; then
			[ "$conclusion" = "$want" ] && return 0
			fail "one-commit on ${sha:0:8} concluded '$conclusion', wanted '$want'"
		fi
		sleep 5
	done
	fail "timed out waiting for one-commit on ${sha:0:8} (last: ${line:-no run yet})"
}

await_check "$(git rev-parse HEAD)" failure
pass "one-commit went red on the 2-commit PR"

# --- 3. the merge is actually blocked --------------------------------------
# The load-bearing assertion: a red advisory check would still merge here.
if gh pr merge "$BRANCH" --rebase 2>/dev/null; then
	fail "PR MERGED WITH A FAILING REQUIRED CHECK — revert it immediately"
fi
pass "gh pr merge was refused while one-commit was red"

# --- 4. squashing to 1 commit clears it ------------------------------------
echo "=== squashing to 1 commit and force-pushing"
git reset -q --soft "$BASE_REF"
git commit -qm "chore: validate-119 throwaway (squashed)"
git push -q --force-with-lease origin "$BRANCH"

await_check "$(git rev-parse HEAD)" success
pass "one-commit went green after the squash"

echo
echo "#119's enforcement is real end to end."
summary
