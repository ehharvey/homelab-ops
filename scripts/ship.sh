#!/usr/bin/env bash
# Push a finished commit and open its PR (#119). Stops there — `make lgtm`
# merges (#125), so CI runs while you actually read the diff.
#
# The PR is a CI gate, not a review bottleneck: main requires 0 approvals, and
# a real approval gate isn't available to a solo dev anyway (GitHub won't let
# you approve your own PR, so requiring 1 review would deadlock everything).
# The ship/lgtm split is a deliberate speed bump, not an enforced control.
#
# `gh pr create --fill` takes the PR title from the commit subject and the PR
# body verbatim from the commit message body — which is why the ## Plan and
# ## Test plan sections live in the commit message (see docs/Development
# Conventions.md). Nothing gets retyped into a web form. --fill copies only at
# creation: re-shipping an amended commit to an open PR (#273) pushes it and
# leaves the PR's title and body alone (/address-feedback syncs them).
#
# Before any of that it makes the branch shippable (#319): a branch of several
# commits is squashed into one carrying the first commit's message, and a branch
# behind main is rebased onto it. It refuses instead when a rebase is already in
# progress, when the rebase stops on a conflict, or when another branch holds
# some of these commits (a stack, #254), and pushes nothing in each case. Not a
# git hook on purpose: pre-push can't change what it pushes, and post-commit
# also fires mid-rebase. pre-push and CI keep refusing multi-commit pushes.
#
# Then it copies the issue's details into the commit (#450), so the PR shows
# what was asked for beside what was done. The issue is SHIP_ISSUE, else the
# <n> of a branch named */#<n>; on any other branch it says so and leaves the
# message alone. scripts/issue-block.sh renders the issue: its stripped title
# replaces the subject (an author's `type(scope): ` prefix goes on purpose, so
# the PR title is the issue's), and its delimited block (scope, Done-when, a
# bundle's open sub-issues, `Closes #<n>`) goes after the author's text and
# before the trailer paragraph. A block from an earlier ship is regenerated,
# not repeated, so an edited issue flows through on re-ship; ## Plan and
# ## Test plan are never touched, and a `Closes #<n>` the author wrote isn't
# repeated. It amends with `-F`, never a bare --amend, and only when the
# message changed, so an unchanged re-ship keeps the SHA. A failed issue or
# sub-issue lookup refuses and pushes nothing. Put `[skip ci]` in the body:
# the subject gets replaced.
#
# Known gap until #281: a branch pushed earlier with no PR open that ship then
# squashes, rebases or amends is refused by the plain push below as
# non-fast-forward.
set -euo pipefail

branch=$(git branch --show-current)

# A rebase that stopped on a conflict (#319, possibly ship's own below) leaves
# HEAD detached and the tree dirty. Say how to finish it before the checks
# below misreport it as "detached" or "dirty".
if [ -e "$(git rev-parse --git-path rebase-merge)" ] || [ -e "$(git rev-parse --git-path rebase-apply)" ]; then
	echo "ship: a rebase is in progress; nothing pushed." >&2
	echo "      Resolve it, finish with \`git rebase --continue\`, then \`make ship\` again." >&2
	exit 1
fi

if [ -z "$branch" ]; then
	echo "ship: HEAD is detached; switch to your eharvey/#<issue> branch first." >&2
	exit 1
fi

if [ "$branch" = "main" ]; then
	echo "ship: on main — branch as eharvey/#<issue> first." >&2
	exit 1
fi

if [ -n "$(git status --porcelain)" ]; then
	echo "ship: working tree is dirty; commit or stash first." >&2
	git status --short >&2
	exit 1
fi

