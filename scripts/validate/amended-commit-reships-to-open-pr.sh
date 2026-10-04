#! /bin/bash
# Validates GH issue #273 (`make ship` re-ships an amended commit). The review
# loop under the one-commit rule is "amend, `make ship`", so scripts/ship.sh
# must force-push a rewritten commit to an open PR, yet never overwrite work
# that reached the branch after it read the PR. `make test` can't prove that:
# every moving part is a real push to GitHub and a real PR. So this drives
# scripts/ship.sh (this checkout's copy) against the real repo:
#
#   1. No PR open: ship pushes and opens one, as it always did.
#   2. PR open, commit amended: ship force-pushes with a lease on the PR head,
#      and prints the same PR rather than opening a second.
#   3. PR open, but the branch moves after ship read the PR head: ship refuses
#      and the intruding commit survives.
#   4. PR closed: ship refuses rather than pushing to a dead PR.
#
# Step 3 needs a push to land *between* ship's `gh pr view` and its `git push`.
# Pushing before ship runs would not test the lease at all: ship would read the
# new head and lease on that. So for that one run a `gh` shim on PATH passes
# every call to the real gh, and right after the head lookup pushes a commit to
# the branch from a second, detached worktree, standing in for another session.
#
# Like multi-commit-pr-cannot-reach-main.sh, this is fail-fast: one real PR,
# each step building on the last. The throwaway PR is always closed, never
# merged; its branch is prefixed `validate-273/` so a leaked one is obvious, and
# its commit subject carries [skip ci] so it doesn't spend a CI run. All work
# happens in temporary worktrees, so the caller's checkout is never switched.
#
# BASE_REF selects what the throwaway branch forks from (default origin/main).
# The ship.sh under test is always this checkout's, whatever BASE_REF is.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="make ship force-pushes an amended commit to its open PR, leased on the PR head, and refuses to overwrite a newer push (#273)"
VALIDATE_GROUP="github"
VALIDATE_NEEDS="gh authenticated with push access (opens and closes a real PR)"
VALIDATE_DURATION="~1m"

validate_parse_args "$@"

require_cmd gh git
check_prereqs

BASE_REF="${BASE_REF:-origin/main}"
BRANCH="validate-273/$(date +%s)"
SHIP="$ROOT_DIR/scripts/ship.sh"
REAL_GH=$(command -v gh)
WORK_DIR=$(mktemp -d)
WT="$WORK_DIR/ship"         # the throwaway branch; ship runs here
INTRUDER="$WORK_DIR/other"  # detached; pushes behind ship's back in step 3
SHIM="$WORK_DIR/shim"

# Fail-fast, as multi-commit-pr-cannot-reach-main.sh: `fail` aborts, and the
# shared recorders only keep the output and exit codes uniform.
pass() { record_pass "$*"; }
fail() {
	record_fail "$*"
	summary
}

