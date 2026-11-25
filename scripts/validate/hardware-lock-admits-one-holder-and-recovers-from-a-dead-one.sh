#! /bin/bash
# Validates GH issue #665: the hardware lock (lib-hardware-lock.sh), which keeps
# two runs of the incus/incus-vm scripts off the shared Incus server at once,
# and run.sh's use of it.
#
# Runs against the real Incus remote, through the incus CLI as people run it
# from the devcontainer, on a private lock (VALIDATE_HARDWARE_LOCK_NAME=
# validate-hwlock-proof-<pid>), so it never touches the real one, which run.sh
# may hold while it runs this. The heartbeat is cut to 1s, each Incus call's
# timeout to 4s and the stale period to 20s (the least the lock accepts with
# those is 19s), so a takeover takes seconds, not five minutes. A test barrier
# (_HWLOCK_TEST_BARRIER) holds the acquirers back until all are ready, so their
# creates really race.
#
# ASSERTED:
#   1. Of several concurrent acquirers exactly one wins; each loser confirms
#      its lost create (a raw 500, §25) by re-reading, looks at the lock
#      again, and exits 2 once it sees the winner's heartbeat move.
#   2. A refused caller is told the holder: its generation and pid.
#   3. Release marks the generation released, and the next acquirer wins the
#      next generation without waiting out the stale period.
#   4. A SIGKILLed holder's lock is taken over, but only once its heartbeat has
#      stood still for the stale period.
#   5. Of several concurrent takeovers of a dead holder, exactly one wins.
#   6. After several cycles at most two lock profiles exist: the newest two.
#   7. A holder sent SIGTERM releases the lock once its command has ended.
#   8. run.sh, on a copy of the harness with stub scripts: while the lock is
#      held it starts no script, names the holder and exits 2, and so does the
#      real `make validate-hardware`; VALIDATE_HARDWARE_LOCK_HELD naming the
#      current generation lets the holder's own run through, and naming another
#      doesn't; --list, --describe, other groups and a run whose incus scripts
#      all skip under VALIDATE_OS never contact the lock's remote, while an
#      unreachable remote is a note, not a refusal; of two runs started
#      together, one runs and the other is refused. The real `make` call runs
#      only once r1 is seen still holding the lock, with a stale period it
#      can't reach inside its timeout, so it can never start the real suite.
#   9. A parallel run.sh (--jobs 2) sent SIGINT ends its running scripts and
#      waits for them before it exits and releases the lock.
#  10. An Incus error that isn't an unreachable remote (here a project that
#      doesn't exist, as a restricted certificate would see) refuses (2)
#      rather than reading as unreachable (3).
#
# Records the remote's cluster membership: the lock assumes one database
# member, and this proof's evidence is only for the server it ran against.
#
# Leaves nothing behind: every generation of the private lock is deleted on
# exit.

# Its helpers are passed by name to check and wait_for, which shellcheck
# can't follow.
# shellcheck disable=SC2329

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SUITE="$ROOT_DIR/scripts/validate"
# shellcheck source=scripts/validate/lib.sh
. "$SUITE/lib.sh"

VALIDATE_PROVES="the hardware lock on the Incus server admits one holder, names it to the refused, frees on release, recovers from a SIGKILLed holder, and gates run.sh and make validate-hardware (#665)"
VALIDATE_GROUP="incus"
VALIDATE_NEEDS="an Incus remote (the incus CLI), jq, timeout, make"
VALIDATE_DURATION="~1.5m"
VALIDATE_SUPPORTED_OS="any"

validate_parse_args "$@"

require_cmd incus jq timeout make
REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
require_incus_remote "$REMOTE"
check_prereqs

echo "remote $REMOTE: $(incus query "$REMOTE:/1.0" 2>/dev/null | jq -r '"clustered=\(.environment.server_clustered)"') members=$(incus query "$REMOTE:/1.0/cluster/members" 2>/dev/null | jq -r 'length' 2>/dev/null || echo 0)"

