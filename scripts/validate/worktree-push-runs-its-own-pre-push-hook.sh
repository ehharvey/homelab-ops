#! /bin/bash
# Validates GH issue #279: a push from a worktree runs that worktree's own
# .githooks/pre-push even when core.hooksPath is absolute.
#
# The repo sets core.hooksPath to the relative `.githooks`, which resolves per
# worktree. Claude Code's worktree setup rewrites the shared config to the
# absolute <main checkout>/.githooks, so git starts the main checkout's copy of
# the hook from every worktree. The hook now hands off to the copy in the
# checkout being pushed. This proves, with this checkout's hook:
#
#   1. Absolute hooksPath, push from a worktree whose hook differs: the main
#      copy starts, hands off, and the worktree's copy runs, with the hook's
#      args and stdin (it still refuses a 2-commit push).
#   2. No recursion when the copy running is the checkout's own: a push from
#      the main checkout, and from a worktree whose hook is a symlink to the
#      main copy, each run the hook exactly once (under a timeout, so a loop
#      is a FAIL, not a hang).
#   3. A worktree with no hook of its own falls back to the main copy, which
#      still enforces one commit. So does one whose hook isn't executable, but
#      that one says so on stderr. A started copy with no run-own-copy.sh
#      beside it checks the push itself instead of failing to source it.
#   4. Relative hooksPath is unchanged: git runs the worktree's copy directly.
#   5. lib.sh's pre_push_under_test names the copy that checks the push: the
#      worktree's own after a hand-off, else the started copy, including one
#      whose source line is commented out or whose helper is missing.
#
# Each checkout gets this checkout's .githooks/run-own-copy.sh (the hand-off)
# and its pre-push with one marker line after the shebang, appending
# "<label> <first arg>" to a log, so the log shows which copies ran, in order.
# Everything runs in throwaway repos under mktemp with a bare local "origin";
# the real shared .git/config is never touched.
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="a worktree's push runs its own pre-push hook under an absolute core.hooksPath, without recursing (#279)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="git, timeout"
VALIDATE_DURATION="~2s"

validate_parse_args "$@"

require_cmd git timeout
check_prereqs

HOOKS="$ROOT_DIR/.githooks"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# The operator's git config must not change what these repos do, and a caller
# running inside a hook must not leak its repo into them.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_EDITOR=true
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR

LOG="$WORK_DIR/ran.log"
O="$WORK_DIR/origin.git"
M="$WORK_DIR/main"

# marked_hooks <checkout> <label>: this checkout's hand-off, and its pre-push
# plus a marker line.
marked_hooks() {
	local d="$1/.githooks"
	mkdir -p "$d"
	cp "$HOOKS/run-own-copy.sh" "$d/"
	rm -f "$d/pre-push"
	{
		sed -n 1p "$HOOKS/pre-push"
		# shellcheck disable=SC2016 # $1 expands when the hook runs
		printf 'echo "%s $1" >>"%s"\n' "$2" "$LOG"
		sed 1d "$HOOKS/pre-push"
	} >"$d/pre-push"
	chmod +x "$d/pre-push"
}

# do_push <dir> <git push args...>: sets PUSH_RC, PUSH_OUT and RAN (the
# marker lines, comma-joined).
do_push() {
	local dir="$1" lines
	shift
	: >"$LOG"
	PUSH_OUT=$(timeout 30 git -C "$dir" push "$@" 2>&1)
	PUSH_RC=$?
	mapfile -t lines <"$LOG"
	RAN=$(IFS=,; echo "${lines[*]}")
}

# --- setup -------------------------------------------------------------------
git_fixture_repos "$O" "$M"
marked_hooks "$M" main
git -C "$M" add .githooks
git -C "$M" commit -q -m "main: hooks"
git -C "$M" push -q --no-verify origin main

# WT: its own, different hook, committed as its one commit.
WT="$WORK_DIR/wt"
git -C "$M" worktree add -q -b feature "$WT" main
marked_hooks "$WT" worktree
git -C "$WT" commit -q -am "wt: own hook"

# The absolute value Claude Code writes, in the config every worktree shares.
git -C "$M" config core.hooksPath "$M/.githooks"
check_eq "the worktree sees the absolute core.hooksPath" \
	"$M/.githooks" "$(git -C "$WT" config core.hooksPath)"

# --- 1. hand-off to the worktree's copy ---------------------------------------
do_push "$WT" origin feature
check_eq "worktree push exits 0 (one commit ahead)" 0 "$PUSH_RC"
check_eq "main copy hands off; worktree copy runs with the hook's args" \
	"main origin,worktree origin" "$RAN"

commit_file "$WT" B "$RANDOM"
do_push "$WT" origin feature
check_eq "worktree copy runs for the 2-commit push" "main origin,worktree origin" "$RAN"
if [ "$PUSH_RC" -ne 0 ] && [[ $PUSH_OUT == *"2 commits ahead of main"* ]]; then
	record_pass "worktree copy reads the refs on stdin and refuses 2 commits"
