#!/usr/bin/env bash
# Does a commit's message carry what the PR body needs (#405)? Every route to
# main runs this one check: `make ship` (scripts/ship.sh) before it pushes, the
# required `one-commit` job (.github/workflows/pr-shape.yml) on the PR's head,
# and the review audit (.claude/skills/review-work/audit.sh). It moved here out
# of audit.sh so the rules live in one place.
#
#   check-commit-msg.sh [--if-shipped] [--no-lookup] [--hint] [--branch <ref>] <rev>|- [<n>]
#   check-commit-msg.sh --closes <rev>|-
#
# `-` reads the message from stdin. The issue <n> is the one given, else the
# <n> of a `--branch` named */#<n>, else the issue of make ship's
# `<!-- ship:issue #<n> -->` block in the message (a ship under SHIP_ISSUE),
# else none. Callers pass --branch rather than deriving <n> themselves, so
# ship, CI and the audit check the same issue.
#
# The message must have:
#
#   - a `## Plan` line and a `## Test plan` line, above make ship's block
#     (#450): the issue text that block copies can quote `## Plan` lines of its
#     own;
#   - with <n>, the exact, case-sensitive `Closes #<n>` (so `#45` doesn't match
#     `#450`), and when #<n> is a /bundle parent (`bundle_title` in
#     .claude/skills/lib.jq) a `Closes #<m>` per OPEN native sub-issue, since
#     the bundle's commit lands their work too (Development Conventions
#     § Sizing and bundles);
#   - without <n>, any `Closes #<k>`.
#
# A `Closes` counts only outside code fences and inline code, where GitHub
# ignores closing keywords. A block marker counts only outside a fence, the way
# ship.sh finds it. --closes prints the numbers the message closes by that rule,
# one per line, for ship's `--omit-closes`.
#
# --if-shipped checks a bundle's sub-issue lines only once make ship's block is
# in the message: ship's block adds them, so before ship has run (what the
# audit sees from /ship-issue) there's nothing to check. --no-lookup skips the
# bundle lookup altogether: ship passes it, because its block was just built
# from the same GitHub data (scripts/issue-block.sh) and CI looks it up anyway.
#
# The bundle lookup fails closed: when #<n>'s title or sub-issues can't be read
# from GitHub, that is reported as a missing part, because a child the commit
# doesn't close stays Ready and gets redone. A non-bundle <n> pays one
# `gh issue view` for the title.
#
# Prints one line per missing part (`## Plan`, `## Test plan`, `Closes #<n>`,
# `Closes #<m> (sub-issue)`, `Closes #<n>` literally when there's no <n>);
# --hint adds a blank line and how to fix the message.
# Exit: 0 nothing missing; 1 something missing; 2 usage, or no such commit.
set -euo pipefail

skills="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.claude/skills" && pwd)"

usage() {
	echo "usage: $0 [--if-shipped] [--no-lookup] [--hint] [--branch <ref>] <rev>|- [<issue-number>]" >&2
	echo "       $0 --closes <rev>|-" >&2
	exit 2
}

if_shipped="" lookup=1 hint="" branch="" closes=""
while [[ $# -gt 0 ]]; do
	case $1 in
	--if-shipped) if_shipped=1 ;;
	--no-lookup) lookup="" ;;
	--hint) hint=1 ;;
	--closes) closes=1 ;;
	--branch)
		[[ $# -ge 2 ]] || usage
		branch=$2
		shift
		;;
	-) break ;;
	-*) usage ;;
	*) break ;;
	esac
	shift
done
[[ $# -ge 1 && $# -le 2 ]] || usage
[[ -z $closes || $# -eq 1 ]] || usage
rev=$1
n=${2:-}
[[ -z $n || $n =~ ^[0-9]+$ ]] || usage

if [[ $rev == - ]]; then
	msg=$(cat)
elif ! msg=$(git log -1 --format=%B "$rev" -- 2>/dev/null); then
	echo "$0: no commit $rev" >&2
	exit 2
fi

# The message with code fences and inline code dropped: what GitHub reads
# closing keywords from. The fence rule is ship.sh's.
code_free=$(awk '
	/^ ? ? ?(```|~~~)/ { fence = !fence; next }
	!fence { gsub(/`[^`]*`/, ""); print }' <<<"$msg")
closed() { grep -oE '(^|[^[:alnum:]])Closes #[0-9]+' <<<"$code_free" | grep -oE '[0-9]+$' || true; }

if [[ -n $closes ]]; then
	closed
	exit 0
fi

# make ship's block starts at the first marker outside a code fence (ship.sh's
# rule, #450); the author's text is everything above it.
marker=$(awk '
	/^ ? ? ?(```|~~~)/ { fence = !fence }
	!fence && /^<!-- ship:issue #[0-9]+ -->$/ { print NR, substr($0, 18) + 0; exit }' <<<"$msg")
authored=$msg
[[ -z $marker ]] || authored=$(head -n "$((${marker% *} - 1))" <<<"$msg")
if [[ -z $n && $branch =~ /#([0-9]+)$ ]]; then
	n=${BASH_REMATCH[1]}
fi
[[ -n $n || -z $marker ]] || n=${marker#* }

mapfile -t closed_nums < <(closed)
closes_n() {
	local k
	for k in "${closed_nums[@]}"; do [[ $k == "$1" ]] && return 0; done
	return 1
}

missing=()
grep -qE '^## Plan[[:space:]]*$' <<<"$authored" || missing+=("## Plan")
grep -qE '^## Test plan[[:space:]]*$' <<<"$authored" || missing+=("## Test plan")
if [[ -n $n ]]; then
	closes_n "$n" || missing+=("Closes #$n")
	if [[ -n $lookup && (-z $if_shipped || -n $marker) ]]; then
		if ! title=$(gh issue view "$n" --json title); then
			missing+=("Closes #<m> per open sub-issue, if #$n is a bundle (couldn't read #$n from GitHub)")
		elif jq -e -L "$skills" 'include "lib"; .title | bundle_title' <<<"$title" >/dev/null; then
			if ! subs=$(gh api "repos/{owner}/{repo}/issues/$n/sub_issues?per_page=100" |
				jq -r -L "$skills" 'include "lib"; open_sub_issues | .number'); then
				missing+=("Closes #<m> per open sub-issue (couldn't read bundle #$n's sub-issues from GitHub)")
			else
				for c in $subs; do
					closes_n "$c" || missing+=("Closes #$c (sub-issue)")
				done
			fi
		fi
	fi
elif [[ ${#closed_nums[@]} -eq 0 ]]; then
	missing+=("Closes #<n>")
fi

[[ ${#missing[@]} -eq 0 ]] && exit 0
printf '%s\n' "${missing[@]}"
if [[ -n $hint ]]; then
	cat <<-'EOF'

		Add them (docs/Development Conventions.md § PR body format), amending with
		`git commit --amend -F <file>`, never a bare --amend, which strips the
		`## ` lines (§ Amending keeps the sections). Then `make ship` again.
	EOF
fi
exit 1
