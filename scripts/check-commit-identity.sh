#!/usr/bin/env bash
# Do these commits carry only the identities main allows (#451)? A human
# commit's author and committer use GitHub's noreply address; a bot's use its
# own `<id>+<name>[bot]@users.noreply.github.com`. The personal address that
# main's history was rewritten to remove must not come back.
#
#   check-commit-identity.sh <range>...
#
# <range> is anything `git rev-list` takes: `A..B`, or `-1 <sha>` for one
# commit. Three places run it: .githooks/pre-push on the commits a push adds
# (validate-*/ branches exempt), the required `one-commit` job
# (.github/workflows/pr-shape.yml) on the PR's commit, and ci.yml's
# `landed-identity` job on what a push to main landed, since GitHub re-commits
# a rebase landing as whoever landed it.
#
# Prints `<short sha> <author|committer> <email>` per offending identity.
# Exit: 0 all allowed; 1 something isn't; 2 usage, or a bad range.
set -euo pipefail

NOREPLY="36806437+ehharvey@users.noreply.github.com"

[[ $# -ge 1 ]] || {
	echo "usage: $0 <range>..." >&2
	exit 2
}

# git's own error goes to stderr too: it says why the range is bad.
if ! log=$(git log --no-show-signature --format='%h %ae %ce' "$@" --); then
	echo "$0: git log $* failed" >&2
	exit 2
fi

# A bot's address is GitHub's `<id>+<name>[bot]@users.noreply.github.com`
# exactly, so a stray `x[bot]@users.noreply.github.com` user.email isn't one.
bot_re='^[0-9]+\+[^@]+\[bot\]@users\.noreply\.github\.com$'
allowed() {
	[[ $1 == "$NOREPLY" || $1 =~ $bot_re ]]
}

bad=0
while read -r sha ae ce; do
	[[ -n $sha ]] || continue
	allowed "$ae" || {
		echo "$sha author $ae"
		bad=1
	}
	allowed "$ce" || {
		echo "$sha committer $ce"
		bad=1
	}
done <<<"$log"

if [[ $bad -ne 0 ]]; then
	cat >&2 <<-EOF

		Commits on main use $NOREPLY (bots: their
		[bot]@users.noreply.github.com address), docs/Development Conventions.md
		§ Branching & issues. Set it with .devcontainer/scripts/5-set-git-identity.sh,
		then re-author the commit:

		    git commit --amend --no-edit --reset-author
	EOF
	exit 1
fi