# FETCH_HEAD rather than origin/main: `git fetch origin main` always writes it,
# whereas whether origin/main advances depends on the remote's refspec.
git fetch -q origin main
main_sha=$(git rev-parse FETCH_HEAD)
mapfile -t commits < <(git rev-list --reverse "$main_sha..HEAD")
ahead=${#commits[@]}

if [ "$ahead" -eq 0 ]; then
	echo "ship: branch is 0 commits ahead of main; main takes exactly 1." >&2
	exit 1
fi

behind=0
git merge-base --is-ancestor "$main_sha" HEAD || behind=1

# Stacked branch (#319): if another branch already holds some of these commits,
# squashing or rebasing would rewrite them out from under it. Refuse rather
# than guess where the stack's base is; #254's stack-base marker can replace
# this check. A stale local branch can trip it falsely: `make wt-gc` clears it.
# Only a rewrite needs it, so a branch 1 ahead and not behind (including the
# bottom of a stack) ships exactly as before.
stacked=()
if [ "$ahead" -gt 1 ] || [ "$behind" -eq 1 ]; then
	for c in "${commits[@]}"; do
		while read -r ref; do
			case "$ref" in
			"refs/heads/$branch" | "refs/remotes/origin/$branch" | refs/remotes/origin/HEAD) ;;
			*) stacked+=("$ref") ;;
			esac
		done < <(git for-each-ref --contains "$c" --format='%(refname)' refs/heads refs/remotes/origin)
	done
fi
if [ "${#stacked[@]}" -gt 0 ]; then
	others=$(printf '%s\n' "${stacked[@]}" | sed -e 's|^refs/heads/||' -e 's|^refs/remotes/||' | sort -u | paste -sd' ' -)
	echo "ship: $branch is stacked: $others already contains some of its commits." >&2
	echo "      Nothing squashed, rebased or pushed. Rebase onto main once that lands" >&2
	echo "      (or \`make restack\`, once #254 provides it), then \`make ship\`." >&2
	exit 1
fi

# More than one commit (#319): fold them into one that carries the FIRST
# commit's message, where the ## Plan / ## Test plan / Closes #<n> body was
# written. -C takes it verbatim: -c would open an editor, and its default
# `strip` cleanup deletes the `## ...` headings as comment lines.
if [ "$ahead" -gt 1 ]; then
	old_head=$(git rev-parse HEAD)
	folded=$(git log --reverse --format='        %h %s' "$main_sha..HEAD")
	git reset -q --soft "$(git merge-base "$main_sha" HEAD)"
	git commit -q -C "${commits[0]}"
	echo "ship: squashed $ahead commits into one, keeping the first's message:"
	echo "$folded"
	echo "      (the previous head was ${old_head:0:12})"
fi

# Behind main (#319): rebase, never merge, because main is rebase-only and its
# required checks are strict. Squashing first means a conflict surfaces once.
# On a conflict the rebase is left stopped for the operator to resolve.
if [ "$behind" -eq 1 ]; then
	if ! git rebase -q "$main_sha"; then
		echo "ship: rebasing onto main stopped on a conflict; nothing pushed." >&2
		echo "      Resolve it, \`git rebase --continue\`, then \`make ship\` again." >&2
		exit 1
	fi
	echo "ship: rebased onto main (${main_sha:0:12})."
	if [ "$(git rev-list --count "$main_sha..HEAD")" -ne 1 ]; then
		echo "ship: nothing is left beyond main after the rebase; nothing pushed." >&2
		exit 1
	fi
fi

# Copy the issue's details into the commit (#450), after the squash and rebase
# so it rewrites the message that actually ships. The issue is SHIP_ISSUE, or
# the <n> of a branch named */#<n>; any other branch (the validate scripts'
# validate-*/ ones) keeps its message.
issue=""
if [ -n "${SHIP_ISSUE:-}" ]; then
	if ! [[ $SHIP_ISSUE =~ ^[0-9]+$ ]]; then
		echo "ship: SHIP_ISSUE=$SHIP_ISSUE is not an issue number; nothing pushed." >&2
		exit 1
	fi
	issue=$SHIP_ISSUE
