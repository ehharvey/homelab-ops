#! /bin/bash
# Validates GH issue #450 (`make ship` copies the issue's details into the
# commit). scripts/ship.sh (this checkout's copy) must replace the subject with
# the issue's stripped title and add scripts/issue-block.sh's block (scope,
# Done-when, Closes) after the author's text and before the trailers, keeping
# ## Plan and ## Test plan byte-for-byte. `make test` can't prove that: it is a
# real commit amended from a real issue, pushed to a real PR that
# `gh pr create --fill` titles from the rewritten subject. So this drives ship
# against the real repo, on a throwaway branch with SHIP_ISSUE set to a closed
# issue (FIXTURE_ISSUE, default #273), whose body no longer changes:
#
#   1. First ship: the subject becomes the issue's title (and so the PR's), one
#      block sits before the trailer paragraph, and the author's text is kept.
#   2. Second ship, nothing changed: no amend, the SHA is kept.
#   3. The author edits the Plan and re-ships: still exactly one block, and
#      the edit is kept.
#   4. SHIP_ISSUE unset: the branch name carries no #<n>, so ship says so and
#      leaves the message exactly as written.
#   5. SHIP_ISSUE names no issue: ship refuses and pushes nothing.
#   6. Skip directives (`[skip ci]` and the rest) in an issue's title and body
#      come out defused, so a copied mention can't skip the PR's checks.
#
# Like amended-commit-reships-to-open-pr.sh, this is fail-fast: one real PR,
# each step building on the last. The throwaway PR is always closed, never
# merged; its branch is prefixed `validate-450/` so a leaked one is obvious,
# and `[skip ci]` sits in the commit body (ship replaces the subject) so it
# doesn't spend a CI run. All work happens in a temporary worktree, so the
# caller's checkout is never switched.
#
# BASE_REF selects what the throwaway branch forks from (default origin/main).
# The ship.sh under test is always this checkout's, whatever BASE_REF is.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="make ship replaces the subject with the issue's title and adds its scope/Done-when/Closes block before the trailers, once, keeping the author's text, and leaves a branch with no issue alone (#450)"
VALIDATE_GROUP="github"
VALIDATE_NEEDS="gh authenticated with push access (opens and closes a real PR)"
VALIDATE_DURATION="~1m"

validate_parse_args "$@"

require_cmd gh git
check_prereqs

BASE_REF="${BASE_REF:-origin/main}"
FIXTURE_ISSUE="${FIXTURE_ISSUE:-273}"
BRANCH="validate-450/$(date +%s)"
SHIP="$ROOT_DIR/scripts/ship.sh"
BLOCK="$ROOT_DIR/scripts/issue-block.sh"
WORK_DIR=$(mktemp -d)
WT="$WORK_DIR/ship"
MSG="$WORK_DIR/msg"
unset SHIP_ISSUE

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
	git branch -qD "$BRANCH" 2>/dev/null
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

gh auth status >/dev/null 2>&1 || fail "gh is not authenticated"
pre_push=$(git rev-parse --path-format=absolute --git-path hooks/pre-push)
[ "$(basename "$(dirname "$pre_push")")" = ".githooks" ] && [ -x "$pre_push" ] \
	|| fail "pre-push hook is $pre_push, not .githooks/pre-push — run 'make hooks' first"
[ "$(gh issue view "$FIXTURE_ISSUE" --json state --jq .state 2>/dev/null)" = "CLOSED" ] \
	|| fail "fixture issue #$FIXTURE_ISSUE isn't a closed issue; set FIXTURE_ISSUE to one"

remote_head() { git ls-remote origin "refs/heads/$BRANCH" | cut -f1; }
message() { git log -1 --format=%B; }
starts() { grep -c "^<!-- ship:issue #$FIXTURE_ISSUE -->\$" <<<"$1" || true; }

await_pr_head() {
	local want=$1 got
	for _ in $(seq 1 20); do
		got=$(gh pr view "$BRANCH" --json headRefOid --jq .headRefOid 2>/dev/null || true)
		[ "$got" = "$want" ] && return 0
		sleep 2
	done
	fail "PR head is ${got:-unknown}, wanted ${want:0:12} (waited 40s)"
}

