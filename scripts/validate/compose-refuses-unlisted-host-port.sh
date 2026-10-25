#! /bin/bash
# Validates GH issue #271: the compose group's host ports have one list, and a
# stack that publishes a port missing from it doesn't start.
#
# Drives lib-compose.sh's real compose() against the real docker-compose.yml,
# with scratch overrides:
#   1. The committed file, and an override adding a listed port beyond the
#      base file's, pass the guard; a port with no fixed host side is ignored.
#   2. An override publishing 9999/tcp, and one publishing 51820 as tcp (the
#      list names it as udp), make `check ... compose up` fail with the port
#      named on that check's FAIL line, and no container is created. So does
#      9999 arriving through the call's own -f or `run -p`.
#   3. What the guard can't check fails closed: a config that won't render,
#      and a global option it doesn't know. Subcommands that create nothing
#      aren't held to it.
#   4. _compose_ports_free reads the same list, tcp and udp: a bound listed
#      port is busy, and free once released.
#   5. A compose script on a host without jq, which the guard needs, exits 2
#      before doing any work.
#
# Nothing here starts a container: every `up` it runs is one the guard refuses,
# under a throwaway project name, so it publishes no host port and never
# touches a stack another script has up. (Each api-serves-style script's own
# `compose up` is what holds that script's override to the list.)

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="compose refuses to start a stack publishing a host port missing from COMPOSE_HOST_PORTS, the one list compose_down waits on (#271)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq python3 ss"
VALIDATE_DURATION="~10s"

validate_parse_args "$@"
cd "$ROOT_DIR" || exit 2

require_compose
require_cmd python3 ss
check_prereqs

