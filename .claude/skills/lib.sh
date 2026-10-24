# shellcheck shell=bash
# Shell helpers shared by the skill scripts (#289). Source it; don't run it:
#
#   # shellcheck source=SCRIPTDIR/../lib.sh
#   . "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
#
# A helper two scripts need lives here, so the copies can't drift apart.

# At most $1 lines of stdin, then a note of how many were cut.
bound() { awk -v max="$1" 'NR <= max { print } END { if (NR > max) printf "  … %d more lines\n", NR - max }'; }

# The worktree that has refs/heads/$1 checked out, if any. Every worktree of
# the repo counts, at any path: the main checkout, `make wt` ones, and
# /run-chain's agent worktrees under .claude/worktrees/agent-*.
worktree_of() {
	git worktree list --porcelain | awk -v ref="refs/heads/$1" '
		/^worktree / { wt = substr($0, 10) }
		$0 == "branch " ref { print wt; exit }'
}