# run.sh exports the real lock's generation to the scripts it starts; this
# proof's own run.sh calls must not inherit it.
unset VALIDATE_HARDWARE_LOCK_HELD _HWLOCK_TEST_BARRIER
export VALIDATE_HARDWARE_LOCK_NAME="validate-hwlock-proof-$$"
export VALIDATE_HARDWARE_LOCK_HEARTBEAT=1
export VALIDATE_HARDWARE_LOCK_CALL_TIMEOUT=4
export VALIDATE_HARDWARE_LOCK_STALE=20
BASE="$VALIDATE_HARDWARE_LOCK_NAME"
STALE="$VALIDATE_HARDWARE_LOCK_STALE"
LOCK="$SUITE/lib-hardware-lock.sh"
N=5

WORK="$(mktemp -d)"

lock_gens() {
	incus query "$REMOTE:/1.0/profiles?project=default&recursion=1" 2>/dev/null |
		jq -r --arg p "$BASE-" '[.[] | select(.name | startswith($p)) | .name | ltrimstr($p) | tonumber] | sort | .[]'
}

profile_key() {
	incus query "$REMOTE:/1.0/profiles/$BASE-$1?project=default" 2>/dev/null |
		jq -r --arg k "$2" '.config[$k] // ""'
}

cleanup() {
	local d g
	rm -f "$WORK"/*.flag
	for d in "$WORK"/race*; do
		[ -d "$d" ] && touch "$d/go"
	done
	wait
	for g in $(lock_gens); do
		incus query -X DELETE "$REMOTE:/1.0/profiles/$BASE-$g?project=default" >/dev/null 2>&1
	done
	rm -rf "$WORK"
}
trap cleanup EXIT

# hold_bg <tag> [VAR=value...] — a holder in the background. It holds until
# $WORK/<tag>.flag is removed; its output is <tag>.log, its exit code <tag>.rc.
# The subshell's own stderr is dropped: it only reports the SIGKILLs below.
hold_bg() {
	local tag="$1"
	shift
	touch "$WORK/$tag.flag"
	(
		# shellcheck disable=SC2016 # $1 is the sh -c body's own argument
		env "$@" "$LOCK" hold -- sh -c 'while [ -e "$1" ]; do sleep 0.2; done' sh "$WORK/$tag.flag" \
			>"$WORK/$tag.log" 2>&1
		echo "$?" >"$WORK/$tag.rc"
	) 2>/dev/null &
}

# wait_for <seconds> <cmd...> — poll cmd every 0.2s.
wait_for() {
	local n=$(($1 * 5))
	shift
	until "$@"; do
		n=$((n - 1))
		[ "$n" -gt 0 ] || return 1
		sleep 0.2
	done
}

# lacks <text> <pattern> — <text> has no line matching <pattern>.
lacks() { ! grep -q "$2" <<<"$1"; }

arrived() {
	local got
	got="$(find "$1" -name 'arrived.*' | wc -l)"
	[ "$got" -ge "$2" ]
}

acquired() {
	grep -q "^hardware lock: acquired $BASE-$2 on " "$WORK/$1.log" 2>/dev/null
}

# outcome <prefix> — "<winners> <refused> <lost the create race>" among
# <prefix>1..<prefix>N; settled <prefix> once every one is a winner or refused.
outcome() {
	local w=0 r=0 lost=0 i
	for i in $(seq 1 "$N"); do
		if grep -q '^hardware lock: acquired ' "$WORK/$1$i.log" 2>/dev/null; then
			w=$((w + 1))
		elif [ "$(cat "$WORK/$1$i.rc" 2>/dev/null)" = 2 ]; then
			r=$((r + 1))
			grep -q '^hardware lock: lost the race for ' "$WORK/$1$i.log" && lost=$((lost + 1))
		fi
	done
	echo "$w $r $lost"
}

settled() {
	local w r lost
	read -r w r lost <<<"$(outcome "$1")"
	[ $((w + r)) -eq "$N" ]
}

winner() {
	local i
	for i in $(seq 1 "$N"); do
		grep -q '^hardware lock: acquired ' "$WORK/$1$i.log" 2>/dev/null && echo "$1$i"
	done
}

# race <prefix> <seconds> — release a barrier of N holders and wait for them.
race() {
	wait_for "$2" arrived "$WORK/race-$1" "$N" ||
		echo "   only $(find "$WORK/race-$1" -name 'arrived.*' | wc -l) of $N reached the barrier"
	touch "$WORK/race-$1/go"
	wait_for 60 settled "$1"
}

echo "== 1. Of $N concurrent acquirers, exactly one wins =="
mkdir "$WORK/race-a"
for i in $(seq 1 "$N"); do hold_bg "a$i" _HWLOCK_TEST_BARRIER="$WORK/race-a"; done
race a 60
read -r w r lost <<<"$(outcome a)"
check_eq "exactly one acquirer won" 1 "$w"
check_eq "the other $((N - 1)) were refused (exit 2)" $((N - 1)) "$r"
check_eq "every loser lost the create and confirmed it by re-reading" $((N - 1)) "$lost"
looked=0
for i in $(seq 1 "$N"); do
	grep -q "^hardware lock: $BASE-1 is unreleased; watching" "$WORK/a$i.log" 2>/dev/null && looked=$((looked + 1))
done
check_eq "and then looked again, watching the winner's heartbeat before refusing" $((N - 1)) "$looked"
A_WIN="$(winner a)"
check "the winner holds generation 1" acquired "${A_WIN:-none}" 1

echo
echo "== 2. A refused caller sees the holder =="
holder_pid="$(profile_key 1 user.pid)"
start=$SECONDS
out="$("$LOCK" hold -- true 2>&1)"
rc=$?
elapsed=$((SECONDS - start))
check_eq "a caller is refused while the lock is held (exit 2)" 2 "$rc"
check "the refusal names generation 1 and the holder's pid ($holder_pid)" \
	grep -q "^hardware lock: held: $BASE-1 by .* pid $holder_pid since " <<<"$out"
check "the recorded pid is the winning holder's, still alive" kill -0 "${holder_pid:-0}"
check "it refused on seeing the heartbeat move, inside the stale period (${elapsed}s < ${STALE}s)" \
	[ "$elapsed" -lt "$STALE" ]

echo
echo "== 3. Release frees the lock for the next generation =="
rm -f "$WORK/$A_WIN.flag"
wait_for 20 test -e "$WORK/$A_WIN.rc"
check_eq "the holder exits with its command's code" 0 "$(cat "$WORK/$A_WIN.rc" 2>/dev/null)"
check_eq "release set user.released=true on generation 1" true "$(profile_key 1 user.released)"
start=$SECONDS
out="$("$LOCK" hold -- true 2>&1)"
rc=$?
elapsed=$((SECONDS - start))
check_eq "the next acquirer wins" 0 "$rc"
check "at generation 2" grep -q "^hardware lock: acquired $BASE-2 on " <<<"$out"
check "without waiting out the stale period (${elapsed}s < ${STALE}s)" [ "$elapsed" -lt "$STALE" ]

echo
echo "== 4. A SIGKILLed holder is recovered after the stale period =="
hold_bg k1
wait_for 20 acquired k1 3
kpid="$(profile_key 3 user.pid)"
check "a holder took generation 3 (pid ${kpid:-none})" [ -n "$kpid" ]
kill -9 "${kpid:-0}" 2>/dev/null
rm -f "$WORK/k1.flag"
start=$SECONDS
out="$("$LOCK" hold -- true 2>&1)"
rc=$?
elapsed=$((SECONDS - start))
check_eq "a waiter takes over the killed holder's lock" 0 "$rc"
check "at generation 4" grep -q "^hardware lock: acquired $BASE-4 on " <<<"$out"
check "only once its heartbeat stood still for the stale period (${elapsed}s >= ${STALE}s)" \
	[ "$elapsed" -ge "$STALE" ]
check_eq "the killed holder's generation 3 was never released" "" "$(profile_key 3 user.released)"

echo
echo "== 5. Of $N concurrent takeovers, exactly one wins =="
hold_bg k2
wait_for 20 acquired k2 5
kpid="$(profile_key 5 user.pid)"
kill -9 "${kpid:-0}" 2>/dev/null
rm -f "$WORK/k2.flag"
mkdir "$WORK/race-t"
for i in $(seq 1 "$N"); do hold_bg "t$i" _HWLOCK_TEST_BARRIER="$WORK/race-t"; done
race t $((STALE + 60))
read -r w r lost <<<"$(outcome t)"
check_eq "exactly one waiter took over" 1 "$w"
check_eq "the other $((N - 1)) were refused (exit 2)" $((N - 1)) "$r"
check_eq "every loser lost the create and confirmed it by re-reading" $((N - 1)) "$lost"
T_WIN="$(winner t)"
check "the winner took generation 6" acquired "${T_WIN:-none}" 6
check "the winner logged the takeover" grep -q "heartbeat stood still" "$WORK/${T_WIN:-none}.log"
rm -f "$WORK"/t*.flag
wait_for 20 test -e "$WORK/${T_WIN:-none}.rc"

echo
echo "== 6. At most two lock profiles after several cycles =="
ok=0
for _ in 1 2 3; do "$LOCK" hold -- true >/dev/null 2>&1 && ok=$((ok + 1)); done
check_eq "three more acquire/release cycles each won" 3 "$ok"
check_eq "only the newest two generations remain" "8 9" "$(lock_gens | tr '\n' ' ' | sed 's/ $//')"
check_eq "the newest is released" true "$(profile_key 9 user.released)"

echo
echo "== 7. A signal releases the lock =="
# bash runs a trap once the foreground command has ended, so the holder's
# command (the flag loop) is ended after the signal is sent.
hold_bg s1
wait_for 20 acquired s1 10
spid="$(profile_key 10 user.pid)"
kill -TERM "${spid:-0}" 2>/dev/null
sleep 1
check_eq "a holder sent SIGTERM keeps the lock while its command runs" "" "$(profile_key 10 user.released)"
rm -f "$WORK/s1.flag"
wait_for 20 test -e "$WORK/s1.rc"
check_eq "then exits 143" 143 "$(cat "$WORK/s1.rc" 2>/dev/null)"
check_eq "having released generation 10" true "$(profile_key 10 user.released)"

echo
echo "== 8. run.sh takes the lock, and only when it must =="
# A copy of the harness, so run.sh's own logic runs without starting the real
# hardware scripts: stub-hw is group incus; stub-gated is group incus-vm but
# supports no backend VALIDATE_OS can name, so the gate skips it; stub-none is
# group none.
COPY="$WORK/suite"
MARKS="$WORK/marks"
mkdir -p "$COPY" "$MARKS"
cp "$SUITE/run.sh" "$SUITE/lib.sh" "$LOCK" "$COPY/"
stub() {
	cat >"$COPY/$1.sh" <<EOF
#! /bin/bash
. "\$(dirname "\$0")/lib.sh"
VALIDATE_PROVES="stub"
VALIDATE_GROUP="$2"
VALIDATE_SUPPORTED_OS="$3"
validate_parse_args "\$@"
touch "\$STUB_MARKS/$1.\$\$"
sleep "\${STUB_SLEEP:-0}"
while [ -n "\${STUB_HOLD:-}" ] && [ -e "\$STUB_HOLD" ]; do sleep 0.2; done
record_pass "stub ran"
summary
EOF
	chmod +x "$COPY/$1.sh"
}
stub stub-hw incus any
stub stub-hw2 incus any
stub stub-gated incus-vm not-a-backend
stub stub-none none any
export STUB_MARKS="$MARKS"
marks() { find "$MARKS" -name "$1.*" | wc -l; }

hold_bg r1
wait_for 20 acquired r1 11
out="$("$COPY/run.sh" --group incus,incus-vm 2>&1)"
rc=$?
check_eq "while held, run.sh --group incus,incus-vm exits 2" 2 "$rc"
check "and names the holder" grep -q "held: $BASE-11 by " <<<"$out"
check_eq "and starts no script" 0 "$(marks stub-hw)"

# Next is the real `make validate-hardware`: were the private lock free, it
# would start the real incus/incus-vm suite. So it runs only once r1 is seen
# still holding the lock, its heartbeat moving, and with a stale period it
# can't reach inside its timeout: it can be refused, or time out having
# started nothing, but never take the lock over.
r1_holds() {
	local pid hb
	[ "$(lock_gens | tail -1)" = 11 ] || return 1
	[ -z "$(profile_key 11 user.released)" ] || return 1
	pid="$(profile_key 11 user.pid)"
	kill -0 "${pid:-0}" 2>/dev/null || return 1
	hb="$(profile_key 11 user.heartbeat)"
	sleep 1.5
	[ "$(profile_key 11 user.heartbeat)" != "$hb" ]
}
if r1_holds; then
	out="$(VALIDATE_HARDWARE_LOCK_STALE=600 timeout 120 make -C "$ROOT_DIR" --no-print-directory validate-hardware 2>&1)"
	rc=$?
	check_eq "while held, make validate-hardware exits 2" 2 "$rc"
	check "and names the holder" grep -q "held: $BASE-11 by " <<<"$out"
	check "and starts no script" lacks "$out" "^======== "
else
	record_fail "make validate-hardware while held" "r1 no longer held $BASE-11 with a moving heartbeat, so the real make call was skipped"
fi

VALIDATE_HARDWARE_LOCK_HELD="$BASE-11" "$COPY/run.sh" --group incus >/dev/null 2>&1
rc=$?
check_eq "the holder's own run.sh (VALIDATE_HARDWARE_LOCK_HELD=$BASE-11) runs" 0 "$rc"
check_eq "and starts its script" 1 "$(marks stub-hw)"
out="$(VALIDATE_HARDWARE_LOCK_HELD="$BASE-999" "$COPY/run.sh" --group incus 2>&1)"
rc=$?
check_eq "VALIDATE_HARDWARE_LOCK_HELD naming another generation is refused (exit 2)" 2 "$rc"
check_eq "and starts no script" 1 "$(marks stub-hw)"

# Against a remote that doesn't exist, any attempt at the lock prints a
# "hardware lock" note; these must print none.
nolock() {
	local want="$1" out rc
	shift
	out="$(VALIDATE_INCUS_REMOTE=validate-hwlock-no-such-remote "$COPY/run.sh" "$@" 2>&1)"
	rc=$?
	check_eq "run.sh $* exits $want" "$want" "$rc"
	check "run.sh $* never contacts the lock" lacks "$out" "hardware lock"
}
nolock 0 --list --group incus,incus-vm
nolock 0 --describe --group incus,incus-vm
nolock 0 --group none
check_eq "and --group none ran its script" 1 "$(marks stub-none)"
nolock 3 --group incus-vm
out="$(VALIDATE_INCUS_REMOTE=validate-hwlock-no-such-remote "$COPY/run.sh" --group incus 2>&1)"
rc=$?
check_eq "with the lock's remote unreachable, run.sh --group incus still runs" 0 "$rc"
check "after a note saying it runs without the lock" \
	grep -q "^NOTE: hardware lock: .*running without the lock" <<<"$out"
check_eq "and starts its script" 2 "$(marks stub-hw)"

rm -f "$WORK/r1.flag"
wait_for 20 test -e "$WORK/r1.rc"

rm -f "$MARKS"/*
mkdir "$WORK/race-two"
# The winner's script holds until the loser has exited, so the loser can't
# find the lock already released and take the next generation.
touch "$WORK/two.flag"
for i in 1 2; do
	(
		STUB_HOLD="$WORK/two.flag" _HWLOCK_TEST_BARRIER="$WORK/race-two" "$COPY/run.sh" --group incus \
			>"$WORK/two$i.log" 2>&1
		echo "$?" >"$WORK/two$i.rc"
	) &
done
wait_for 60 arrived "$WORK/race-two" 2
touch "$WORK/race-two/go"
one_done() { [ -e "$WORK/two1.rc" ] || [ -e "$WORK/two2.rc" ]; }
wait_for 60 one_done
rm -f "$WORK/two.flag"
both_done() { [ -e "$WORK/two1.rc" ] && [ -e "$WORK/two2.rc" ]; }
wait_for 60 both_done
check_eq "of two run.sh started together, one runs and one is refused" "0 2" \
	"$(cat "$WORK/two1.rc" "$WORK/two2.rc" 2>/dev/null | sort | tr '\n' ' ' | sed 's/ $//')"
check_eq "only one started its script" 1 "$(marks stub-hw)"
check "the refused one named the holder" grep -q "held: $BASE-" "$WORK/two1.log" "$WORK/two2.log"
top="$(lock_gens | tail -1)"
check_eq "the run released the lock on exit" true "$(profile_key "${top:-0}" user.released)"
check "at most two lock profiles remain" [ "$(lock_gens | wc -l)" -le 2 ]

echo
echo "== 9. A signal to a parallel run.sh ends its scripts before it releases =="
rm -f "$MARKS"/*
# A background job starts with SIGINT ignored, and a script can't trap a
# signal ignored on entry; env --default-signal undoes that, so run.sh gets
# the SIGINT a Ctrl-C would send it.
STUB_SLEEP=60 env --default-signal=INT "$COPY/run.sh" --group incus --jobs 2 >"$WORK/par.log" 2>&1 &
par=$!
both_started() { [ "$(marks stub-hw)" -eq 1 ] && [ "$(marks stub-hw2)" -eq 1 ]; }
wait_for 30 both_started
check "run.sh --jobs 2 took the lock and started both scripts" both_started
stub_pids="$(find "$MARKS" -name 'stub-hw*' | sed 's/.*\.//')"
start=$SECONDS
kill -INT "$par"
wait "$par"
rc=$?
elapsed=$((SECONDS - start))
check_eq "sent SIGINT, it exits 130" 130 "$rc"
check "having ended its scripts, not waited them out (${elapsed}s < 30s)" [ "$elapsed" -lt 30 ]
alive=0
for p in $stub_pids; do kill -0 "$p" 2>/dev/null && alive=$((alive + 1)); done
check_eq "no script is still running once it has exited" 0 "$alive"
top="$(lock_gens | tail -1)"
check_eq "and it released the lock" true "$(profile_key "${top:-0}" user.released)"

echo
echo "== 10. An Incus error other than an unreachable remote refuses =="
# The lock's own functions, pointed at a project that doesn't exist: the server
# answers, but with an error, as it would a certificate restricted to another
# project.
out="$(
	# shellcheck source=scripts/validate/lib-hardware-lock.sh
	. "$LOCK"
	HWLOCK_PROJECT=validate-hwlock-no-such-project
	hwlock_acquire 2>/dev/null
	echo "rc=$? $HWLOCK_ERROR"
)"
check "hwlock_acquire returns 2 (an Incus error), not 3 (unreachable): $out" grep -q "^rc=2 cannot list profiles" <<<"$out"

summary
