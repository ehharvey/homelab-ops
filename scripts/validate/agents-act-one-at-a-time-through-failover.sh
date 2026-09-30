#! /bin/bash
# Validates GH issue #101: real cmd/agent processes, electing through real
# Incus (docs/Decisions.md §25 and its #212 addendum), keep leadership to one
# agent at a time.
#
# Unit tests cover leaderelection.Designated's rule over fake peers. This runs
# three real agent binaries against the real Incus they publish to and a real
# git remote they fetch from, and asserts on what Incus itself shows:
#
#   1. exactly one agent acts — the designated primary's;
#   2. a commit changing `primary` hands over with no overlap: never two
#      agents with acting=true at once, and the old one's log ends acting
#      before the new one's begins;
#   3. an agent on a stale checkout stands down: it still believes it's
#      primary and nobody else is acting, but a running peer has published a
#      commit it doesn't have. A control then removes that peer evidence and
#      shows the same agent *does* act — so the fence is what held it back.
#
# Each agent claims the identity of a throwaway Alpine container created here
# (validate-agent-<pid>-node<N>-g0), because the registry's keys live on the
# agent's own Incus instance and peers must be positively Running. The agents
# themselves run locally, over a unix socket to that Incus.
#
# Getting the socket, as for incuslocal-round-trips-instances-over-unix-socket.sh:
# on the Incus host use /var/lib/incus/unix.socket; from the devcontainer run
# the incus-bridge compose service (see docker-compose.yml) and point
# VALIDATE_INCUS_SOCKET at it. Unset or unusable, every check skips
# [incus-socket]; no pinned container image, [ct-image]; a daemon that can't
# create instances, [incus-instances].
#
# Everything this creates on the host is prefixed validate-agent- and deleted
# on exit. Fixtures go in the default project, the only one internal/incuslocal
# looks in. Each run tags its containers with an App of its own
# (validate-agent-<pid>), and agents elect only among their own App's
# instances, so these never fence or block real agents on the same Incus —
# not even if a SIGKILL skips the cleanup and leaves them running.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="exactly one cmd/agent acts; a commit changing primary hands over with no overlap; a stale-checkout agent stands down (#101, #212)"
VALIDATE_GROUP="incus"
VALIDATE_NEEDS="read/write access to the Incus unix socket, an Incus that can create instances, go, git, curl, jq, [a container image]"
VALIDATE_DURATION="~45s"

validate_parse_args "$@"

SOCKET="${VALIDATE_INCUS_SOCKET:-/var/lib/incus/unix.socket}"
CT_IMAGE="${VALIDATE_ALPINE_CT:-validate-alpine}"
TICK="${VALIDATE_AGENT_TICK:-1s}"

WORK_DIR="$(mktemp -d)"
AGENT_BIN="$WORK_DIR/agent"
REMOTE="$WORK_DIR/fleet.git"      # the config repo every agent follows
STALE="$WORK_DIR/fleet-stale.git" # a mirror frozen at the first commit
SAMPLES="$WORK_DIR/samples"       # one line per Incus snapshot: <epoch> <acting names...>

PREFIX="validate-agent-$$"
APP_TAG="$PREFIX"
NODES=(node0 node1 node2)
declare -A INSTANCE PID
for n in "${NODES[@]}"; do
	INSTANCE[$n]="$PREFIX-$n-g0"
done
SAMPLER_PID=""

_checks=(
	"agent binary builds"
	"every agent publishes the commit it synced"
	"the designated primary's agent acts"
	"exactly one agent acts while the designation is steady"
	"every agent writes a heartbeat"
	"a commit changing primary hands acting to the new primary"
	"the old primary stops acting after the change"
	"never two agents acting at once, per Incus, through the handoff"
	"never two agents acting at once, per the agents' own logs"
	"the old primary's log ends acting before the new primary's begins"
	"a stale-checkout agent publishes its stale commit"
	"a stale-checkout agent that believes it's primary stands down"
	"it says why: it is behind a peer"
	"control: with no running peer ahead of it, the same stale agent acts"
)

api() {
	local method="$1" path="$2"
	shift 2
	curl -s --unix-socket "$SOCKET" -X "$method" "$@" "http://unix$path"
}

# await <response-json> — succeeds on a sync response, or on an async one
# whose operation finishes successfully.
await() {
	local op
	op="$(jq -r 'select(.type=="async") | .operation // empty' <<<"$1" 2>/dev/null)"
	[ -z "$op" ] && jq -e '.type == "sync"' <<<"$1" >/dev/null 2>&1 && return 0
	[ -z "$op" ] && return 1
	curl -s --unix-socket "$SOCKET" "http://unix${op}/wait" |
		jq -e '.metadata.status_code == 200' >/dev/null 2>&1
}

