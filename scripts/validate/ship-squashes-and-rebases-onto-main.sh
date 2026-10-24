#! /bin/bash
# Validates GH issue #319: `make ship` makes a branch shippable instead of
# refusing it. scripts/ship.sh (this checkout's copy) squashes a multi-commit
# branch into one commit carrying the FIRST commit's message, rebases a branch
# that is behind main, and refuses, pushing nothing, when its rebase conflicts,
# when a rebase is already in progress, or when the branch is stacked on
# another. It also proves the corrected manual squash that .githooks/pre-push
# prints. The old one soft-reset straight to FETCH_HEAD, which silently
# reverted whatever landed on main since the branch forked, and reused the
# message with `commit -c`, whose editor cleanup deletes `## Plan` headings.
#
# `make test` can't see any of this: it is git history, pushes and hooks. Every
# case runs in a throwaway repo with a bare "origin" under mktemp, so nothing
# touches GitHub or the caller's checkout. A stub `gh` on PATH answers "no PR"
# to ship's lookup, records `pr create`, and prints a fake PR for the final
# `pr view`. Each repo uses this checkout's .githooks, so ship's own pushes go
# through the pre-push hook under test.
#
# Not covered on purpose: a branch pushed earlier with no PR open that ship
# then rewrites. Its plain push is refused as non-fast-forward until #281.
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="make ship squashes to the first commit's message, rebases onto main, and refuses mid-rebase, conflicting or stacked branches; the pre-push squash hint keeps main's changes (#319)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="git"
VALIDATE_DURATION="~3s"

validate_parse_args "$@"

require_cmd git
check_prereqs

SHIP="$ROOT_DIR/scripts/ship.sh"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

# The operator's git config (aliases, pull.rebase, signing, hooks) must not
# change what these repos do; each repo gets its own identity below.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_EDITOR=true

STUB="$WORK_DIR/stub"
mkdir -p "$STUB"
cat >"$STUB/gh" <<'EOF'
#!/bin/bash
echo "$*" >>"$GH_LOG"
case "$1 $2" in
"pr view")
	# ship's final line asks for number,url; its first lookup gets "no PR".
	[[ $* == *number,url* ]] && { echo "ship: #1 open — stub"; exit 0; }
	exit 1
	;;
"pr create") exit 0 ;;
esac
exit 1
EOF
chmod +x "$STUB/gh"

M1=$(printf 'feat: first commit\n\n## Plan\n\nThe body ship must keep.\n\n## Test plan\n\n- [x] it\n\nCloses #0')

# new_world <name>: a bare origin whose main has file A, a working clone W
# (where ship runs), and a second clone UP that advances main behind W's back.
new_world() {
	local d="$WORK_DIR/$1"
	O="$d/origin.git"
	W="$d/work"
	UP="$d/up"
	export GH_LOG="$d/gh.log"
	git init -q --bare -b main "$O"
	git init -q -b main "$W"
	repo_config "$W"
	git -C "$W" remote add origin "$O"
	echo a >"$W/A"
	git -C "$W" add A
	git -C "$W" commit -q -m "main: A"
	git -C "$W" push -q --no-verify origin main
	git clone -q "$O" "$UP"
	repo_config "$UP"
}

repo_config() {
	git -C "$1" config user.name validate
	git -C "$1" config user.email validate@example.invalid
	git -C "$1" config commit.gpgsign false
	git -C "$1" config core.hooksPath "$ROOT_DIR/.githooks"
}

# commit_file <repo> <file> <content> [message]
commit_file() {
	echo "$3" >"$1/$2"
	git -C "$1" add "$2"
	git -C "$1" commit -q -m "${4:-touch $2}"
}

# advance_main <file> <content>: land a commit on origin's main from UP.
advance_main() {
	commit_file "$UP" "$1" "$2" "main: $1"
	git -C "$UP" push -q --no-verify origin main
}

# ship: run scripts/ship.sh in W; sets OUT (stdout+stderr) and RC.
ship() {
	OUT=$(cd "$W" && PATH="$STUB:$PATH" "$SHIP" 2>&1)
	RC=$?
}

remote_has() { git --git-dir="$O" rev-parse -q --verify "refs/heads/$1" >/dev/null; }
rebasing() { (cd "$W" && [ -e "$(git rev-parse --git-path rebase-merge)" ] || [ -e "$(git rev-parse --git-path rebase-apply)" ]); }
show_out() { sed 's/^/    | /' <<<"$OUT"; }

echo "== squash + rebase: 2 commits, main advanced =="
new_world squash
git -C "$W" switch -q -c feat
commit_file "$W" c1 one "$M1"
commit_file "$W" c2 two
advance_main B b
ship
[ "$RC" -eq 0 ] || show_out
check_eq "ship exits 0" 0 "$RC"
check_eq "origin/feat is exactly 1 commit ahead of origin/main" 1 \
	"$(git --git-dir="$O" rev-list --count main..feat 2>/dev/null)"
check_eq "origin/feat is not behind origin/main" 0 \
	"$(git --git-dir="$O" rev-list --count feat..main 2>/dev/null)"
check_eq "the shipped commit's message is the first commit's" "$M1" \
	"$(git --git-dir="$O" log -1 --format=%B feat 2>/dev/null)"
