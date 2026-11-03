#! /bin/bash
# Validates GH issue #486: the validate scripts take the host-OS backend as a
# parameter, VALIDATE_OS (docs/Decisions.md §31), with lib.sh as the gate.
#
# incusos is the only backend the harness knows, so a known-but-unsupported
# backend can't be shown with a real script. This drives lib.sh through fixture
# scripts in a scratch dir instead, each adding a fake backend, fakeos, to
# VALIDATE_KNOWN_OS after sourcing it:
#   1. VALIDATE_OS unset, empty or incusos: the script runs, exit 0.
#   2. An unknown VALIDATE_OS: exit 2 before any work, --describe included,
#      the message naming the known backends.
#   3. A known backend the script doesn't declare: one SKIP tagged host-os,
#      exit 3 before any work; --strict makes it exit 1, and
#      --allow-skip host-os makes it a skip again.
#   4. A script declaring "any", or listing the backend, runs under it.
#   5. --describe reports the declaration.
# Then, against the real suite:
#   6. Every script declares VALIDATE_SUPPORTED_OS, as "any" or known backends.
#   7. run.sh refuses an unknown VALIDATE_OS once, before running any script.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR" || exit 2
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="an unknown VALIDATE_OS is a usage error (exit 2) and a known backend a script doesn't declare skips it (exit 3) (#486)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="bash"
VALIDATE_DURATION="~5s"
VALIDATE_SUPPORTED_OS="any"

validate_parse_args "$@"
check_prereqs

SUITE_DIR="$ROOT_DIR/scripts/validate"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

# fixture <name> <supported> — a script that sources the real lib.sh, knows a
# second backend, declares <supported>, and prints "ran" once past the gate.
fixture() {
	cat >"$WORK_DIR/$1.sh" <<EOF
#! /bin/bash
set -uo pipefail
. "$SUITE_DIR/lib.sh"
VALIDATE_KNOWN_OS+=(fakeos)
VALIDATE_PROVES="fixture"
VALIDATE_GROUP="none"
VALIDATE_SUPPORTED_OS="$2"
validate_parse_args "\$@"
echo "ran"
record_pass "fixture ran"
summary
EOF
	chmod +x "$WORK_DIR/$1.sh"
}
fixture incusos-only incusos
fixture agnostic any
fixture both "incusos fakeos"

# run_fixture <name> <VALIDATE_OS or "-" for unset> [args...] — sets OUT, ERR, RC.
run_fixture() {
	local name="$1" os="$2"
	shift 2
	if [ "$os" = - ]; then
		env -u VALIDATE_OS "$WORK_DIR/$name.sh" "$@" >"$WORK_DIR/out" 2>"$WORK_DIR/err"
	else
		VALIDATE_OS="$os" "$WORK_DIR/$name.sh" "$@" >"$WORK_DIR/out" 2>"$WORK_DIR/err"
	fi
	RC=$?
	OUT=$(cat "$WORK_DIR/out")
	ERR=$(cat "$WORK_DIR/err")
}

ran() { grep -qx ran <<<"$OUT"; }

# 1. The default.
run_fixture incusos-only -
check_eq "VALIDATE_OS unset: an incusos script exits 0" 0 "$RC"
check "VALIDATE_OS unset: an incusos script runs" ran
run_fixture incusos-only ""
check_eq "VALIDATE_OS empty: an incusos script exits 0" 0 "$RC"
run_fixture incusos-only incusos
check_eq "VALIDATE_OS=incusos: an incusos script exits 0" 0 "$RC"
check "VALIDATE_OS=incusos: an incusos script runs" ran

# 2. An unknown backend.
run_fixture agnostic nosuchos
check_eq "VALIDATE_OS=nosuchos exits 2, even for an agnostic script" 2 "$RC"
check "VALIDATE_OS=nosuchos does no work" test "$(ran && echo ran)" = ""
check "VALIDATE_OS=nosuchos names the known backends" \
	grep -qx 'unknown VALIDATE_OS: nosuchos (known backends: incusos fakeos)' <<<"$ERR"
run_fixture incusos-only nosuchos --describe
check_eq "VALIDATE_OS=nosuchos exits 2 under --describe too" 2 "$RC"

# 3. A known backend the script doesn't support.
run_fixture incusos-only fakeos
check_eq "VALIDATE_OS=fakeos: an incusos script exits 3" 3 "$RC"
check "VALIDATE_OS=fakeos: an incusos script does no work" test "$(ran && echo ran)" = ""
check "VALIDATE_OS=fakeos: one SKIP tagged host-os" \
	grep -qx 'SKIP: incusos-only.sh on VALIDATE_OS=fakeos \[host-os\] — the script supports only: incusos' <<<"$OUT"
run_fixture incusos-only fakeos --strict
check_eq "VALIDATE_OS=fakeos --strict: exits 1" 1 "$RC"
run_fixture incusos-only fakeos --strict --allow-skip host-os
check_eq "VALIDATE_OS=fakeos --strict --allow-skip host-os: exits 3" 3 "$RC"

# 4. Agnostic and multi-backend declarations.
run_fixture agnostic fakeos
check_eq "VALIDATE_OS=fakeos: an agnostic script exits 0" 0 "$RC"
check "VALIDATE_OS=fakeos: an agnostic script runs" ran
run_fixture both fakeos
check_eq "VALIDATE_OS=fakeos: a script listing fakeos exits 0" 0 "$RC"

# 5. --describe.
run_fixture both - --describe
check "--describe reports the declared backends" grep -qx 'os:       incusos fakeos' <<<"$OUT"
run_fixture agnostic fakeos --describe
check "--describe reports an agnostic script as any" grep -qx 'os:       any' <<<"$OUT"

# 6. The real suite declares, and only known backends.
undeclared="" unknown=""
for f in "$SUITE_DIR"/*.sh; do
	case "$(basename "$f")" in lib*.sh | run.sh) continue ;; esac
	grep -q '^VALIDATE_SUPPORTED_OS=' "$f" || undeclared="$undeclared $(basename "$f")"
	declared=$(env -u VALIDATE_OS "$f" --describe 2>/dev/null | sed -n 's/^os: *//p')
	for os in $declared; do
		case " any ${VALIDATE_KNOWN_OS[*]} " in
		*" $os "*) ;;
		*) unknown="$unknown $(basename "$f"):$os" ;;
		esac
	done
done
check_eq "every suite script declares VALIDATE_SUPPORTED_OS" "" "${undeclared# }"
check_eq "every declared backend is any or known" "" "${unknown# }"

# 7. run.sh fails fast.
VALIDATE_OS=nosuchos "$SUITE_DIR/run.sh" --group none >"$WORK_DIR/out" 2>"$WORK_DIR/err"
check_eq "run.sh with VALIDATE_OS=nosuchos exits 2" 2 "$?"
check "run.sh with VALIDATE_OS=nosuchos runs no script" test ! -s "$WORK_DIR/out"
check "run.sh names the known backends, once" \
	test "$(grep -c '^unknown VALIDATE_OS: nosuchos (known backends: incusos)$' "$WORK_DIR/err")" = 1

summary
