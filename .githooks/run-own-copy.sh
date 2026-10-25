# shellcheck shell=bash
# Every hook in .githooks sources this first (#279), so one hand-off covers
# them all. A hook added later needs only this line, which skips the hand-off
# when the helper isn't beside it (the hook then checks for itself):
#
#   [ ! -f "${0%/*}/run-own-copy.sh" ] || . "${0%/*}/run-own-copy.sh"
#
# It runs the copy of the current hook that the checkout git is working in
# has. The repo sets core.hooksPath to the relative `.githooks`, which resolves
# per worktree, but Claude Code's worktree setup rewrites the shared config to
# the absolute <main checkout>/.githooks, so git starts the main checkout's
# copy from every worktree. This file then execs the worktree's own copy with
# the same args, and stdin is inherited.
#
# The hand-off happens only when the copy git starts sources this file. If the
# main checkout is on a branch from before #279, or its hook has been edited to
# drop the line, that copy checks every worktree's push itself.
#
# `-ef` (same file, symlinks followed; a bash builtin, so no realpath) is the
# recursion guard: the copy handed to finds itself and carries on. A checkout
# with no executable copy of its own (an older branch) gets the running one.
if _top=$(git rev-parse --show-toplevel 2>/dev/null); then
	_own="$_top/.githooks/${0##*/}"
	if [ -x "$_own" ]; then
		[ "$_own" -ef "$0" ] || exec "$_own" "$@"
	elif [ -e "$_own" ]; then
		echo "${0##*/}: $_own is not executable, so $0 runs instead." >&2
	fi
	unset _own
fi
unset _top