# The author's text, exactly as written, and the trailer paragraph after it.
AUTHOR=$(printf '%s\n' \
	'## Plan' '' \
	'Throwaway PR opened by scripts/validate/ship-copies-issue-details-into-commit.sh.' \
	'Closed automatically.  Two spaces, a `## Inner` mention and a trailing list:' '' \
	'- one' '  - nested' '' \
	'## Test plan' '' \
	'- [x] ship keeps this byte-for-byte' '' \
	'[skip ci]')
TRAILER="Co-Authored-By: validate-450 <validate-450@example.invalid>"
SUBJECT="feat(validate): DO NOT MERGE, validate #450"

# Render the block the way ship does, to know the exact message it must write.
details=$("$BLOCK" "$FIXTURE_ISSUE") || fail "issue-block.sh couldn't render #$FIXTURE_ISSUE"
title=${details%%$'\n'*}
block=${details#*$'\n'}
want="$title"$'\n\n'"$AUTHOR"$'\n\n'"$block"$'\n\n'"$TRAILER"

# --- 1. first ship: subject replaced, one block before the trailers --------
echo "=== building a 1-commit branch off $BASE_REF in a temporary worktree"
git fetch -q origin
git worktree add -q --no-track -b "$BRANCH" "$WT" "$BASE_REF"
cd "$WT"
echo "validate-450 v1" >.validate-450-scratch
git add .validate-450-scratch
printf '%s\n\n%s\n\n%s\n' "$SUBJECT" "$AUTHOR" "$TRAILER" >"$MSG"
git commit -q -F "$MSG"
original=$(message)

PR_OPEN=1
out=$(SHIP_ISSUE=$FIXTURE_ISSUE "$SHIP" 2>&1) || fail "first ship failed: $out"
grep -qF "ship: copied #$FIXTURE_ISSUE's details into the commit." <<<"$out" \
	|| fail "ship didn't say it copied #$FIXTURE_ISSUE's details: $out"
msg=$(message)
[ "${msg%%$'\n'*}" = "$title" ] || fail "subject is '${msg%%$'\n'*}', wanted the issue title '$title'"
[ "$(starts "$msg")" = 1 ] || fail "$(starts "$msg") blocks in the message, wanted 1"
[[ $msg == *$'\n\n'"$AUTHOR"$'\n\n'"<!-- ship:issue #$FIXTURE_ISSUE -->"* ]] \
	|| fail "the author's ## Plan / ## Test plan text isn't kept byte-for-byte ahead of the block"
[[ $msg == *$'\n'"<!-- /ship:issue -->"$'\n\n'"$TRAILER" ]] \
	|| fail "the block doesn't end right before the trailer paragraph"
[ "$msg" = "$want" ] || fail "message differs from the expected one: $(diff <(echo "$want") <(echo "$msg") | head -20)"
v1=$(git rev-parse HEAD)
pr=$(gh pr view "$BRANCH" --json number,title --jq '"\(.number) \(.title)"') \
	|| fail "ship reported success but no PR exists for $BRANCH"
number=${pr%% *}
[ "${pr#* }" = "$title" ] || fail "PR #$number is titled '${pr#* }', wanted '$title'"
[ "$(remote_head)" = "$v1" ] || fail "origin/$BRANCH is not the amended commit"
await_pr_head "$v1"
pass "first ship: subject and PR #$number's title are #$FIXTURE_ISSUE's, one block before the trailers, author text kept"

# --- 2. nothing changed: no amend, SHA kept ---------------------------------
echo "=== shipping again with nothing changed"
out=$(SHIP_ISSUE=$FIXTURE_ISSUE "$SHIP" 2>&1) || fail "second ship failed: $out"
[ "$(git rev-parse HEAD)" = "$v1" ] || fail "an unchanged re-ship rewrote the commit"
! grep -qF "copied" <<<"$out" || fail "ship claimed to copy into an unchanged commit: $out"
[ "$(remote_head)" = "$v1" ] || fail "origin/$BRANCH moved on an unchanged re-ship"
pass "unchanged re-ship: no amend, SHA ${v1:0:12} kept"

# --- 3. the author edits the Plan: still one block --------------------------
echo "=== editing the Plan and shipping again"
message | sed 's/^Closed automatically\.  Two spaces/Closed automatically, edited.  Two spaces/' >"$MSG"
git commit -q --amend -F "$MSG"
edited=$(message)
[[ $edited == *"Closed automatically, edited."* ]] || fail "the test's own Plan edit didn't take"
out=$(SHIP_ISSUE=$FIXTURE_ISSUE "$SHIP" 2>&1) || fail "re-ship after editing the Plan failed: $out"
msg=$(message)
[ "$(starts "$msg")" = 1 ] || fail "$(starts "$msg") blocks after the re-ship, wanted 1"
[ "$msg" = "$edited" ] || fail "re-ship changed the edited message: $(diff <(echo "$edited") <(echo "$msg") | head -20)"
v3=$(git rev-parse HEAD)
[ "$(remote_head)" = "$v3" ] || fail "origin/$BRANCH is not the edited commit"
await_pr_head "$v3"
pass "edited Plan: re-ship kept the edit and exactly one block"

# --- 4. SHIP_ISSUE unset: the message is left alone -------------------------
echo "=== restoring the hand-written message and shipping with SHIP_ISSUE unset"
printf '%s\n' "$original" >"$MSG"
git commit -q --amend -F "$MSG"
v4=$(git rev-parse HEAD)
out=$("$SHIP" 2>&1) || fail "ship with SHIP_ISSUE unset failed: $out"
grep -qF "ship: no issue number in $BRANCH; message left as is." <<<"$out" \
	|| fail "ship didn't say it left the message alone: $out"
[ "$(git rev-parse HEAD)" = "$v4" ] || fail "ship rewrote the commit with no issue to copy"
[ "$(message)" = "$original" ] || fail "the message changed with SHIP_ISSUE unset"
[ "$(remote_head)" = "$v4" ] || fail "origin/$BRANCH is not the hand-written commit"
pass "SHIP_ISSUE unset: ship said so and shipped the message as written"

# --- 5. no such issue: ship refuses and pushes nothing ----------------------
echo "=== shipping with SHIP_ISSUE naming no issue"
if out=$(SHIP_ISSUE=999999999 "$SHIP" 2>&1); then
	fail "ship succeeded with an issue it couldn't read: $out"
fi
grep -qF "couldn't read #999999999's details" <<<"$out" || fail "ship refused, but not for the lookup: $out"
[ "$(git rev-parse HEAD)" = "$v4" ] || fail "a failed lookup still rewrote the commit"
[ "$(remote_head)" = "$v4" ] || fail "a failed lookup still pushed"
pass "unreadable issue: ship refused and pushed nothing"

# --- 6. skip directives in the issue text are defused ----------------------
# Offline, against a stub gh, so it doesn't depend on any real issue's body.
echo "=== rendering an issue whose title and body carry skip directives"
mkdir -p "$WORK_DIR/stub"
cat >"$WORK_DIR/stub/gh" <<'EOF'
#!/bin/bash
[ "$1 $2 $3" = "issue view 900" ] || exit 1
jq -n '{title: "[Task]: Put [skip ci] in the body",
	body: "### What needs to be built\nUse [skip ci], [CI Skip], [no ci], [skip actions] or [actions skip].\n\n### Done when\n- [ ] the commit says [skip ci]"}'
EOF
chmod +x "$WORK_DIR/stub/gh"
out=$(PATH="$WORK_DIR/stub:$PATH" "$BLOCK" 900) || fail "issue-block.sh failed on the stub issue: $out"
! grep -qiE '\[(skip ci|ci skip|no ci|skip actions|actions skip)\]' <<<"$out" \
	|| fail "a skip directive survived the copy: $(grep -iE '\[(skip ci|ci skip|no ci|skip actions|actions skip)\]' <<<"$out")"
[ "${out%%$'\n'*}" = "Put (skip ci) in the body" ] || fail "title is '${out%%$'\n'*}', wanted 'Put (skip ci) in the body'"
grep -qF -- '- [ ] the commit says (skip ci)' <<<"$out" || fail "the Done-when item wasn't kept, defused: $out"
pass "skip directives in the issue's title, scope and Done-when come out defused"

echo
echo "#450's copy step holds end to end."
summary
