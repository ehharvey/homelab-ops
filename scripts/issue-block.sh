#!/usr/bin/env bash
# Print an issue's details as the block `make ship` copies into the commit
# message (#450), so the PR shows what was asked for beside what was done and
# `git log` keeps both offline.
#
#   scripts/issue-block.sh [--omit-closes <k>]... <n>
#
# Line 1 is issue #n's title with its template prefix (`[Task]: `, `[Bug]: `,
# `[Decision]: `) stripped; ship makes it the commit subject. The rest is the
# block, delimited so a later ship can find and regenerate it:
#
#   <!-- ship:issue #<n> -->
#   ## Issue #<n>: <stripped title>
#
#   <the "### What needs to be built" section, verbatim>
#
#   ### Done when (from #<n>)
#   <the "### Done when" items, verbatim, still `- [ ]`>
#
#   Closes #<n>
#   <!-- /ship:issue -->
#
# A bug has neither section. Its scope is `### Expected behavior` and its
# Done-when the `Done when:` list that ends that section (or a `### Done when`
# section, if refinement added one). With no list, the Done-when heading is
# left off; with no scope section, so is the scope.
#
# A bundle parent (`bundle_title` in .claude/skills/lib.jq) also gets one
# section per OPEN native sub-issue, after its Done-when and before its
# Closes: the set audit.sh requires a `Closes` for. Each is the child's
# heading, its Done-when items and its `Closes #<m>`. An umbrella (sub-issues,
# any other title) gets none, since its children ship alone.
#
# The copied text, title included, is verbatim except for GitHub closing
# keywords followed by an issue number, which become `Closes issue #<k>`, and
# skip directives such as `[skip ci]`, which become `(skip ci)` (see defuse
# below): the block's own Closes lines are the only ones meant to close
# anything, and copied text must never skip the PR's checks.
#
# `--omit-closes <k>` leaves out the `Closes #<k>` line, for one the author's
# text already has. Any failed `gh` call fails the script, so ship refuses
# rather than shipping a half-built block.
set -euo pipefail

skills="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.claude/skills" && pwd)"

usage() {
	echo "usage: $0 [--omit-closes <k>]... <issue-number>" >&2
	exit 2
}

omit=" "
n=""
while [ $# -gt 0 ]; do
	case "$1" in
	--omit-closes)
		[[ ${2:-} =~ ^[0-9]+$ ]] || usage
		omit+="$2 "
		shift 2
		;;
	*)
		[ -z "$n" ] && [[ $1 =~ ^[0-9]+$ ]] || usage
		n=$1
		shift
		;;
	esac
done
[ -n "$n" ] || usage

# section <heading>: the body (stdin) under `### <heading>`, up to the next
# heading of level 1-3 outside a code fence, with CRs and the surrounding
# blank lines removed. Headings inside fences are content, not boundaries.
section() {
	awk -v want="### $1" '
		{ sub(/\r$/, "") }
		!fence && /^##?#? / {
			h = $0
			sub(/[ \t]+$/, "", h)
			on = (h == want)
			next
		}
		/^ ? ? ?(```|~~~)/ { fence = !fence }
		on { line[++nl] = $0 }
		END {
			first = 1
			while (first <= nl && line[first] ~ /^[ \t]*$/) first++
			while (nl >= first && line[nl] ~ /^[ \t]*$/) nl--
			for (i = first; i <= nl; i++) print line[i]
		}'
}

# split_done <part>: a section's text (stdin) around its last `Done when:` line
# outside a fence: `before` prints what precedes it, `after` the list itself,
# both without surrounding blank lines. With no such line, `before` is all of
# it and `after` is empty.
split_done() {
	awk -v part="$1" '
		{ line[++nl] = $0 }
		/^ ? ? ?(```|~~~)/ { fence = !fence; next }
		!fence && /^(\*\*)?Done when:?(\*\*)?:?[ \t]*$/ { at = nl }
		function out(a, b,   i) {
			while (a <= b && line[a] ~ /^[ \t]*$/) a++
			while (b >= a && line[b] ~ /^[ \t]*$/) b--
			for (i = a; i <= b; i++) print line[i]
		}
		END {
			if (part == "before") out(1, at ? at - 1 : nl)
			else if (at) out(at + 1, nl)
		}'
}

# defuse: the text (stdin) with each GitHub closing keyword that is followed
# by an issue number (`Closes #12`, `fixed: #3`) turned into `Closes issue #12`.
# Copied issue text often names another issue's Closes line, and in a commit
# that lands on main, or in a PR body, GitHub would close that issue too. The
# block's own Closes lines are added after this, so they still close.
#
# GitHub's skip directives (`[skip ci]`, `[ci skip]`, `[no ci]`,
# `[skip actions]`, `[actions skip]`) become `(skip ci)` and so on: GitHub
# honours them anywhere in the head commit's message, so an issue that only
# mentions one would skip every workflow on the PR, the required checks
# included (#460 shipped that way). A `skip-checks:` trailer needs no
# defusing, since the block always sits before the trailers.
defuse() {
	sed -E -e 's/(^|[^[:alnum:]_])(close[sd]?|fix(e[sd])?|resolve[sd]?)(:?[[:space:]]+)#([0-9])/\1\2\4issue #\5/Ig' \
		-e 's/\[(skip ci|ci skip|no ci|skip actions|actions skip)\]/(\1)/Ig'
}

# render <json>: sets title, scope and donewhen from one issue's {title, body}.
render() {
	local body
	title=$(jq -r '.title | sub("^\\[(Task|Bug|Decision)\\]: *"; "")' <<<"$1" | defuse)
	body=$(jq -r '.body // ""' <<<"$1" | defuse)
	scope=$(section "What needs to be built" <<<"$body")
	donewhen=$(section "Done when" <<<"$body")
	if [ -z "$scope" ] || [ -z "$donewhen" ]; then
		local expected
		expected=$(section "Expected behavior" <<<"$body")
		[ -n "$scope" ] || scope=$(split_done before <<<"$expected")
		[ -n "$donewhen" ] || donewhen=$(split_done after <<<"$expected")
	fi
}

closes() {
	case "$omit" in
	*" $1 "*) ;;
	*) printf '\n\nCloses #%s' "$1" ;;
	esac
}

json=$(gh issue view "$n" --json title,body)
render "$json"

out="<!-- ship:issue #$n -->"$'\n'"## Issue #$n: $title"
[ -z "$scope" ] || out+=$'\n\n'"$scope"
[ -z "$donewhen" ] || out+=$'\n\n'"### Done when (from #$n)"$'\n'"$donewhen"

if jq -e -L "$skills" 'include "lib"; .title | bundle_title' <<<"$json" >/dev/null; then
	children=$(gh api "repos/{owner}/{repo}/issues/$n/sub_issues?per_page=100" \
		--jq '.[] | select(.state == "open") | .number')
	for m in $children; do
		child=$(gh issue view "$m" --json title,body)
		render "$child"
		out+=$'\n\n'"### Sub-issue #$m: $title"
		[ -z "$donewhen" ] || out+=$'\n'"$donewhen"
		out+=$(closes "$m")
	done
	render "$json"
fi

out+=$(closes "$n")
out+=$'\n'"<!-- /ship:issue -->"

printf '%s\n%s\n' "$title" "$out"
