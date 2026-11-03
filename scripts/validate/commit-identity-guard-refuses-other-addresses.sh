#! /bin/bash
# Validates GH issue #451: a commit whose author or committer isn't the noreply
# identity (or a [bot] noreply address) is refused before it can reach main.
#
#   1. scripts/check-commit-identity.sh exits 1 and names the commit and the
#      field for a personal author or committer address, and for a
#      `x[bot]@users.noreply.github.com` author with no `<id>+`; it exits 0 for
#      the noreply identity and for a dependabot-style `<id>+<name>[bot]`
#      author, and 2 for a bad range.
#   2. .githooks/pre-push runs it on the commits a push adds: a commit authored
#      as `x <emil.h.harvey@gmail.com>` on a non-validate branch is refused and
#      nothing reaches the remote (`git ls-remote` is empty); the same commit
#      on a validate-*/ branch is let through (the suite's throwaway repos use
#      validate@example.invalid); a noreply commit pushes. When the check
#      can't run (exit 2), the hook refuses with the check's own error, not as
#      an identity violation.
#   3. A fixture repo that uses the hook without the script beside it (what
#      the other hook fixtures do) still pushes.
#
# Everything runs in throwaway repos under mktemp with a bare local "origin",
# never against GitHub; the real refusal against origin is the operator's
# real-run check in #451's PR.
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="a commit not on the noreply identity is refused by check-commit-identity.sh and pre-push; validate-*/ branches are exempt (#451)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="git"
VALIDATE_DURATION="~2s"

validate_parse_args "$@"

require_cmd git
check_prereqs

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_EDITOR=true
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR

NOREPLY="36806437+ehharvey@users.noreply.github.com"
PERSONAL="emil.h.harvey@gmail.com"
BOT="49699333+dependabot[bot]@users.noreply.github.com"
O="$WORK_DIR/origin.git"
W="$WORK_DIR/work"

# commit_as <name> <author email> <committer email> <file>
commit_as() {
	echo "$RANDOM" >"$W/$4"
	git -C "$W" add "$4"
	GIT_AUTHOR_NAME=$1 GIT_AUTHOR_EMAIL=$2 GIT_COMMITTER_NAME=$1 GIT_COMMITTER_EMAIL=$3 \
		git -C "$W" commit -q -m "touch $4"
}

git_fixture_repos "$O" "$W"
git -C "$W" config user.name "Emil Harvey"
git -C "$W" config user.email "$NOREPLY"
mkdir -p "$W/.githooks" "$W/scripts"
cp "$ROOT_DIR/.githooks/pre-push" "$ROOT_DIR/.githooks/run-own-copy.sh" "$W/.githooks/"
cp "$ROOT_DIR/scripts/check-commit-identity.sh" "$W/scripts/"
git -C "$W" add .githooks scripts
git -C "$W" commit -q -m "main: hooks and the identity check"
git -C "$W" push -q --no-verify origin main
git -C "$W" config core.hooksPath .githooks

check_id() { (cd "$W" && "$W/scripts/check-commit-identity.sh" "$@"); }

# --- 1. the check ---------------------------------------------------------------
git -C "$W" switch -q -c eharvey/#1
commit_as x "$PERSONAL" "$NOREPLY" A
out=$(check_id HEAD~1..HEAD 2>/dev/null)
rc=$?
check_eq "a personal author address exits 1" 1 "$rc"
check_eq "  and names the commit and the field" \
	"$(git -C "$W" rev-parse --short HEAD) author $PERSONAL" "$out"

git -C "$W" switch -q -c eharvey/#2 main
commit_as "Emil Harvey" "$NOREPLY" "$PERSONAL" B
out=$(check_id -1 HEAD 2>/dev/null)
rc=$?
check_eq "a personal committer address exits 1" 1 "$rc"
check_eq "  and names the committer" "$(git -C "$W" rev-parse --short HEAD) committer $PERSONAL" "$out"

git -C "$W" switch -q -c eharvey/#4 main
commit_as "dependabot[bot]" "$BOT" "$NOREPLY" D
check "a [bot] author exits 0" check_id main..HEAD

git -C "$W" switch -q -c eharvey/#5 main
commit_as "x[bot]" "x[bot]@users.noreply.github.com" "$NOREPLY" E
out=$(check_id main..HEAD 2>/dev/null)
rc=$?
check_eq "a [bot] address with no <id>+ exits 1" 1 "$rc"
check_eq "  and names the author" \
	"$(git -C "$W" rev-parse --short HEAD) author x[bot]@users.noreply.github.com" "$out"

git -C "$W" switch -q -c eharvey/#3 main
commit_as "Emil Harvey" "$NOREPLY" "$NOREPLY" C
check "the noreply identity exits 0" check_id main..HEAD
check_eq "a bad range exits 2" 2 "$(check_id nosuch..HEAD >/dev/null 2>&1; echo $?)"

# --- 2. pre-push ----------------------------------------------------------------
out=$(git -C "$W" push origin eharvey/#1 2>&1)
rc=$?
check_eq "pre-push refuses a personal-address commit on eharvey/#1" 1 "$rc"
if [[ $out == *"author $PERSONAL"* ]]; then
	record_pass "  and prints the offending commit"
else
	record_fail "  and prints the offending commit" "$out"
fi
check_eq "  nothing reached origin (ls-remote is empty)" "" "$(git -C "$W" ls-remote origin refs/heads/eharvey/#1)"

check "the same commit on validate-451/x pushes (validate branches are exempt)" \
	git -C "$W" push -q origin eharvey/#1:refs/heads/validate-451/x
check "a noreply commit on eharvey/#3 pushes" git -C "$W" push -q origin eharvey/#3
check_eq "  and reached origin" "$(git -C "$W" rev-parse eharvey/#3)" \
	"$(git -C "$W" ls-remote origin refs/heads/eharvey/#3 | cut -f1)"

# A check that can't run (exit 2) refuses with its own error, not as an
# identity violation. The hook runs the working tree's copy, so a stub there
# stands in for git log failing.
git -C "$W" switch -q -c eharvey/#6 main
cp "$W/scripts/check-commit-identity.sh" "$WORK_DIR/check.sh"
printf '#!/bin/sh\necho "fatal: stub range error" >&2\nexit 2\n' >"$W/scripts/check-commit-identity.sh"
commit_as "Emil Harvey" "$NOREPLY" "$NOREPLY" F
out=$(git -C "$W" push origin eharvey/#6 2>&1)
rc=$?
cp "$WORK_DIR/check.sh" "$W/scripts/check-commit-identity.sh"
check_eq "pre-push refuses when the check exits 2" 1 "$rc"
if [[ $out == *"fatal: stub range error"* && $out == *"exited 2"* && $out != *"isn't"*"noreply address"* ]]; then
	record_pass "  showing the check's error, not an identity violation"
else
	record_fail "  showing the check's error, not an identity violation" "$out"
fi

# --- 3. a hook fixture with no script ---------------------------------------------
git -C "$W" switch -q eharvey/#1
git -C "$W" rm -q scripts/check-commit-identity.sh
git -C "$W" commit -q --amend --no-edit
check "without scripts/check-commit-identity.sh the hook skips the check" \
	git -C "$W" push -q origin eharvey/#1:refs/heads/fixture

summary
