#! /bin/bash
# Validates GH issue #451: scripts/backfill-history.sh refuses to fold two
# commits of one unit that aren't adjacent, since folding them would reorder
# history, and its dry run changes no ref.
#
# A throwaway repo of four commits, classified with --units so no GitHub
# lookup runs: issue #1, a convention commit, issue #1 again, another
# convention commit. The dry run must exit 1 naming both #1 commits, and leave
# every ref as it was. The same history classified all convention (which
# needs no GitHub lookup to build its messages) runs to a full report, with no
# fold and no ref changed.
#
# The fold and the full dry/apply/verify run against main need GitHub and are
# recorded in #451's PR instead.
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="backfill-history.sh refuses to fold non-adjacent commits of one unit, and its dry run changes no ref (#451)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="git, jq"
VALIDATE_DURATION="~2s"

validate_parse_args "$@"

require_cmd git jq
check_prereqs

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_EDITOR=true
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR
# The script's cache must not be the real one.
export BACKFILL_CACHE="$WORK_DIR/cache"

O="$WORK_DIR/origin.git"
W="$WORK_DIR/work"
git_fixture_repos "$O" "$W"
for f in a b c d; do commit_file "$W" "$f" "$f"; done
git -C "$W" push -q --no-verify origin main
git -C "$W" fetch -q origin

mapfile -t S < <(git -C "$W" rev-list --reverse main)
cat >"$WORK_DIR/units" <<EOF
${S[0]:0:7} issue 1
${S[1]:0:7} convention
${S[2]:0:7} issue 1
${S[3]:0:7} convention
EOF

refs_before=$(git -C "$W" for-each-ref --format='%(refname) %(objectname)')
out=$("$ROOT_DIR/scripts/backfill-history.sh" --dry-run --repo "$W" --units "$WORK_DIR/units" origin/main 2>&1)
rc=$?
check_eq "a unit in two non-adjacent commits: exit 1" 1 "$rc"
if [[ $out == *"issue #1 is in ${S[0]:0:7} and ${S[2]:0:7}, which aren't adjacent"* ]]; then
	record_pass "  naming the unit and both commits"
else
	record_fail "  naming the unit and both commits" "$out"
fi
check_eq "  no ref changed" "$refs_before" "$(git -C "$W" for-each-ref --format='%(refname) %(objectname)')"

sed -i 's/ issue 1$/ convention/' "$WORK_DIR/units"
out=$("$ROOT_DIR/scripts/backfill-history.sh" --dry-run --repo "$W" --units "$WORK_DIR/units" origin/main 2>&1)
rc=$?
check_eq "no shared unit: the dry run exits 0" 0 "$rc"
if [[ $out == *"commits: 4 old, 4 new"* && $out == *"folds: 0"* ]]; then
	record_pass "  and reports 4 commits and no fold"
else
	record_fail "  and reports 4 commits and no fold" "$out"
fi
check_eq "  no ref changed" "$refs_before" "$(git -C "$W" for-each-ref --format='%(refname) %(objectname)')"

summary