check "main's file B is in the pushed tree (the FETCH_HEAD regression)" \
	git --git-dir="$O" cat-file -e feat:B
check "both branch commits' files are in the pushed tree" \
	bash -c "git --git-dir='$O' cat-file -e feat:c1 && git --git-dir='$O' cat-file -e feat:c2"
check "ship prints the commits it folded in" \
	bash -c "grep -q 'squashed 2 commits' <<<\"\$1\" && grep -q 'touch c2' <<<\"\$1\"" _ "$OUT"
check "ship says it rebased onto main" grep -q 'rebased onto main' <<<"$OUT"
check "ship opened the PR with --fill" grep -q '^pr create --fill' "$GH_LOG"

echo "== behind only: 1 commit, main advanced =="
new_world behind
git -C "$W" switch -q -c feat
commit_file "$W" c1 one "$M1"
advance_main B b
ship
[ "$RC" -eq 0 ] || show_out
check_eq "ship exits 0" 0 "$RC"
check "origin/feat is rebased onto current main" \
	git --git-dir="$O" merge-base --is-ancestor main feat
check "main's file B is in the pushed tree" git --git-dir="$O" cat-file -e feat:B

echo "== already shippable: 1 commit, not behind =="
new_world one
git -C "$W" switch -q -c feat
commit_file "$W" c1 one "$M1"
before=$(git -C "$W" rev-parse HEAD)
ship
[ "$RC" -eq 0 ] || show_out
check_eq "ship exits 0" 0 "$RC"
check_eq "the pushed commit is the local one, untouched" "$before" \
	"$(git --git-dir="$O" rev-parse feat 2>/dev/null)"
check "ship neither squashed nor rebased" \
	bash -c "! grep -qE 'squashed|rebased' <<<\"\$1\"" _ "$OUT"

echo "== conflict: branch and main both edit A =="
new_world conflict
git -C "$W" switch -q -c feat
commit_file "$W" A branch-edit "$M1"
commit_file "$W" c2 two
advance_main A main-edit
ship
check "ship exits non-zero" test "$RC" -ne 0
check "the rebase is left stopped for resolution" rebasing
check "nothing was pushed" bash -c "! git --git-dir='$O' rev-parse -q --verify refs/heads/feat"
echo "-- ship again, mid-rebase --"
ship
check "ship exits non-zero" test "$RC" -ne 0
check "ship prints the git rebase --continue hint" grep -q 'git rebase --continue' <<<"$OUT"
check "nothing was pushed" bash -c "! git --git-dir='$O' rev-parse -q --verify refs/heads/feat"
remote_has feat && show_out

echo "== stacked: stack-c sits on stack-p (pushed, 2 commits) =="
new_world stacked
git -C "$W" switch -q -c stack-p
commit_file "$W" p1 one "$M1"
commit_file "$W" p2 two
git -C "$W" push -q --no-verify -u origin stack-p
git -C "$W" switch -q -c stack-c
commit_file "$W" c1 three
before=$(git -C "$W" rev-parse HEAD)
ship
check "ship exits non-zero" test "$RC" -ne 0
check "ship names stack-p as the containing branch" grep -qw 'stack-p' <<<"$OUT"
check_eq "stack-c's history is unchanged" "$before" "$(git -C "$W" rev-parse HEAD)"
check "stack-c is still 3 commits beyond main" \
	test "$(git -C "$W" rev-list --count origin/main..HEAD)" -eq 3
check "nothing was pushed" bash -c "! git --git-dir='$O' rev-parse -q --verify refs/heads/stack-c"
[ "$RC" -ne 0 ] || show_out

echo "== pre-push hint: run the hook's printed squash on a behind 2-commit branch =="
new_world hint
git -C "$W" switch -q -c feat
commit_file "$W" c1 one "$M1"
commit_file "$W" c2 two
advance_main B b
push_out=$(git -C "$W" push origin feat 2>&1)
push_rc=$?
check "the pre-push hook refuses the 2-commit push" test "$push_rc" -ne 0
hint=$(sed -n 's/^    \(git .*\)$/\1/p' <<<"$push_out")
check "the hook prints a merge-base squash" grep -q 'reset --soft "$(git merge-base FETCH_HEAD HEAD)"' <<<"$hint"
hint_out=$(cd "$W" && bash -euo pipefail -c "$hint" 2>&1)
hint_rc=$?
[ "$hint_rc" -eq 0 ] || sed 's/^/    | /' <<<"$hint_out"
check_eq "the printed commands run cleanly" 0 "$hint_rc"
check_eq "HEAD is 1 commit beyond main" 1 "$(git -C "$W" rev-list --count FETCH_HEAD..HEAD)"
check_eq "the squashed commit's message is the first commit's" "$M1" "$(git -C "$W" log -1 --format=%B)"
check_eq "the squashed commit changes only the branch's files, not main's B" "c1 c2" \
	"$(git -C "$W" diff --name-only HEAD^ HEAD | paste -sd' ' -)"
check "the pre-push hook now accepts the push" git -C "$W" push -q origin feat
check "B survives once the commit is rebased onto main" \
	bash -c "cd '$W' && git rebase -q FETCH_HEAD && git cat-file -e HEAD:B"

summary