cleanup() {
	set +e
	cd "$ROOT_DIR" || return
	if [ -n "${PR_OPEN:-}" ]; then
		echo "--- teardown: closing PR and deleting $BRANCH"
		gh pr close "$BRANCH" >/dev/null 2>&1
	fi
	git push -q --no-verify origin --delete "$BRANCH" >/dev/null 2>&1
	git worktree remove -f "$WT" 2>/dev/null
	git worktree remove -f "$INTRUDER" 2>/dev/null
	git branch -qD "$BRANCH" 2>/dev/null
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

gh auth status >/dev/null 2>&1 || fail "gh is not authenticated"
# Ship's pushes must pass the real pre-push hook, so it has to be installed.
# core.hooksPath may be relative (`make hooks`) or absolute; ask git where the
# hook it will run lives rather than matching the setting's spelling.
pre_push=$(git rev-parse --path-format=absolute --git-path hooks/pre-push)
[ "$(basename "$(dirname "$pre_push")")" = ".githooks" ] && [ -x "$pre_push" ] \
	|| fail "pre-push hook is $pre_push, not .githooks/pre-push — run 'make hooks' first"

remote_head() { git ls-remote origin "refs/heads/$BRANCH" | cut -f1; }

# The PR's headRefOid trails a push by a moment; poll rather than race it.
await_pr_head() {
	local want=$1 got i
	for i in $(seq 1 20); do
		got=$(gh pr view "$BRANCH" --json headRefOid --jq .headRefOid 2>/dev/null || true)
		[ "$got" = "$want" ] && return 0
		sleep 2
	done
	fail "PR head is ${got:-unknown}, wanted ${want:0:12} (waited 40s)"
}

# --- 1. no PR: ship pushes and opens one -----------------------------------
echo "=== building a 1-commit branch off $BASE_REF in a temporary worktree"
git fetch -q origin
git worktree add -q --no-track -b "$BRANCH" "$WT" "$BASE_REF"
cd "$WT"

echo "validate-273 v1" >.validate-273-scratch
git add .validate-273-scratch
git commit -qm "DO NOT MERGE: validate #273 [skip ci]" \
	-m "Throwaway PR opened by scripts/validate/amended-commit-reships-to-open-pr.sh. Closed automatically."
v1=$(git rev-parse HEAD)

PR_OPEN=1
out=$("$SHIP" 2>&1) || fail "ship failed with no PR open: $out"
pr=$(gh pr view "$BRANCH" --json number,state --jq '"\(.number) \(.state)"') \
	|| fail "ship reported success but no PR exists for $BRANCH"
number=${pr%% *}
[ "${pr##* }" = "OPEN" ] || fail "PR #$number is ${pr##* }, wanted OPEN"
[ "$(remote_head)" = "$v1" ] || fail "origin/$BRANCH is not the shipped commit"
await_pr_head "$v1"
grep -qF "ship: #$number open" <<<"$out" || fail "ship did not print the new PR: $out"
pass "no PR: ship pushed the commit and opened PR #$number"

# --- 2. amended commit: ship force-pushes to the same PR -------------------
echo "=== amending the commit and shipping again"
echo "validate-273 v2" >>.validate-273-scratch
git add .validate-273-scratch
git commit -q --amend --no-edit
v2=$(git rev-parse HEAD)

out=$("$SHIP" 2>&1) || fail "ship failed to re-ship an amended commit: $out"
[ "$(remote_head)" = "$v2" ] || fail "origin/$BRANCH is not the amended commit"
await_pr_head "$v2"
grep -qF "ship: #$number open" <<<"$out" || fail "ship did not print the existing PR #$number: $out"
count=$(gh pr list --head "$BRANCH" --state all --json number --jq length)
[ "$count" = "1" ] || fail "$count PRs exist for $BRANCH; ship opened another"
pass "amended commit: ship force-pushed to the open PR #$number and printed it"

# --- 3. branch moved after ship read the PR head: ship refuses -------------
echo "=== amending again, with a push landing between ship's PR lookup and its push"
git worktree add -q --detach "$INTRUDER" "$v2"
echo "validate-273 intruder" >"$INTRUDER/.validate-273-intruder"
git -C "$INTRUDER" add .validate-273-intruder
git -C "$INTRUDER" commit -qm "validate #273: a push ship hasn't seen [skip ci]"
intruder=$(git -C "$INTRUDER" rev-parse HEAD)

mkdir "$SHIM"
cat >"$SHIM/gh" <<EOF
#!/usr/bin/env bash
# Generated by amended-commit-reships-to-open-pr.sh: the real gh, plus a push
# to $BRANCH right after ship.sh reads the PR head.
"$REAL_GH" "\$@" || exit
case " \$* " in
*headRefOid*)
	git -C "$INTRUDER" push -q --no-verify origin "HEAD:refs/heads/$BRANCH" >&2
	touch "$WORK_DIR/intruded"
	;;
esac
EOF
chmod +x "$SHIM/gh"

echo "validate-273 v3" >>.validate-273-scratch
git add .validate-273-scratch
git commit -q --amend --no-edit

if out=$(PATH="$SHIM:$PATH" "$SHIP" 2>&1); then
	fail "ship succeeded over a push it had not seen: $out"
fi
[ -e "$WORK_DIR/intruded" ] || fail "the shim never pushed; ship didn't look the PR up as expected: $out"
[ "$(remote_head)" = "$intruder" ] || fail "origin/$BRANCH lost the intruding commit"
grep -qF "moved past the PR head" <<<"$out" || fail "ship refused, but not for the lease: $out"
pass "branch moved past the PR head: ship refused and the newer commit survived"

# --- 4. closed PR: ship refuses --------------------------------------------
echo "=== closing the PR and shipping again"
gh pr close "$BRANCH" >/dev/null 2>&1 || fail "could not close PR #$number"
PR_OPEN=
if out=$("$SHIP" 2>&1); then
	fail "ship succeeded against a closed PR: $out"
fi
grep -qF "is CLOSED" <<<"$out" || fail "ship refused, but not because the PR is closed: $out"
[ "$(remote_head)" = "$intruder" ] || fail "ship pushed to the closed PR's branch"
pass "closed PR: ship refused and pushed nothing"

echo
echo "#273's re-ship path holds end to end."
summary
