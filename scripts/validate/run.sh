#! /bin/bash
# Run the validation suite, or a group of it (#140).
#
#   run.sh                                        every script that can run here
#   run.sh --group compose                        one group
#   run.sh --group compose --jobs 4               in parallel
#   run.sh --group compose --strict --allow-skip base-image
#   run.sh --list --group compose                 names only
#   run.sh --list --group compose --json          CI's job list
#   run.sh --describe                             the prerequisite matrix
#   VALIDATE_OS=incusos run.sh                    the host-OS backend (default incusos)
#
# Groups come from each script's own VALIDATE_GROUP declaration, read via
# --describe, rather than a list maintained here. A suite whose coverage depends
# on remembering to update a central list is exactly the rot this exists to stop
# — the same reasoning as lint-mermaid.sh globbing docs/*.md.
#
# Exit codes aggregate the children's (see lib.sh): any 1 → 1; else any 2 → 2;
# else any 3 → 3; else 0. So "something is broken" always outranks "something
# couldn't run", which always outranks "everything that ran, passed".
#
# An unknown VALIDATE_OS exits 2 here, once, before any script is asked
# anything; lib.sh holds the list of known backends (#486).
#
# A run that would start an incus/incus-vm script the VALIDATE_OS gate lets run
# takes the hardware lock on the Incus server first (#665, lib-hardware-lock.sh).
# Held by another: no script starts, the holder is named, exit 2. A caller that
# holds it says so with VALIDATE_HARDWARE_LOCK_HELD=<generation>. Remote
# unreachable: a note, and the run goes ahead without it. Any other Incus error
# (a permission error, say): no script starts, exit 2.

set -uo pipefail

SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$SUITE_DIR/lib.sh"
# shellcheck source=scripts/validate/lib-hardware-lock.sh
. "$SUITE_DIR/lib-hardware-lock.sh"
HWLOCK_COMMAND="run.sh $*"

GROUP=""
JOBS=1
LIST=0
JSON=0
DESCRIBE=0
PASSTHRU=()

while [ $# -gt 0 ]; do
	case "$1" in
	--group)
		GROUP="$2"
		shift
		;;
	--group=*) GROUP="${1#*=}" ;;
	--jobs)
		JOBS="$2"
		shift
		;;
	--jobs=*) JOBS="${1#*=}" ;;
	--list) LIST=1 ;;
	--json) JSON=1 ;;
	--describe) DESCRIBE=1 ;;
	--strict) PASSTHRU+=("--strict") ;;
	--allow-skip)
		PASSTHRU+=("--allow-skip" "$2")
		shift
		;;
	--allow-skip=*) PASSTHRU+=("$1") ;;
	-h | --help)
		sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 2
		;;
	esac
	shift
done

validate_os_known || exit 2

# describe_field <describe output> <field> — one field of a script's --describe
# output. A script that declares no group isn't part of the suite proper.
describe_field() {
	sed -n "s/^$2:[[:space:]]*//p" <<<"$1"
}

# in_group — comma-separated GROUP matches any of the listed groups.
in_group() {
	local want="$1"
	[ -z "$GROUP" ] && return 0
	case ",$GROUP," in
	*",$want,"*) return 0 ;;
	esac
	return 1
}

