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
#
# Groups come from each script's own VALIDATE_GROUP declaration, read via
# --describe, rather than a list maintained here. A suite whose coverage depends
# on remembering to update a central list is exactly the rot this exists to stop
# — the same reasoning as lint-mermaid.sh globbing docs/*.md.
#
# Exit codes aggregate the children's (see lib.sh): any 1 → 1; else any 2 → 2;
# else any 3 → 3; else 0. So "something is broken" always outranks "something
# couldn't run", which always outranks "everything that ran, passed".

set -uo pipefail

SUITE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
		sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 2
		;;
	esac
	shift
done

# script_group <path> — the script's declared group, or "" if it doesn't declare
# one (which means it isn't part of the suite proper: the lib*.sh files).
script_group() {
	"$1" --describe 2>/dev/null | awk '/^group:/ {print $2}'
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

SCRIPTS=()
for f in "$SUITE_DIR"/*.sh; do
	base="$(basename "$f")"
	case "$base" in
	lib*.sh | run.sh) continue ;;
	esac
	[ -x "$f" ] || continue
	g="$(script_group "$f")"
	[ -n "$g" ] || continue
	in_group "$g" || continue
	SCRIPTS+=("$f")
done

if [ "${#SCRIPTS[@]}" -eq 0 ]; then
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
trap 'rm -rf "$TMP"' EXIT

run_one() {
	local f="$1" base
	base="$(basename "$f")"
	if [ "$JOBS" -gt 1 ]; then
		"$f" "${PASSTHRU[@]+"${PASSTHRU[@]}"}" >"$TMP/$base.out" 2>&1
		echo "$?" >"$TMP/$base.rc"
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