WORK_DIR="$(mktemp -d)"
export COMPOSE_PROJECT_NAME="validate-port-guard-$$"
holder=
cleanup() {
	[ -n "$holder" ] && kill "$holder" 2>/dev/null
	# Only has anything to remove if the guard let an `up` through.
	_compose down --remove-orphans >/dev/null 2>&1
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# override <name> — writes stdin to a scratch override; prints its path.
override() {
	local f="$WORK_DIR/$1.yml"
	cat >"$f"
	echo "$f"
}

# listed_with <override> — the guard alone, on `up`. Locals keep the override
# and any failure it records from leaking into this script.
listed_with() {
	local VALIDATE_COMPOSE_OVERRIDE="$1" VALIDATE_FAIL="$VALIDATE_FAIL"
	_compose_parse up
	_compose_ports_listed up
}

# guarded <override> <compose args...> — `check "docker compose succeeds"
# compose <args>`, the way a compose script runs it, in a subshell so its
# failures don't count here. Prints what that printed, then "fails=<failures
# it recorded>". An empty override runs with none.
guarded() (
	VALIDATE_COMPOSE_OVERRIDE="$1"
	shift
	VALIDATE_FAIL=0
	check "docker compose succeeds" compose "$@"
	echo "fails=$VALIDATE_FAIL"
)

# port_busy <port/proto> — _compose_ports_free's verdict with only that port
# listed.
port_busy() {
	local COMPOSE_HOST_PORTS=("$1")
	! _compose_ports_free
}

# not <cmd...> — succeeds when cmd fails; `check` takes a command, not `!`.
not() { ! "$@"; }

# refused <desc> <want-reason> <out> — guarded's output is one FAIL line for the
# compose call carrying want-reason, and one recorded failure.
refused() {
	check "$1: the compose check's FAIL line says why" \
		grep -qF "FAIL: docker compose succeeds — compose publishes only listed host ports: $2" <<<"$3"
	check_eq "$1: one failure, counted once" "fails=1" "$(tail -n 1 <<<"$3")"
}

echo "== 1. Listed ports pass =="
_compose_parse up
check "the committed docker-compose.yml publishes only listed ports" _compose_ports_listed up
extra_listed=$(override extra-listed <<'EOF'
services:
  web:
    ports:
      - "127.0.0.1:8081:8081"
EOF
)
check "an override adding listed 8081 passes" listed_with "$extra_listed"
ephemeral=$(override ephemeral <<'EOF'
services:
  registry:
    ports:
      - "5001"
EOF
)
check "a port with no fixed host side is ignored" listed_with "$ephemeral"

echo
echo "== 2. An unlisted port fails a check and starts nothing =="
unlisted=$(override unlisted <<'EOF'
services:
  registry:
    ports:
      - "9999:9999"
EOF
)
check "the guard refuses an override publishing 9999" not listed_with "$unlisted"
refused "up with 9999 in the override" "9999/tcp is not in COMPOSE_HOST_PORTS" \
	"$(guarded "$unlisted" up -d registry)"
ids=$(_compose ps -aq) || ids="(compose ps failed)"
check_eq "no container was created" "" "$ids"

wrong_proto=$(override wrong-proto <<'EOF'
services:
  registry:
    ports:
      - "51820:51820"
EOF
)
refused "51820 published as tcp (the list names 51820/udp)" "51820/tcp is not in COMPOSE_HOST_PORTS" \
	"$(guarded "$wrong_proto" up -d registry)"

# The guard renders the config the call starts, its own global options in.
refused "up -f <9999 file>" "9999/tcp is not in COMPOSE_HOST_PORTS" \
	"$(guarded "" -f "$ROOT_DIR/docker-compose.yml" -f "$unlisted" up -d registry)"
refused "up --file=<9999 file>" "9999/tcp is not in COMPOSE_HOST_PORTS" \
	"$(guarded "" --file="$unlisted" up -d registry)"
refused "run -p 9999:9999" "9999/tcp is not in COMPOSE_HOST_PORTS" \
	"$(guarded "" run --rm -p 127.0.0.1:9999:5000 registry true)"
refused "run --publish=9998:5000/udp" "9998/udp is not in COMPOSE_HOST_PORTS" \
	"$(guarded "" run --rm --publish=9998:5000/udp registry true)"
check_eq "a bracketed IPv6 host address is stripped" "9999/tcp" "$(_compose_publish_port '[::1]:9999:5000')"
check_eq "-p with no fixed host side publishes nothing fixed" "" \
	"$(_compose_publish_port 5000; _compose_publish_port 127.0.0.1::5000)"

echo
echo "== 3. What it can't check fails closed =="
broken=$(override broken <<'EOF'
services: [
EOF
)
refused "a config that won't render" "could not render the compose config" \
	"$(guarded "$broken" up -d registry)"
refused "an unknown global option" "can't tell the subcommand (unknown global option --some-new-flag" \
	"$(guarded "" --some-new-flag value up -d registry)"
check_eq "a subcommand that creates nothing isn't held to it" "fails=0" \
	"$(guarded "$unlisted" ps -q | tail -n 1)"

echo
echo "== 4. _compose_ports_free waits on the same list =="
# Bind a free port of each protocol, list only it, and ask.
for proto in tcp udp; do
	port_file="$WORK_DIR/$proto.port"
	python3 -c '
import socket, sys, time
kind = socket.SOCK_STREAM if sys.argv[1] == "tcp" else socket.SOCK_DGRAM
s = socket.socket(socket.AF_INET, kind)
s.bind(("127.0.0.1", 0))
if kind == socket.SOCK_STREAM:
    s.listen()
with open(sys.argv[2], "w") as f:
    f.write(str(s.getsockname()[1]))
time.sleep(30)
' "$proto" "$port_file" &
	holder=$!
	for _ in $(seq 1 50); do
		[ -s "$port_file" ] && break
		sleep 0.1
	done
	port=$(cat "$port_file" 2>/dev/null)
	check "a bound listed $proto port is reported busy" port_busy "$port/$proto"
	kill "$holder" 2>/dev/null
	wait "$holder" 2>/dev/null
	holder=
	check "and free once released ($proto)" not port_busy "$port/$proto"
done

echo
echo "== 5. No jq is an unmet prerequisite, not a failure =="
# A PATH with only what a compose script runs before check_prereqs.
nojq="$WORK_DIR/nojq-bin"
mkdir -p "$nojq"
ln -s "$(command -v dirname)" "$(command -v docker)" "$nojq/"
PATH="$nojq" /bin/bash "$ROOT_DIR/scripts/validate/background-poll-warns-on-config-diff.sh" >/dev/null 2>&1
check_eq "a compose script without jq exits 2 before doing any work" "2" "$?"

summary
