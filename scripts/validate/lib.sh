#! /bin/bash
# Shared harness for the validation suite (#140). Source it; don't run it.
#
# Sourcing scripts keep `set -uo pipefail` and NEVER `set -e`: checks accumulate
# so that one failure doesn't hide the rest of the run. lib.sh therefore never
# sets -e on your behalf, and every helper here is written to be safe under -u.
#
# The contract this exists to fix (#115): an unmet prerequisite used to be
# printed as `FAIL: ... (skipped: X not installed)` and counted as a failure, so
# "you didn't install a tool" was indistinguishable from "the thing under test
# is broken" — in both the output and the exit code. #136 is what that costs:
# a missing `go install` presented as five failures reading like a provisioning
# regression. Skips are now a first-class outcome with their own exit code.
#
# Exit codes (extending the gate app-produces-working-installer-e2e.sh already
# established):
#
#   0  every check ran and passed
#   1  at least one check FAILED — a real defect
#   2  hard prerequisites unmet; declined to run at all, before doing any work
#   3  no failures, but at least one check was SKIPPED
#
# Callers that don't care about skips treat 0 and 3 alike. CI uses --strict to
# promote 3 to 1, with --allow-skip for the skips a given environment genuinely
# cannot satisfy (a hosted runner has no 3.2 GB IncusOS image, for instance).
#
# Naming note: every script in this suite uses `pass`/`fail` as *variables*, and
# multi-commit-pr-cannot-reach-main.sh defines `pass` as a *function*. Hence
# record_pass/record_fail and the VALIDATE_-prefixed counters — a helper named
# `pass` would collide on sourcing.

VALIDATE_PASS=0
VALIDATE_FAIL=0
VALIDATE_SKIP=0
VALIDATE_STRICT=0
VALIDATE_ALLOWED_SKIPS=""
VALIDATE_PREREQ_MISSING=0

# ---------------------------------------------------------------------------
# Declarations. Each script sets these near its header comment; --describe
# prints them and exits without running anything, which is what lets run.sh
# and the README derive the prerequisite matrix instead of duplicating it.
# ---------------------------------------------------------------------------
VALIDATE_PROVES="${VALIDATE_PROVES:-}"
VALIDATE_GROUP="${VALIDATE_GROUP:-}"       # none | compose | incus | incus-vm | github
VALIDATE_NEEDS="${VALIDATE_NEEDS:-}"
VALIDATE_DURATION="${VALIDATE_DURATION:-}"

# ---------------------------------------------------------------------------
# Outcomes
# ---------------------------------------------------------------------------

record_pass() {
	echo "PASS: $1"
	VALIDATE_PASS=$((VALIDATE_PASS + 1))
}

record_fail() {
	# $2 is optional context (got: ..., want: ...), appended when present.
	echo "FAIL: $1${2:+ — $2}"
	VALIDATE_FAIL=$((VALIDATE_FAIL + 1))
}

_skip_allowed() {
	case " $VALIDATE_ALLOWED_SKIPS " in
	*" $1 "*) return 0 ;;
	*) return 1 ;;
	esac
}

# skip_check <desc> <tag> <reason> — an unmet *soft* prerequisite.
#
# Never a FAIL. Under --strict a skip whose tag isn't allowlisted becomes one,
# which is how CI says "these must actually run": a route that silently gains a
# gate produces a failure rather than a quietly-tolerated skip, and a *new* tag
# fails until someone blesses it explicitly.
skip_check() {
	local desc="$1" tag="$2" reason="$3"
	if [ "$VALIDATE_STRICT" = 1 ] && ! _skip_allowed "$tag"; then
		record_fail "$desc" "skipped [$tag] but --strict does not allow it: $reason"
		return
	fi
	echo "SKIP: $desc [$tag] — $reason"
	VALIDATE_SKIP=$((VALIDATE_SKIP + 1))
}

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

# check <desc> <cmd...> — runs cmd, passing if it exits 0.
#
# Output is discarded, which is deliberate for the common case but is also how
# #136 hid a perfectly clear CLI error ("flasher-tool not found: install via
# go install ..."). Prefer check_eq or an explicit stderr match when the *reason*
# for failure is what the assertion is really about.
check() {
	local desc="$1"
	shift
	if "$@" >/dev/null 2>&1; then
		record_pass "$desc"
	else
		record_fail "$desc"
	fi
}

check_json() {
	local desc="$1" json="$2" filter="$3"
	if echo "$json" | jq -e "$filter" >/dev/null 2>&1; then
		record_pass "$desc"
	else
		record_fail "$desc" "got: $json"
	fi
}

# check_eq <desc> <want> <got> — reports both sides on failure, unlike check,
# which can only tell you that something didn't work.
check_eq() {
	local desc="$1" want="$2" got="$3"
	if [ "$want" = "$got" ]; then
		record_pass "$desc"
	else
		record_fail "$desc" "want $want, got $got"
	fi
}