elif [[ $branch =~ /#([0-9]+)$ ]]; then
	issue=${BASH_REMATCH[1]}
fi

# last_para head|tail: the text (stdin) before its last paragraph, or that
# paragraph itself, ignoring trailing blank lines.
last_para() {
	awk -v part="$1" '
		{ l[++n] = $0 }
		END {
			while (n > 0 && l[n] ~ /^[ \t]*$/) n--
			for (b = n; b > 0 && l[b] !~ /^[ \t]*$/; b--) ;
			if (part == "head") for (i = 1; i < b; i++) print l[i]
			else for (i = b + 1; i <= n; i++) print l[i]
		}'
}

if [ -z "$issue" ]; then
	echo "ship: no issue number in $branch; message left as is."
else
	old=$(git log -1 --format=%B)
	rest=""
	[[ $old == *$'\n'* ]] && rest=${old#*$'\n'}
	# The trailer paragraph (Co-Authored-By: etc.) stays last, where GitHub
	# and git look for it. --no-divider: a Markdown `---` rule isn't a patch.
	author=$rest
	trailers=""
	if [ -n "$(git interpret-trailers --parse --no-divider <<<"$old")" ]; then
		author=$(last_para head <<<"$rest")
		trailers=$(last_para tail <<<"$rest")
	fi
	# Drop the block an earlier ship added: from this issue's first start
	# marker to the LAST end marker, since an issue body may quote the end
	# marker itself. Only this issue's: the author may quote another's block.
	if ! author=$(awk -v start="<!-- ship:issue #$issue -->" '
		{ l[++n] = $0 }
		!s && $0 == start { s = n }
		s && $0 == "<!-- /ship:issue -->" { e = n }
		END {
			if (s && !e) exit 1
			for (i = 1; i <= n; i++) if (!s || i < s || i > e) print l[i]
		}' <<<"$author"); then
		echo "ship: the commit message has a <!-- ship:issue --> line with no <!-- /ship:issue --> after it." >&2
		echo "      Fix the message (\`git commit --amend -F <file>\`), then \`make ship\` again; nothing pushed." >&2
		exit 1
	fi
	# A Closes line the author already wrote isn't repeated: the exact,
	# case-sensitive match audit.sh uses.
	omit=()
	for k in $(grep -oE '(^|[^[:alnum:]])Closes #[0-9]+' <<<"$author"$'\n'"$trailers" | grep -oE '[0-9]+$' || true); do
		omit+=(--omit-closes "$k")
	done
	if ! details=$("$(dirname "${BASH_SOURCE[0]}")/issue-block.sh" "${omit[@]}" "$issue"); then
		echo "ship: couldn't read #$issue's details from GitHub; nothing pushed." >&2
		exit 1
	fi
	new=${details%%$'\n'*}$'\n\n'
	[ -z "$(tr -d '[:space:]' <<<"$author")" ] || new+=$author$'\n\n'
	new+=${details#*$'\n'}
	[ -z "$trailers" ] || new+=$'\n\n'$trailers
	# `-F` with whitespace cleanup keeps the `## ` lines (Development
	# Conventions § Amending keeps the sections); stripspace applies the same
	# cleanup first, so an unchanged issue compares equal and keeps the SHA.
	msg_file=$(git rev-parse --git-path SHIP_EDITMSG)
	git stripspace <<<"$new" >"$msg_file"
	if [ "$(cat "$msg_file")" != "$old" ]; then
		git commit -q --amend --cleanup=whitespace -F "$msg_file"
		echo "ship: copied #$issue's details into the commit."
	fi
	rm -f "$msg_file"
fi

# Look the PR up BEFORE pushing (#273): an amended commit needs a force-push,
# which is only safe against a PR whose head we know. `gh pr view` also returns
# a closed or merged PR for the branch, so only OPEN counts as "already open".
# A failed lookup is treated as "no PR", as before: the plain push below then
# refuses a rewritten commit as non-fast-forward rather than overwriting it.
pr=$(gh pr view --json state,headRefOid --jq '"\(.state) \(.headRefOid)"' 2>/dev/null || true)
state=${pr%% *}
head_oid=${pr##* }

case "$state" in
"")
	git push -u origin "$branch"
	gh pr create --fill
	;;
OPEN)
	# Re-ship to the open PR. The lease names the PR's head SHA explicitly,
	# not a bare --force-with-lease: that compares against origin/<branch>,
	# which a fresh `make wt` worktree may lack and a background fetch may
	# have advanced. If anything reached the branch after the head read
	# above, the push refuses instead of overwriting it.
	if ! git push -u --force-with-lease="refs/heads/$branch:$head_oid" origin "$branch"; then
		echo "ship: push refused — origin/$branch moved past the PR head ${head_oid:0:12}." >&2
		echo "      Fetch it, fold anything new into your commit, and ship again." >&2
		exit 1
	fi
	echo "ship: re-shipped to the open PR for $branch."
	;;
*)
	echo "ship: the PR for $branch is $state, not open; nothing pushed." >&2
	echo "      Reopen it, or branch afresh for new work." >&2
	exit 1
	;;
esac

gh pr view --json number,url --jq '"ship: #\(.number) open — \(.url)"'
echo "ship: review it, then \`make lgtm\` to merge."