else
	record_fail "worktree copy reads the refs on stdin and refuses 2 commits" \
		"rc $PUSH_RC, output: $PUSH_OUT"
fi
git -C "$WT" reset -q --hard HEAD~1

# --- 2. no recursion when it is the same file -----------------------------------
git -C "$M" switch -q -c main-feature
commit_file "$M" C "$RANDOM"
do_push "$M" origin main-feature
check_eq "main checkout push exits 0 (no loop, no timeout)" 0 "$PUSH_RC"
check_eq "main checkout runs its hook exactly once" "main origin" "$RAN"

WT2="$WORK_DIR/wt2"
git -C "$M" worktree add -q -b feature2 "$WT2" main
rm "$WT2/.githooks/pre-push"
ln -s "$M/.githooks/pre-push" "$WT2/.githooks/pre-push"
commit_file "$WT2" D "$RANDOM"
do_push "$WT2" origin feature2
check_eq "symlinked worktree hook push exits 0 (no loop, no timeout)" 0 "$PUSH_RC"
check_eq "a symlink to the running hook is the same file: it runs once" "main origin" "$RAN"

# --- 3. no hook of its own: the running copy enforces -------------------------
WT3="$WORK_DIR/wt3"
git -C "$M" worktree add -q -b feature3 "$WT3" main
rm "$WT3/.githooks/pre-push"
commit_file "$WT3" E "$RANDOM"
commit_file "$WT3" F "$RANDOM"
do_push "$WT3" origin feature3
check_eq "a worktree without its own hook falls back to the main copy" "main origin" "$RAN"
if [ "$PUSH_RC" -ne 0 ] && [[ $PUSH_OUT == *"2 commits ahead of main"* ]]; then
	record_pass "the fallback copy still refuses 2 commits"
else
	record_fail "the fallback copy still refuses 2 commits" "rc $PUSH_RC, output: $PUSH_OUT"
fi

WT4="$WORK_DIR/wt4"
git -C "$M" worktree add -q -b feature4 "$WT4" main
chmod -x "$WT4/.githooks/pre-push"
commit_file "$WT4" G "$RANDOM"
do_push "$WT4" origin feature4
check_eq "a worktree whose hook isn't executable falls back to the main copy" \
	"main origin" "$RAN"
if [ "$PUSH_RC" -eq 0 ] && [[ $PUSH_OUT == *"$WT4/.githooks/pre-push is not executable"* ]]; then
	record_pass "the non-executable fallback says so on stderr"
else
	record_fail "the non-executable fallback says so on stderr" "rc $PUSH_RC, output: $PUSH_OUT"
fi

# The started copy has no helper beside it: it checks the push itself.
BARE="$WORK_DIR/bare-hooks"
marked_hooks "$WORK_DIR/bare" bare
mv "$WORK_DIR/bare/.githooks" "$BARE"
rm "$BARE/run-own-copy.sh"
git -C "$M" config core.hooksPath "$BARE"
do_push "$WT3" origin feature3
check_eq "a started copy without run-own-copy.sh runs itself" "bare origin" "$RAN"
if [ "$PUSH_RC" -ne 0 ] && [[ $PUSH_OUT == *"2 commits ahead of main"* ]] &&
	[[ $PUSH_OUT != *"No such file"* ]]; then
	record_pass "  and still refuses 2 commits, with no sourcing error"
else
	record_fail "  and still refuses 2 commits, with no sourcing error" \
		"rc $PUSH_RC, output: $PUSH_OUT"
fi
git -C "$M" config core.hooksPath "$M/.githooks"

# --- 4. relative hooksPath: unchanged ---------------------------------------------
git -C "$M" config core.hooksPath .githooks
do_push "$WT" origin HEAD:refs/heads/feature-rel
check_eq "relative hooksPath push from the worktree exits 0" 0 "$PUSH_RC"
check_eq "relative hooksPath runs only the worktree's copy" "worktree origin" "$RAN"

# --- 5. pre_push_under_test -----------------------------------------------------
git -C "$M" config core.hooksPath "$M/.githooks"
check_eq "under test after a hand-off: the worktree's own copy" \
	"$WT/.githooks/pre-push" "$(cd "$WT" && pre_push_under_test)"
git -C "$M" config core.hooksPath "$BARE"
check_eq "under test when the started copy lacks the helper: the started copy" \
	"$BARE/pre-push" "$(cd "$WT" && pre_push_under_test)"
cp "$M/.githooks/run-own-copy.sh" "$BARE/"
sed -i 's|^\(\[ ! -f .*run-own-copy\.sh"\)$|# \1|' "$BARE/pre-push"
check "  (the fixture's source line is commented out, its shellcheck comment kept)" \
	grep -q '^# \[ ! -f .*run-own-copy\.sh"$' "$BARE/pre-push"
check_eq "under test when the source line is commented out: the started copy" \
	"$BARE/pre-push" "$(cd "$WT" && pre_push_under_test)"
git -C "$M" config core.hooksPath "$M/.githooks"

summary