# ---------------------------------------------------------------------------
# Hard prerequisites.
#
# These accumulate rather than exiting on the first miss, so one run reports
# everything that's absent instead of making the operator rediscover them one
# at a time. check_prereqs() then exits 2 "before doing any work" — generalizing
# the gate app-produces-working-installer-e2e.sh already had.
# ---------------------------------------------------------------------------

_prereq_missing() {
	echo "ERROR: $1" >&2
	VALIDATE_PREREQ_MISSING=1
}

require_cmd() {
	local c
	for c in "$@"; do
		command -v "$c" >/dev/null 2>&1 ||
			_prereq_missing "required command not found: $c"
	done
}

require_docker_compose() {
	docker compose version >/dev/null 2>&1 ||
		_prereq_missing "'docker compose' not available"
}

require_incus_remote() {
	incus info "$1:" >/dev/null 2>&1 ||
		_prereq_missing "incus remote '$1' not reachable"
}

require_incus_project() {
	local remote="$1" project="$2"
	incus project list "$remote:" -f csv | cut -d, -f1 | sed 's/ (current)$//' |
		grep -qx "$project" ||
		_prereq_missing "incus project '$project' not found on $remote"
}

require_incus_network() {
	local remote="$1" project="$2" network="$3"
	incus network list "$remote:" --project "$project" -f csv | cut -d, -f1 |
		sed 's/ (current)$//' | grep -qx "$network" ||
		_prereq_missing "incus network '$network' not found in project '$project' on $remote"
}

# require_incus_image <remote> <project> <alias> — the pinned base image must
# exist locally on the host. A hard prereq rather than a skip: it's operator
# setup (run .devcontainer/scripts/3-pin-validate-images.sh), the same class as
# "the remote isn't there", not an environmental limit like a missing 3.2GB
# base image. A skip would need a new tag, and under --strict a new tag fails
# CI until someone blesses it — a diagnostic pointing at the fix is honester.
require_incus_image() {
	local remote="$1" project="$2" alias="$3"
	incus image list "$remote:" --project "$project" -c l -f csv |
		cut -d, -f1 | grep -qx "$alias" ||
		_prereq_missing "incus image alias '$alias' not found in project '$project' on $remote — run .devcontainer/scripts/3-pin-validate-images.sh"
}

# require_env_file <VARNAME> — the variable must be set AND name a real file.
require_env_file() {
	local name="$1" value="${!1:-}"
	if [ -z "$value" ]; then
		_prereq_missing "$name not set"
	elif [ ! -f "$value" ]; then
		_prereq_missing "$name=$value not found"
	fi
}

check_prereqs() {
	if [ "$VALIDATE_PREREQ_MISSING" -ne 0 ]; then
		echo "ERROR: prerequisites not met — aborting before doing any work" >&2
		exit 2
	fi
}

# ---------------------------------------------------------------------------
# Soft prerequisites — report availability; the caller decides what to skip.
# ---------------------------------------------------------------------------

# have_env_file <VARNAME> — 0 if set and the file exists, else 1. No output,
# no side effects; pair it with skip_check.
have_env_file() {
	local value="${!1:-}"
	[ -n "$value" ] && [ -f "$value" ]
}

have_cmd() {
	command -v "$1" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# Arg parsing and summary
# ---------------------------------------------------------------------------

validate_describe() {
	echo "name:     $(basename "${BASH_SOURCE[-1]}")"
	echo "proves:   $VALIDATE_PROVES"
	echo "group:    $VALIDATE_GROUP"
	echo "needs:    $VALIDATE_NEEDS"
	echo "duration: $VALIDATE_DURATION"
}

# validate_parse_args "$@" — call once, early, before doing any work.
# Recognizes --describe (print declarations and exit 0), --strict, and
# --allow-skip <tag>[,<tag>...] (repeatable).
validate_parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--describe)
			validate_describe
			exit 0
			;;
		--strict)
			VALIDATE_STRICT=1
			;;
		--allow-skip)
			VALIDATE_ALLOWED_SKIPS="$VALIDATE_ALLOWED_SKIPS ${2//,/ }"
			shift
			;;
		--allow-skip=*)
			VALIDATE_ALLOWED_SKIPS="$VALIDATE_ALLOWED_SKIPS ${1#*=}"
			VALIDATE_ALLOWED_SKIPS="${VALIDATE_ALLOWED_SKIPS//,/ }"
			;;
		-h | --help)
			validate_describe
			echo
			echo "usage: $(basename "${BASH_SOURCE[-1]}") [--describe] [--strict] [--allow-skip TAG[,TAG...]]"
			exit 0
			;;
		*)
			echo "unknown argument: $1" >&2
			exit 2
			;;
		esac
		shift
	done
}

# summary — print the tally and exit with the contract's code. Call last.
summary() {
	echo
	echo "$VALIDATE_PASS passed, $VALIDATE_FAIL failed, $VALIDATE_SKIP skipped"
	[ "$VALIDATE_FAIL" -eq 0 ] || exit 1
	[ "$VALIDATE_SKIP" -eq 0 ] || exit 3
	exit 0
}
