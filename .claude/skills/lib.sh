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

# The order Ready issues are listed and run in (#392), shared by next.sh's
# Ready section and run-chain/queue.sh's Run list. Keys:
#   p0        issues labelled `p0` first
#   unblocks  most open dependents first
#   number    ascending issue number, always the final tiebreaker
# `ready_order` in lib.jq sorts; this holds the default and checks the keys.
# shellcheck disable=SC2034 # used by the scripts that source this
order_default=p0,unblocks,number

# The comma-separated keys in $1 as a JSON array; or, for an empty list, an
# empty, unknown or repeated key, the reason on stderr and status 2. The
# caller then prints its usage line and exits 2.
order_json() {
	local k seen=,
	local -a keys
	if [ -z "$1" ]; then
		echo "--order: empty key list" >&2
		return 2
	fi
	if [[ ,$1, == *,,* ]]; then
		echo "--order: empty key in '$1'" >&2
		return 2
	fi
	IFS=, read -ra keys <<<"$1"
	for k in "${keys[@]}"; do
		case $k in
		p0 | unblocks | number) ;;
		*)
			echo "--order: unknown key '$k' (keys: p0, unblocks, number)" >&2
			return 2
			;;
		esac
		if [[ $seen == *",$k,"* ]]; then
			echo "--order: repeated key '$k'" >&2
			return 2
		fi
		seen+="$k,"
	done
	printf '%s\n' "${keys[@]}" | jq -R . | jq -sc .
}
