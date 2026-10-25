#! /bin/bash
# Validates GH issue #291: after a root restart, /run-chain can tell whether
# the old root still runs, and so may still drive a worker, before it sends
# "Resume".
#
# Drives .claude/skills/run-chain/root-alive.sh against a stand-in root: a
# sleep process, copied to a name holding ") " so /proc/<pid>/stat's comm
# field can't shift the start time.
#   1. --self prints `root: <pid> <start>` from CLAUDE_PID and /proc.
#   2. That pair while the process runs: live (exit 1). With the pid but a
#      different start (the pid reused): gone (exit 0). With CLAUDE_PID the
#      same pid: self (exit 0). Once the process exits: gone (exit 0).
#   3. Bad arguments, or --self without CLAUDE_PID, exit 2.
#
# What it can't prove: that a real root's PID behaves this way. That is the
# real-run check in #291's commit message (a running root reads live, and
# one whose session ended reads gone).

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR" || exit 2
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="/run-chain's root-alive.sh reports the old root live while its PID and start time still run, and gone once it exits (#291)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="sleep; /proc"
VALIDATE_DURATION="~1s"

validate_parse_args "$@"

require_cmd sleep
[ -d /proc/self ] || _prereq_missing "/proc (procfs)"
check_prereqs

ALIVE="$ROOT_DIR/.claude/skills/run-chain/root-alive.sh"

WORK_DIR="$(mktemp -d)"
ROOT=
cleanup() {
	[ -n "$ROOT" ] && kill "$ROOT" 2>/dev/null
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

NAME="r) 1"
cp "$(command -v sleep)" "$WORK_DIR/$NAME"
"$WORK_DIR/$NAME" 60 &
ROOT=$!
# Wait for the exec to land: until then the child is still a bash fork.
for _ in $(seq 50); do
	[ "$(cat "/proc/$ROOT/comm" 2>/dev/null)" = "$NAME" ] && break
	sleep 0.1
done
START=$(sed "s/.*) //" "/proc/$ROOT/stat" | cut -d" " -f20)

# probe [args...] — runs the script with CLAUDE_PID unset; sets RC and OUT.
probe() {
	OUT=$(env -u CLAUDE_PID "$ALIVE" "$@" 2>&1)
	RC=$?
}

OUT=$(CLAUDE_PID=$ROOT "$ALIVE" --self 2>&1)
RC=$?
check_eq "--self: exit 0" 0 "$RC"
check_eq "  and it prints the pid and its start time" "root: $ROOT $START" "$OUT"

probe "$ROOT" "$START"
check_eq "the root's pid and start, running: exit 1" 1 "$RC"
check "  and it says live" grep -q '^verdict: live' <<<"$OUT"

probe "$ROOT" "$((START + 1))"
check_eq "the pid with another start (reused): exit 0" 0 "$RC"
check "  and it says reused" grep -q "^root: pid $ROOT was reused" <<<"$OUT"

OUT=$(CLAUDE_PID=$ROOT "$ALIVE" "$ROOT" "$START" 2>&1)
RC=$?
check_eq "the pid is this process's: exit 0" 0 "$RC"
check "  and it says self" grep -q '^verdict: self' <<<"$OUT"

kill "$ROOT"
wait "$ROOT" 2>/dev/null
ROOT_GONE=$ROOT
ROOT=
probe "$ROOT_GONE" "$START"
check_eq "after the root exits: exit 0" 0 "$RC"
check "  and it says not running" grep -q "^root: pid $ROOT_GONE is not running" <<<"$OUT"

probe "$ROOT_GONE"
check_eq "one argument: exit 2" 2 "$RC"
probe "abc" "$START"
check_eq "a malformed pid: exit 2" 2 "$RC"
probe --self
check_eq "--self without CLAUDE_PID: exit 2" 2 "$RC"

summary
