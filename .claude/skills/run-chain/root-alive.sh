#!/usr/bin/env bash
# Is the root that started a /run-chain worker still running (#291)? A new root
# runs this after a restart, before any "Resume" SendMessage to a ledger worker
# that hasn't handed back, because a new root can't stop another process's
# agent, and two drivers on one worker kill each other's shells (49482e4).
#
#   .claude/skills/run-chain/root-alive.sh --self          # at the start of a run
#   .claude/skills/run-chain/root-alive.sh <pid> <start>   # after a restart
#
# --self prints this root's `root: <pid> <start>` line for the ledger: Claude
# Code's PID (CLAUDE_PID, set in every Bash call) and its start time, field 22
# of /proc/<pid>/stat, so a PID reused since then doesn't read as the old root.
# Claude Code's own worktree lock records the same pair (`pid <n> start <t>`).
#
# A worker runs inside its root's process (its shells are that process's
# children), so the old root's process is the one signal: while it runs it may
# still drive the worker, whatever the worker is doing (a tool call, a
# permission prompt, an API retry); once it's gone, nothing drives the worker.
#
# Exit 0: gone (safe to resume), or this very process (not a restart).
# Exit 1: live (send nothing). Exit 2: usage.
set -euo pipefail

usage() {
	echo "usage: root-alive.sh --self | <pid> <start>" >&2
	exit 2
}

# start_of <pid>: the process's start time, or nothing if it doesn't exist.
# Fields are counted after the last ')', since comm may hold spaces.
start_of() {
	local stat
	stat=$(cat "/proc/$1/stat" 2>/dev/null) || return 0
	stat=${stat##*) }
	# shellcheck disable=SC2086 # split into fields on purpose
	set -- $stat
	echo "${20}"
}

if [ "${1:-}" = "--self" ] && [ $# -eq 1 ]; then
	[ -n "${CLAUDE_PID:-}" ] || {
		echo "root-alive.sh: CLAUDE_PID is unset; run this from Claude Code's Bash tool" >&2
		exit 2
	}
	echo "root: $CLAUDE_PID $(start_of "$CLAUDE_PID")"
	exit 0
fi

if [ $# -ne 2 ] || [[ ! $1 =~ ^[0-9]+$ || ! $2 =~ ^[0-9]+$ ]]; then
	usage
fi
pid=$1 start=$2

now=$(start_of "$pid")
if [ -z "$now" ]; then
	echo "root: pid $pid is not running"
	echo "verdict: gone"
	exit 0
fi
if [ "$now" != "$start" ]; then
	echo "root: pid $pid was reused (start $now, ledger says $start)"
	echo "verdict: gone"
	exit 0
fi
if [ "$pid" = "${CLAUDE_PID:-}" ]; then
	echo "root: pid $pid is this process"
	echo "verdict: self (not a restart; § 3's sends apply)"
	exit 0
fi
echo "root: pid $pid running since start $start ($(cat "/proc/$pid/comm" 2>/dev/null))"
echo "verdict: live (send nothing; park and ask the operator)"
exit 1