# launch <name> — a started container from the pinned image, carrying the App
# tag incuslocal.ListInstances filters on. Prints the daemon's error, if any.
launch() {
	local body resp op
	body="$(jq -nc --arg n "$1" --arg img "$CT_IMAGE" --arg t "$APP_TAG" \
		'{name: $n, source: {type: "image", alias: $img}, config: {"user.homelab-ops.app": $t}, start: true}')"
	resp="$(api POST /1.0/instances -H 'Content-Type: application/json' -d "$body")"
	if [ "$(jq -r '.type // empty' <<<"$resp")" = "error" ]; then
		jq -r '.error' <<<"$resp"
		return 1
	fi
	op="$(jq -r '.operation // empty' <<<"$resp")"
	curl -s --unix-socket "$SOCKET" "http://unix${op}/wait" |
		jq -r 'select(.metadata.status_code != 200) | .metadata.err // "unknown failure"'
}

set_state() { # set_state <name> <start|stop>
	await "$(api PUT "/1.0/instances/$1/state" -H 'Content-Type: application/json' \
		-d "{\"action\": \"$2\", \"force\": true, \"timeout\": 30}")"
}

destroy() {
	set_state "$1" stop >/dev/null 2>&1
	await "$(api DELETE "/1.0/instances/$1")" >/dev/null 2>&1
}

# agents_json — this run's instances as [{name, status, config}], one read.
agents_json() {
	api GET "/1.0/instances?recursion=1" |
		jq -c --arg t "$APP_TAG" '[.metadata[] | select(.config["user.homelab-ops.app"] == $t)
			| {name, status, config}]'
}

key() { # key <node> <agent key suffix>
	agents_json | jq -r --arg n "${INSTANCE[$1]}" --arg k "user.homelab-ops.agent.$2" \
		'.[] | select(.name == $n) | .config[$k] // ""'
}

# acting_names — the names Incus shows with acting=true right now.
acting_names() {
	agents_json | jq -r '.[] | select(.config["user.homelab-ops.agent.acting"] == "true") | .name' | sort | tr '\n' ' '
}

# acting_is <node> — Incus shows exactly that node's agent acting, and no other.
acting_is() { [ "$(acting_names)" = "${INSTANCE[$1]} " ]; }

key_is() { [ "$(key "$1" "$2")" = "$3" ]; } # key_is <node> <key> <want>

start_agent() { # start_agent <node> <repo-url> <log-suffix>
	local n="$1" log="$WORK_DIR/agent-$1$3.log"
	AGENT_NODE_NAME="$n" \
		AGENT_INSTANCE_NAME="${INSTANCE[$n]}" \
		CONFIG_REPO_URL="$2" \
		AGENT_REPO_DIR="$WORK_DIR/clone-$n$3" \
		AGENT_INCUS_SOCKET="$SOCKET" \
		AGENT_TICK_INTERVAL="$TICK" \
		AGENT_SYNC_FAILURE_THRESHOLD=5 \
		"$AGENT_BIN" >"$log" 2>&1 &
	PID[$n]=$!
}

stop_agent() {
	[ -n "${PID[$1]:-}" ] || return 0
	kill -TERM "${PID[$1]}" 2>/dev/null
	wait "${PID[$1]}" 2>/dev/null
	PID[$1]=""
}

# wait_for <timeout-s> <cmd...> — poll until cmd succeeds.
wait_for() {
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -ge "$deadline" ] && return 1
		sleep 0.3
	done
}

# sample_forever — snapshot who Incus shows acting, as fast as it can read.
sample_forever() {
	while :; do
		printf '%s %s\n' "$(date +%s.%N)" "$(acting_names)" >>"$SAMPLES"
		sleep 0.2
	done
}

# max_acting_since <epoch> — the most agents any one snapshot showed acting.
max_acting_since() {
	awk -v since="$1" '$1 >= since { n = NF - 1; if (n > max) max = n } END { print max + 0 }' "$SAMPLES"
}

commit_designation() { # commit_designation <primary>
	(
		cd "$WORK_DIR/work" &&
			sed -i "s/^primary: .*/primary: $1/" fleet.yaml &&
			git commit -qam "primary: $1" &&
			git push -q origin HEAD:main &&
			git rev-parse HEAD
	)
}

