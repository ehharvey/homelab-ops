#!/usr/bin/env bash
# One git worktree per issue, so Claude sessions and subagents never work in
# the operator's main checkout, and finished worktrees don't pile up (#252).
#
#   scripts/worktree.sh new <n>   # create or reuse .claude/worktrees/issue-<n>
#                                 # on eharvey/#<n>; prints its path on stdout
#   scripts/worktree.sh list      # every linked worktree and what gc would do
#   scripts/worktree.sh gc        # remove the ones whose work has landed
#
# gc only removes a worktree when nothing in it can be lost: it is clean, and
# either its PR merged at the commit it has checked out, or it has no commits
# that aren't already on main. Everything else is kept and reported with the
# reason, including a PR closed unmerged, since that may be work to resume.
# Locked worktrees (Claude Code locks a subagent's while it runs) and the one
# you're standing in are never touched.
#
# No per-worktree setup is needed: core.hooksPath is the relative `.githooks`
# (see `make hooks`), so every worktree runs its own copy of the hooks.
set -euo pipefail

# The main checkout, even when run from inside a worktree, so `new` never
# nests a worktree inside another one.
root=$(dirname "$(git rev-parse --path-format=absolute --git-common-dir)")
here=$(git rev-parse --show-toplevel)

usage() {
	sed -n '4,8p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

cmd_new() {
	local n="${1:-}"
	[[ "$n" =~ ^[0-9]+$ ]] || { echo "worktree: new needs an issue number (make wt N=<n>)." >&2; exit 2; }
	local path="$root/.claude/worktrees/issue-$n" branch="eharvey/#$n"

	if [ -d "$path" ]; then
		local on
		on=$(git -C "$path" branch --show-current)
		[ "$on" = "$branch" ] || echo "worktree: note: $path is on '$on', not $branch." >&2
		echo "$path"
		return
	fi

	git -C "$root" fetch -q origin main
	if git -C "$root" show-ref --verify --quiet "refs/heads/$branch"; then
		git -C "$root" worktree add "$path" "$branch" >&2
	elif git -C "$root" ls-remote --exit-code --heads origin "$branch" >/dev/null; then
		# Resuming a pushed branch, e.g. from a fresh container.
		git -C "$root" fetch -q origin "+refs/heads/$branch:refs/remotes/origin/$branch"
		git -C "$root" worktree add --track -b "$branch" "$path" "origin/$branch" >&2
	else
		git -C "$root" worktree add -b "$branch" "$path" FETCH_HEAD >&2
	fi
	echo "$path"
}

# verdict <path> <head-sha> <branch-or-empty> — prints "remove|keep<TAB>reason".
verdict() {
	local path="$1" head="$2" branch="$3"

	[ "$path" = "$here" ] && { printf 'keep\tcurrent worktree\n'; return; }
	[ -n "$(git -C "$path" status --porcelain)" ] && { printf 'keep\tuncommitted changes\n'; return; }

	if [ -n "$branch" ]; then
		local pr
		pr=$(gh pr list --head "$branch" --state all --limit 1 \
			--json number,state,headRefOid --jq '.[0] // empty | "\(.number) \(.state) \(.headRefOid)"')
		if [ -n "$pr" ]; then
			local num state oid
			read -r num state oid <<<"$pr"
			case "$state" in
			MERGED)
				if [ "$oid" = "$head" ]; then
					printf 'remove\tPR #%s merged\n' "$num"
				else
					printf 'keep\tPR #%s merged, but local %s differs from its head\n' "$num" "${head:0:7}"
				fi
				return
				;;
			OPEN) printf 'keep\tPR #%s open\n' "$num"; return ;;
			*) printf 'keep\tPR #%s closed unmerged; remove by hand if abandoned\n' "$num"; return ;;
			esac
		fi
	fi

	local ahead
	ahead=$(git -C "$root" rev-list --count "FETCH_HEAD..$head")
	if [ "$ahead" -eq 0 ]; then
		printf 'remove\tnothing beyond main\n'
	else
		printf 'keep\t%s unshipped commit(s), no PR\n' "$ahead"
	fi
}

# walk <apply:0|1> — one line per linked worktree: action, path, branch, reason.
walk() {
	local apply="$1"
	git -C "$root" fetch -q origin main

	local path="" head="" branch="" flag="" first=1
	# A blank line ends each record; the trailing echo flushes the last one.
	while IFS= read -r line; do
		case "$line" in
		"worktree "*) path="${line#worktree }" ;;
		"HEAD "*) head="${line#HEAD }" ;;
		"branch "*) branch="${line#branch refs/heads/}" ;;
		locked*) flag=locked ;;
		prunable*) flag=prunable ;;
		"")
			if [ "$first" = 1 ]; then
				first=0 # the main checkout
			elif [ -n "$path" ]; then
				local action reason
				case "$flag" in
				prunable) action=prune reason="directory is gone" ;;
				locked) action=keep reason="locked (in use)" ;;
				*) IFS=$'\t' read -r action reason < <(verdict "$path" "$head" "$branch") ;;
				esac
				if [ "$apply" = 1 ] && [ "$action" = remove ]; then
					git -C "$root" worktree remove "$path"
					[ -n "$branch" ] && git -C "$root" branch -q -D "$branch"
					action=removed
				elif [ "$apply" = 1 ] && [ "$action" = prune ]; then
					action=pruned # by the `worktree prune` below
				elif [ "$apply" = 0 ] && [ "$action" != keep ]; then
					action="would-$action"
				fi
				printf '%-14s %s  %s  (%s)\n' "$action" "${path#"$root"/}" "${branch:-detached}" "$reason"
			fi
			path="" head="" branch="" flag=""
			;;
		esac
	done < <(git -C "$root" worktree list --porcelain; echo)

	[ "$apply" = 1 ] && git -C "$root" worktree prune
	return 0
}

case "${1:-}" in
new) shift; cmd_new "$@" ;;
list) walk 0 ;;
gc) walk 1 ;;
*) usage ;;
esac
