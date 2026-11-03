#! /bin/bash
# Validates GH issue #451: `scripts/check-commit-msg.sh --no-closes` checks
# the `## Plan` and `## Test plan` lines alone, by the default mode's rule, for
# a commit that closes no issue (the history backfill's PR-unit commits).
#
#   1. Both sections and no `Closes`: exit 0.
#   2. A section missing: exit 1, naming it. `Plan:` doesn't count, as in the
#      default mode, and a section only below make ship's block doesn't either.
#   3. With <n>, --branch or --closes too: a usage error, exit 2.
#   4. The default mode still fails the same message for its missing Closes.
#
# Messages go in on stdin (`-`), so no repo, network or gh is needed.
set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="check-commit-msg.sh --no-closes passes on both sections without Closes, names a missing one, and refuses <n>/--branch/--closes (#451)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="bash"
VALIDATE_DURATION="~1s"

validate_parse_args "$@"
check_prereqs

C="$ROOT_DIR/scripts/check-commit-msg.sh"

# run <message> <args...>: sets RC and OUT.
run() {
	local msg=$1
	shift
	OUT=$("$C" "$@" <<<"$msg" 2>&1)
	RC=$?
}

BOTH=$'subject\n\n## Plan\n\nWhy.\n\n## Test plan\n\n- [x] it ran'
NO_TEST=$'subject\n\n## Plan\n\nWhy.'
NO_PLAN=$'subject\n\nPlan:\n\nWhy.\n\n## Test plan\n\n- [x] it ran'
BELOW_BLOCK=$'subject\n\n## Plan\n\nWhy.\n\n<!-- ship:issue #7 -->\n## Test plan\n<!-- /ship:issue -->'

run "$BOTH" --no-closes -
check_eq "both sections, no Closes: exit 0" 0 "$RC"
check_eq "  and prints nothing" "" "$OUT"

run "$NO_TEST" --no-closes -
check_eq "no ## Test plan: exit 1" 1 "$RC"
check_eq "  naming it" "## Test plan" "$OUT"

run "$NO_PLAN" --no-closes -
check_eq "'Plan:' isn't ## Plan: exit 1" 1 "$RC"
check_eq "  naming ## Plan" "## Plan" "$OUT"

run "$BELOW_BLOCK" --no-closes -
check_eq "a ## Test plan only inside ship's block doesn't count: exit 1" 1 "$RC"

run "$BOTH" --no-closes - 451
check_eq "--no-closes with <n>: exit 2" 2 "$RC"
run "$BOTH" --no-closes --branch eharvey/#451 -
check_eq "--no-closes with --branch: exit 2" 2 "$RC"
run "$BOTH" --no-closes --closes -
check_eq "--no-closes with --closes: exit 2" 2 "$RC"
check "  the usage lists --no-closes" grep -q -- '--no-closes <rev>|-' <<<"$OUT"

run "$BOTH" -
check_eq "the default mode still fails it for no Closes: exit 1" 1 "$RC"
check_eq "  naming Closes #<n>" "Closes #<n>" "$OUT"

summary