cleanup() {
	[ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null
	for n in "${NODES[@]}"; do
		stop_agent "$n"
	done
	# The agents' own account is the first thing to read when a check failed.
	if [ "$VALIDATE_FAIL" -gt 0 ]; then
		for f in "$WORK_DIR"/agent-*.log; do
			[ -f "$f" ] || continue
			echo "--- $(basename "$f") (last 15 lines)"
			tail -n 15 "$f"
		done
	fi
	for n in "${NODES[@]}"; do
		destroy "${INSTANCE[$n]}"
	done
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

skip_all() { # skip_all <tag> <reason>
	for _d in "${_checks[@]}"; do
		skip_check "$_d" "$1" "$2"
	done
	summary
}

echo "== 0. Prerequisites =="
require_cmd go git curl jq
check_prereqs

if [ ! -S "$SOCKET" ] || [ ! -r "$SOCKET" ] || [ ! -w "$SOCKET" ]; then
	skip_all incus-socket "$SOCKET is not a readable/writable socket (run on the Incus host, or set VALIDATE_INCUS_SOCKET)"
fi
info="$(api GET /1.0)"
echo "   server $(jq -r '.metadata.environment.server_version // "?"' <<<"$info"), clustered=$(jq -r '.metadata.environment.server_clustered | if . == null then "?" else tostring end' <<<"$info"), socket $SOCKET"
if ! api GET "/1.0/images/aliases/$CT_IMAGE" | jq -e '.type == "sync"' >/dev/null 2>&1; then
	skip_all ct-image "container image alias '$CT_IMAGE' not on this Incus (run .devcontainer/scripts/3-pin-validate-images.sh, or set VALIDATE_ALPINE_CT)"
fi

echo
echo "== 1. Build the agent, a config repo, and three agent instances =="
if ! go -C "$ROOT_DIR" build -o "$AGENT_BIN" ./cmd/agent; then
	record_fail "agent binary builds"
	for _d in "${_checks[@]:1}"; do record_fail "$_d" "agent did not build"; done
	summary
fi
record_pass "agent binary builds"

git init -q --bare -b main "$REMOTE"
git clone -q "$REMOTE" "$WORK_DIR/work" 2>/dev/null
(
	cd "$WORK_DIR/work" &&
		git config user.email dev@homelab-ops.local &&
		git config user.name validate &&
		cat >fleet.yaml <<-'EOF' &&
			kind: Network
			name: lan
			cidr: 10.99.0.0/24
			gateway: 10.99.0.1
			dhcp_excluded_range: 10.99.0.200-10.99.0.250
			---
			kind: Instance
			name: node0
			mac: 02:00:00:00:99:00
			network: lan
			static_ip: 10.99.0.200
			disk: single
			nic: single
			applications: [incus]
			---
			kind: Instance
			name: node1
			mac: 02:00:00:00:99:01
			network: lan
			static_ip: 10.99.0.201
			disk: single
			nic: single
			applications: [incus]
			---
			kind: Instance
			name: node2
			mac: 02:00:00:00:99:02
			network: lan
			static_ip: 10.99.0.202
			disk: single
			nic: single
			applications: [incus]
			---
			kind: Designation
			primary: node0
		EOF
		git add fleet.yaml && git commit -qm "primary: node0" && git push -q origin HEAD:main
)
C1="$(git -C "$REMOTE" rev-parse main)"
git clone -q --bare "$REMOTE" "$STALE"
echo "   c1 = $C1 (primary: node0)"

for n in "${NODES[@]}"; do
	if ! err="$(launch "${INSTANCE[$n]}")" || [ -n "$err" ]; then
		skip_all incus-instances "the Incus at $SOCKET could not start ${INSTANCE[$n]} from '$CT_IMAGE': $err"
	fi
done
echo "   started ${INSTANCE[*]}"

echo
echo "== 2. Exactly one agent acts =="
: >"$SAMPLES"
sample_forever &
SAMPLER_PID=$!
T0="$(date +%s.%N)"
for n in "${NODES[@]}"; do
	start_agent "$n" "$REMOTE" ""
done

all_on() { # all_on <commit> <node...>
	local c="$1" n
	shift
	for n in "$@"; do [ "$(key "$n" commit)" = "$c" ] || return 1; done
}
if wait_for 30 all_on "$C1" "${NODES[@]}"; then
	record_pass "every agent publishes the commit it synced"
else
	record_fail "every agent publishes the commit it synced" \
		"$(for n in "${NODES[@]}"; do printf '%s=%s ' "$n" "$(key "$n" commit)"; done)"
fi

if wait_for 30 acting_is node0; then
	record_pass "the designated primary's agent acts"
else
	record_fail "the designated primary's agent acts" "acting: [$(acting_names)]"
fi

# Hold the designation steady for a few ticks and require every snapshot in
# that window to show exactly node0.
steady_start="$(date +%s.%N)"
sleep 5
steady="$(awk -v s="$steady_start" '$1 >= s { $1 = ""; print }' "$SAMPLES" | sort | uniq -c)"
if [ "$(wc -l <<<"$steady")" -eq 1 ] && grep -q " ${INSTANCE[node0]} *$" <<<"$steady"; then
	record_pass "exactly one agent acts while the designation is steady"
else
	record_fail "exactly one agent acts while the designation is steady" "snapshots: $(tr '\n' '|' <<<"$steady")"
fi

hb_ok=true
for n in "${NODES[@]}"; do
	[ -n "$(key "$n" heartbeat)" ] || hb_ok=false
done
if "$hb_ok"; then
	record_pass "every agent writes a heartbeat"
else
	record_fail "every agent writes a heartbeat"
fi

echo
echo "== 3. One commit changes primary to node1: handoff with no overlap =="
C2="$(commit_designation node1)"
echo "   c2 = $C2 (primary: node1)"
if wait_for 30 acting_is node1; then
	record_pass "a commit changing primary hands acting to the new primary"
else
	record_fail "a commit changing primary hands acting to the new primary" "acting: [$(acting_names)]"
fi
sleep 3
if [ "$(key node0 acting)" = "false" ] && [ "$(acting_names)" = "${INSTANCE[node1]} " ]; then
	record_pass "the old primary stops acting after the change"
else
	record_fail "the old primary stops acting after the change" "node0 acting=$(key node0 acting); acting: [$(acting_names)]"
fi

max="$(max_acting_since "$T0")"
check_eq "never two agents acting at once, per Incus, through the handoff" 1 "$max"

# The agents log "ACTING begin" once MayAct has verified a claim and "ACTING
# end" before clearing the flag: the window in which each may act. Merge every
# log's windows on one timeline (same machine, same clock) and require the
# running count never to exceed one. At equal timestamps an end sorts first.
acting_overlap() {
	local f
	for f in "$@"; do
		[ -f "$f" ] || continue
		awk '/ACTING begin/ { print $1 " " $2 " 1" } /ACTING end/ { print $1 " " $2 " 0" }' "$f"
	done | sort | awk '{ c += ($3 == 1 ? 1 : -1); if (c > max) max = c } END { print max + 0 }'
}
check_eq "never two agents acting at once, per the agents' own logs" 1 \
	"$(acting_overlap "$WORK_DIR"/agent-*.log)"

end0="$(grep -h "ACTING end" "$WORK_DIR"/agent-node0*.log | head -1 | awk '{print $1" "$2}')"
begin1="$(grep -h "ACTING begin" "$WORK_DIR"/agent-node1*.log | head -1 | awk '{print $1" "$2}')"
if [ -n "$end0" ] && [ -n "$begin1" ] && [[ "$end0" < "$begin1" ]]; then
	record_pass "the old primary's log ends acting before the new primary's begins"
	echo "   node0 ended $end0, node1 began $begin1"
else
	record_fail "the old primary's log ends acting before the new primary's begins" "node0 end='$end0' node1 begin='$begin1'"
fi

echo
echo "== 4. A stale checkout stands down =="
# node1's agent stops cleanly (its instance keeps Running, publishing c2 and
# acting=false). node0's agent restarts on a mirror frozen at c1, where it is
# still primary. Nobody else is acting, so the only thing that can stop it is
# node1's and node2's published c2, which it doesn't have.
stop_agent node1
stop_agent node0
start_agent node0 "$STALE" "-stale"
stale_start="$(date +%s.%N)"

if wait_for 30 key_is node0 commit "$C1"; then
	record_pass "a stale-checkout agent publishes its stale commit"
else
	record_fail "a stale-checkout agent publishes its stale commit" "node0 commit=$(key node0 commit), want $C1"
fi
sleep 6
node0_acted="$(awk -v s="$stale_start" -v n="${INSTANCE[node0]}" '$1 >= s && index($0, n) { print "yes"; exit }' "$SAMPLES")"
if [ -z "$node0_acted" ] && [ "$(key node0 acting)" = "false" ]; then
	record_pass "a stale-checkout agent that believes it's primary stands down"
else
	record_fail "a stale-checkout agent that believes it's primary stands down" "node0 acting=$(key node0 acting); seen acting=${node0_acted:-no}"
fi
status0="$(key node0 status)"
if grep -q "behind:" <<<"$status0"; then
	record_pass "it says why: it is behind a peer"
else
	record_fail "it says why: it is behind a peer" "status=$status0"
fi
echo "   node0 status: $status0"

# Control. Stop node1's and node2's instances: Stopped instances aren't peers,
# so their c2 no longer fences anyone. The same stale agent now acts — which is
# the documented limit (a lone stale agent can't know what it hasn't seen), and
# proof that the fence, not something else, held it back above.
stop_agent node2
set_state "${INSTANCE[node1]}" stop
set_state "${INSTANCE[node2]}" stop
if wait_for 20 acting_is node0; then
	record_pass "control: with no running peer ahead of it, the same stale agent acts"
else
	record_fail "control: with no running peer ahead of it, the same stale agent acts" \
		"acting: [$(acting_names)] status=$(key node0 status)"
fi

echo
summary
