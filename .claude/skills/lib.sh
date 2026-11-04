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

# Every open issue as one JSON array, with the fields /next and
# scripts/hygiene.sh read (#388). $1 is the repo as owner/name.
open_issues_json() {
	# shellcheck disable=SC2016 # the $vars in single quotes are GraphQL's
	gh api graphql --paginate -F owner="${1%/*}" -F name="${1#*/}" -f query='
query($owner: String!, $name: String!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, first: 100, after: $endCursor) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title body createdAt
        milestone { title }
        labels(first: 20) { nodes { name } }
        parent { number state title }
        blockedBy(first: 50) { nodes { number state stateReason } }
        blocking(first: 50) { nodes { number state } }
      }
    }
  }
}' | jq -s '[.[].data.repository.issues.nodes[]]'
}

# Every issue's and PR's state as {"<n>": "OPEN" | "CLOSED" | "MERGED"}, so a
# blocker or a prose mention can be shown open or closed.
issue_states_json() {
	{
		gh issue list --state all --limit 5000 --json number,state
		gh pr list --state all --limit 5000 --json number,state
	} | jq -s 'add | map({key: (.number | tostring), value: .state}) | from_entries'
}

# docs/Roadmap.md's checklist items as TSV: section, depth, checked, text
# (continuation lines joined). Run it from the repo root.
roadmap_tsv() {
	awk '
	function flush() { if (text != "") print section "\t" depth "\t" checked "\t" text; text = "" }
	/^## / { flush(); section = substr($0, 4); next }
	/^[ \t]*- \[[ xX]\] / {
		flush()
		match($0, /^[ \t]*/); depth = int(RLENGTH / 2)
		checked = ($0 ~ /- \[[xX]\]/) ? "x" : " "
		text = $0; sub(/^[ \t]*- \[[ xX]\] /, "", text); next
	}
	/^[ \t]*$/ || /^[^ \t]/ { flush(); next }
	{ if (text != "") { line = $0; sub(/^[ \t]+/, "", line); text = text " " line } }
	END { flush() }
	' docs/Roadmap.md
}

# The issues the Roadmap marks paused, as a JSON array of numbers, so a paused
# blocker reads differently from a live one. Input: roadmap_tsv's output.
paused_json() {
	jq -R -s '[split("\n")[] | split("\t") | select(.[2] == " " and (.[3] // "" | startswith("*(paused")))
	  | .[3] | scan("see #[0-9]+(?:, #[0-9]+)*") | scan("#([0-9]+)") | .[0] | tonumber] | unique'
}