# NEEDS_LOCK: some selected script drives the shared Incus server and would
# run, not skip, under VALIDATE_OS (validate_os_supports, the gate's own rule).
SCRIPTS=()
NEEDS_LOCK=0
for f in "$SUITE_DIR"/*.sh; do
	base="$(basename "$f")"
	case "$base" in
	lib*.sh | run.sh) continue ;;
	esac
	[ -x "$f" ] || continue
	desc="$("$f" --describe 2>/dev/null)"
	g="$(describe_field "$desc" group)"
	[ -n "$g" ] || continue
	in_group "$g" || continue
	SCRIPTS+=("$f")
	case "$g" in
	incus | incus-vm)
		validate_os_supports "$(describe_field "$desc" os)" && NEEDS_LOCK=1
		;;
	esac
done

if [ "${#SCRIPTS[@]}" -eq 0 ]; then
	# shellcheck disable=SC2016 # inside "${:+}" the single quotes are literal; $GROUP expands
	echo "no scripts matched${GROUP:+ group '$GROUP'}" >&2
	exit 2
fi

if [ "$DESCRIBE" = 1 ]; then
	for f in "${SCRIPTS[@]}"; do
		"$f" --describe
		echo
	done
	exit 0
fi

if [ "$LIST" = 1 ]; then
	if [ "$JSON" = 1 ]; then
		printf '['
		sep=""
		for f in "${SCRIPTS[@]}"; do
			printf '%s"%s"' "$sep" "$(basename "$f")"
			sep=","
		done
		printf ']\n'
	else
		for f in "${SCRIPTS[@]}"; do basename "$f"; done
	fi
	exit 0
fi

# --- run ---------------------------------------------------------------------
# Output is captured per script and printed whole, so parallel runs don't
# interleave into nonsense. Sequential runs stream directly.

TMP="$(mktemp -d)"
# hwlock_release is a no-op unless this run took the lock. The signal traps
# make INT/TERM/HUP exit through the EXIT trap, so they release it too, and
# never under a running script. Run in sequence, a script is in the
# foreground, and bash runs a trap only once it has ended. Run in parallel
# (--jobs > 1), `wait` returns as soon as the signal lands, and the scripts are
# background jobs, which bash starts ignoring SIGINT, so a Ctrl-C never reaches
# them: on_signal sends them TERM and waits for them before exiting. It flags
# the stop first, and run_one checks the flag before starting a script and
# again once its pid file is written, so a script starting just as the signal
# lands is stopped too, rather than run to its end under on_signal's `wait`.
# shellcheck disable=SC2329 # invoked by the traps below
on_signal() {
	local p
	touch "$TMP/stopping"
	for p in "$TMP"/*.pid; do
		[ -e "$p" ] && kill -TERM "$(cat "$p")" 2>/dev/null
	done
	wait
	exit "$1"
}
trap 'hwlock_release; rm -rf "$TMP"' EXIT
trap 'on_signal 130' INT
trap 'on_signal 143' TERM
trap 'on_signal 129' HUP

# --- the hardware lock (#665) ------------------------------------------------
if [ "$NEEDS_LOCK" = 1 ]; then
	hwlock_check_config || exit 2
	if [ -n "${VALIDATE_HARDWARE_LOCK_HELD:-}" ]; then
		hwlock_check_held "$VALIDATE_HARDWARE_LOCK_HELD"
		case $? in
		0) echo "hardware lock: $VALIDATE_HARDWARE_LOCK_HELD, held by this run's caller" >&2 ;;
		3) echo "NOTE: hardware lock: $HWLOCK_ERROR; running without checking VALIDATE_HARDWARE_LOCK_HELD" >&2 ;;
		*)
			echo "ERROR: hardware lock: VALIDATE_HARDWARE_LOCK_HELD is set, but $HWLOCK_ERROR — starting no script" >&2
			exit 2
			;;
		esac
	else
		hwlock_acquire
		case $? in
		0) export VALIDATE_HARDWARE_LOCK_HELD="$HWLOCK_NAME" ;;
		1)
			echo "ERROR: hardware lock: held: $HWLOCK_HOLDER — starting no script (incus profile show $HWLOCK_REMOTE:${HWLOCK_HOLDER%% *} --project $HWLOCK_PROJECT)" >&2
			exit 2
			;;
		3) echo "NOTE: hardware lock: $HWLOCK_ERROR; running without the lock" >&2 ;;
		*)
			echo "ERROR: hardware lock: $HWLOCK_ERROR — starting no script" >&2
			exit 2
			;;
		esac
	fi
fi

run_one() {
	local f="$1" base
	base="$(basename "$f")"
	if [ "$JOBS" -gt 1 ]; then
		# The script's own pid, for on_signal.
		[ ! -e "$TMP/stopping" ] || return 0
		"$f" "${PASSTHRU[@]+"${PASSTHRU[@]}"}" >"$TMP/$base.out" 2>&1 &
		echo "$!" >"$TMP/$base.pid"
		[ ! -e "$TMP/stopping" ] || kill -TERM "$!" 2>/dev/null
		wait "$!"
		echo "$?" >"$TMP/$base.rc"
		rm -f "$TMP/$base.pid"
	else
		"$f" "${PASSTHRU[@]+"${PASSTHRU[@]}"}" 2>&1 | tee "$TMP/$base.out"
		echo "${PIPESTATUS[0]}" >"$TMP/$base.rc"
	fi
}

if [ "$JOBS" -gt 1 ]; then
	running=0
	for f in "${SCRIPTS[@]}"; do
		run_one "$f" &
		running=$((running + 1))
		if [ "$running" -ge "$JOBS" ]; then
			wait -n 2>/dev/null || wait
			running=$((running - 1))
		fi
	done
	wait
	for f in "${SCRIPTS[@]}"; do
		base="$(basename "$f")"
		echo "======== $base ========"
		cat "$TMP/$base.out"
		echo
	done
else
	for f in "${SCRIPTS[@]}"; do
		echo "======== $(basename "$f") ========"
		run_one "$f"
		echo
	done
fi

# --- aggregate ---------------------------------------------------------------
worst=0
any_fail=0
any_prereq=0
any_skip=0

echo "======== summary ========"
for f in "${SCRIPTS[@]}"; do
	base="$(basename "$f")"
	rc="$(cat "$TMP/$base.rc" 2>/dev/null || echo 1)"
	line="$(grep -E '^[0-9]+ passed, ' "$TMP/$base.out" | tail -1)"
	case "$rc" in
	0) verdict="ok" ;;
	1) verdict="FAILED" && any_fail=1 ;;
	2) verdict="prereqs unmet" && any_prereq=1 ;;
	3) verdict="skipped some" && any_skip=1 ;;
	*) verdict="exit $rc" && any_fail=1 ;;
	esac
	printf '%-52s %-14s %s\n' "$base" "$verdict" "${line:-(no summary)}"
done

[ "$any_fail" = 1 ] && worst=1
[ "$worst" = 0 ] && [ "$any_prereq" = 1 ] && worst=2
[ "$worst" = 0 ] && [ "$any_skip" = 1 ] && worst=3
exit "$worst"
