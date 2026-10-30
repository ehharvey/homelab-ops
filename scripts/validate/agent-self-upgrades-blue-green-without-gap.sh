#! /bin/bash
# Validates GH issue #109 (the agent's blue-green self-upgrade); the script
# itself was #103's, folded into #109 (operator, 2026-10-28).
#
# Unit tests (internal/agent/upgrade*_test.go) cover the upgrade pass over a
# fake Incus. This runs the real thing on a standalone host: real agent
# containers, built from this tree, upgrading themselves through their own
# incus-socket, after one commit in git changes the agent App's image.
#
#   1. Build the agent image and import it into the host's image store under
#      two aliases, <prefix>-a and <prefix>-b: two images as the agent App's
#      `image` sees them (agentdeploy.ImageTag differs), one build, so both
#      are this tree's agent. They go into the host's image store, as for the
#      VM scripts (lib.sh's build_agent_incus_image), rather than through the
#      compose registry (#102), which Incus would have to pull from over
#      plain HTTP.
#   2. Stand up a throwaway config repo: dev/git-fixture's image, run as an
#      application container on the host, serving git:// to the agents. It
#      declares one Instance (the host), a kind: Designation naming it
#      primary, and the agent App on image a. No second App (§30).
#   3. `bootstrap deploy-agent` (#100) creates <app>-<node>-g0; the script
#      sets its tick and self-upgrade knobs low and restarts it, and waits
#      for it to act. The agents it creates inherit those knobs from its own
#      environment (cmd/agent's inheritedEnv), so nothing touches them after.
#   4. A commit bumps the agent App to image b. From then on the script only
#      watches, sampling every agent instance about twice a second, and
#      asserts in order: -g1 appears while -g0 still runs; -g1 is marked
#      healthy; -g0 drains with reason `superseded`, no sooner than
#      AGENT_MIN_HEALTHY_DURATION into -g1's streak; -g1 acts; -g0 is gone,
#      retired by -g1 (its own log says so); exactly one agent instance is
#      left, on image b. At every sample, at most one instance acts and some
#      agent's heartbeat is fresh: the literal "no gap".
#
# Not covered: the upgrade across real cluster members (a group incus-vm
# script once Phase 5's clusters exist, #184), and the stale-checkout
# stand-down and failover (agents-act-one-at-a-time-through-failover.sh).
#
# What it needs from the host, and leaves there:
#   - A standalone Incus (unclustered, or a one-member cluster) as
#     VALIDATE_INCUS_REMOTE. The agents look only in the default project, so
#     everything goes there, whatever VALIDATE_INCUS_PROJECT says. Its
#     default profile must give a container a network the others can reach.
#   - The incus-socket profile (internal/seed). If the host lacks one, this
#     creates it and deletes it on exit; while it exists, anything that uses
#     it is root on the host's Incus. Only this run's agents do.
#   - Everything else is prefixed validate-upg-<pid> and deleted on exit. The
#     agents are an App of their own (validate-upg-<pid>), so they never
#     elect with, fence or touch real agents on the same Incus.
# A missing remote, a multi-member cluster, or a daemon that can't create
# instances SKIPs (exit 3).

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="bumping the agent App's image in git replaces a standalone host's agent blue-green with no operator step: the candidate drains the old one once sustained-healthy and retires it, at most one instance acts at any sample, and some agent's heartbeat stays fresh (#109, #103)"
VALIDATE_GROUP="incus"
VALIDATE_NEEDS="a standalone Incus remote (VALIDATE_INCUS_REMOTE; default project), go, docker, jq"
VALIDATE_DURATION="~2m"

validate_parse_args "$@"

REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
INCUS_CONF_DIR="${INCUS_CONF:-$HOME/.config/incus}"
TICK=2s
STALE_AFTER=10   # AGENT_HEARTBEAT_STALE_AFTER, seconds; also the "no gap" bound
MIN_HEALTHY=12   # AGENT_MIN_HEALTHY_DURATION, seconds
DEADLINE=2m      # AGENT_UPGRADE_HEALTH_DEADLINE

WORK_DIR="$(mktemp -d)"
SAMPLES="$WORK_DIR/samples" # one line per sample: <epoch> <json array of this run's agents>
PREFIX="validate-upg-$$"
APP="$PREFIX"
IMAGE_A="$PREFIX-a"
IMAGE_B="$PREFIX-b"
FIXTURE="$PREFIX-git"
FIXTURE_IMAGE="$PREFIX-git"
PROFILE_CREATED=""
SAMPLER_PID=""
NODE="" # the host's Instance name: its member name, on a cluster
G0="" G1=""

_checks=(
	"the agent, bootstrap and fixture images build and import"
	"bootstrap deploy-agent creates the first agent"
	"the first agent acts as the designated primary"
	"the primary creates its successor while it still runs"
	"the candidate is marked healthy"
	"the old agent drains with reason superseded"
	"it drains only after the candidate's minimum healthy duration"
	"the candidate acts only after the old agent drained"
	"the candidate retires the old agent"
	"exactly one agent instance is left, on the new image"
	"never two agents acting at once, through the upgrade"
	"some agent's heartbeat is fresh at every sample"
)

r() { printf '%s:%s' "$REMOTE" "$1"; } # r <name> — name on the remote

# agents_json — this run's agent instances as [{name, status, config}].
agents_json() {
	incus query "$REMOTE:/1.0/instances?recursion=1" 2>/dev/null |
		jq -c --arg a "$APP" '[.[] | select(.config["user.homelab-ops.app"] == $a) | {name, status, config}]' 2>/dev/null
}

cfg() { # cfg <instance> <key> — one config value of one of this run's agents
	agents_json | jq -r --arg n "$1" --arg k "$2" '.[] | select(.name == $n) | .config[$k] // ""'
}

# wait_for <timeout-s> <cmd...> — poll until cmd succeeds.
wait_for() {
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -ge "$deadline" ] && return 1
		sleep 1
	done
}

# sample_forever — one line per read of this run's agents, stamped when the
# read returned: a state a sample shows held by its stamp, so "seen at" is
# never earlier than it happened, and a heartbeat's age is never understated.
sample_forever() {
	local json
	while :; do
		json="$(agents_json)"
		printf '%s %s\n' "$(date +%s)" "$json" >>"$SAMPLES"
		sleep 0.5
	done
}

# fleet_yaml <image alias> — the config repo's whole content.
fleet_yaml() {
	cat <<-EOF
		kind: Network
		name: validate-lan
		cidr: 10.250.0.0/24
		gateway: 10.250.0.1
		---
		kind: Instance
		name: $NODE
		network: validate-lan
		---
		kind: Designation
		primary: $NODE
		---
		kind: App
		name: $APP
		type: agent
		replicas: per-node
		image:
		  alias: $1
	EOF
}

# commit_fleet <image alias> <message> — commit fleet.yaml on that image,
# inside the fixture, and push it to the repo the agents follow.
commit_fleet() {
	fleet_yaml "$1" >"$WORK_DIR/fleet.yaml"
	incus file push -q "$WORK_DIR/fleet.yaml" "$(r "$FIXTURE")/work/fleet.yaml" &&
		incus exec "$(r "$FIXTURE")" -- sh -c "cd /work && git add fleet.yaml && git commit -qm '$2' && git push -q origin HEAD:main"
}

console_tail() { # console_tail <instance> <lines>
	incus console "$(r "$1")" --show-log 2>/dev/null | tail -n "$2"
}

cleanup() {
	[ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null
	if [ "$VALIDATE_FAIL" -gt 0 ]; then
		for n in $(agents_json | jq -r '.[].name'); do
			echo "--- $n console (last 20 lines)"
			console_tail "$n" 20
		done
	fi
	for n in $(agents_json | jq -r '.[].name'); do
		incus delete -f "$(r "$n")" >/dev/null 2>&1
	done
	incus delete -f "$(r "$FIXTURE")" >/dev/null 2>&1
	incus image delete "$(r "$IMAGE_A")" >/dev/null 2>&1
	incus image delete "$(r "$FIXTURE_IMAGE")" >/dev/null 2>&1
	[ -n "$PROFILE_CREATED" ] && incus profile delete "$(r incus-socket)" >/dev/null 2>&1
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

skip_all() { # skip_all <tag> <reason>
	for _d in "${_checks[@]}"; do
		skip_check "$_d" "$1" "$2"
	done
	summary
}

fail_rest() { # fail_rest <from-index> <why>
	for _d in "${_checks[@]:$1}"; do
		record_fail "$_d" "$2"
	done
	summary
}

echo "== 0. Prerequisites =="
require_cmd go docker incus jq
require_docker_credential_helper
check_prereqs

if ! info="$(incus query "$REMOTE:/1.0" 2>&1)"; then
	skip_all incus-remote "incus remote '$REMOTE' not reachable (set VALIDATE_INCUS_REMOTE): $info"
fi
if [ "$(jq -r '.environment.server_clustered' <<<"$info")" = "true" ]; then
	members="$(incus query "$REMOTE:/1.0/cluster/members" | jq length)"
	if [ "$members" != 1 ]; then
		skip_all incus-remote "'$REMOTE' is a $members-member cluster; this proves a standalone host's upgrade (the multi-member proof is #184's)"
	fi
	NODE="$(jq -r '.environment.server_name' <<<"$info")"
else
	NODE="node0"
fi
G0="$APP-$NODE-g0"
G1="$APP-$NODE-g1"
addr="$(incus remote list -f json | jq -r --arg r "$REMOTE" '.[$r] | .LastWorkingAddr // .Addr // .Addrs[0] // empty' | sed 's|^https://||')"
echo "   remote $REMOTE ($addr), server $(jq -r '.environment.server_version' <<<"$info"), host Instance $NODE"

echo
echo "== 1. Build and import the agent image (as a and b) and the git fixture =="
if ! go -C "$ROOT_DIR" build -o "$WORK_DIR/bootstrap" ./cmd/bootstrap; then
	fail_rest 0 "bootstrap did not build"
fi
build_fixture() {
	build_agent_incus_image "$ROOT_DIR" "$WORK_DIR/img" &&
		build_incus_oci_image "$ROOT_DIR/dev/git-fixture/Dockerfile" "$ROOT_DIR/dev/git-fixture" "$WORK_DIR/img" fixture || return 1
	local fp
	if ! fp="$(incus image import "$WORK_DIR/img/agent-meta.tar.gz" "$WORK_DIR/img/agent-rootfs.tar.gz" "$REMOTE:" --alias "$IMAGE_A" 2>&1)"; then
		fail_why "import" "$fp"
		return 1
	fi
	fp="$(incus image info "$(r "$IMAGE_A")" | awk '/^Fingerprint:/ { print $2 }')"
	incus image alias create "$(r "$IMAGE_B")" "$fp" &&
		incus image set-property "$(r "$IMAGE_A")" type=oci &&
		incus image import "$WORK_DIR/img/fixture-meta.tar.gz" "$WORK_DIR/img/fixture-rootfs.tar.gz" "$REMOTE:" --alias "$FIXTURE_IMAGE" >/dev/null &&
		incus image set-property "$(r "$FIXTURE_IMAGE")" type=oci
}
check "${_checks[0]}" build_fixture
[ "$VALIDATE_FAIL" -gt 0 ] && fail_rest 1 "no images"

if ! out="$(incus launch "$(r "$FIXTURE_IMAGE")" "$(r "$FIXTURE")" 2>&1)"; then
	skip_all incus-instances "'$REMOTE' could not start a container: $out"
fi
fixture_ip() {
	incus query "$REMOTE:/1.0/instances/$FIXTURE/state" 2>/dev/null |
		jq -r '[.network // {} | to_entries[] | select(.key != "lo") | .value.addresses[] | select(.family == "inet") | .address][0] // empty'
}
has_ip() { [ -n "$(fixture_ip)" ]; }
wait_for 60 has_ip
ip="$(fixture_ip)"
if [ -z "$ip" ]; then
	fail_rest 1 "the git fixture got no IPv4 address on the default profile's network"
fi
# Not the image's own fleet.git: that's the dev fixture's fleet.
REPO_URL="git://$ip/validate.git"
incus exec "$(r "$FIXTURE")" -- sh -c '
	set -e
	git init -q --bare -b main /srv/git/validate.git
	git clone -q /srv/git/validate.git /work 2>/dev/null
	git -C /work config user.email validate@example.invalid
	git -C /work config user.name validate
	git -C /work config commit.gpgsign false'
if ! commit_fleet "$IMAGE_A" "agent on image a"; then
	fail_rest 1 "could not commit the first fleet.yaml in the fixture"
fi
echo "   config repo $REPO_URL"

if ! incus profile show "$(r incus-socket)" >/dev/null 2>&1; then
	incus profile create "$(r incus-socket)" >/dev/null &&
		incus profile set "$(r incus-socket)" user.created-by="$PREFIX" &&
		incus profile device add "$(r incus-socket)" incus-socket proxy bind=instance \
			listen=unix:/dev/incus-host.sock connect=unix:/var/lib/incus/unix.socket >/dev/null &&
		PROFILE_CREATED=1
	echo "   created the incus-socket profile (deleted on exit)"
fi

echo
echo "== 2. bootstrap deploy-agent, then lower the knobs and wait for it to act =="
fleet_yaml "$IMAGE_A" >"$WORK_DIR/fleet.yaml"
if out="$("$WORK_DIR/bootstrap" deploy-agent --file "$WORK_DIR/fleet.yaml" --addr "$addr" \
	--cert "$INCUS_CONF_DIR/client.crt" --key "$INCUS_CONF_DIR/client.key" \
	--config-repo-url "$REPO_URL" --timeout 2m 2>&1)" && grep -q "deployed $G0" <<<"$out"; then
	record_pass "${_checks[1]}"
else
	record_fail "${_checks[1]}" "$out"
	fail_rest 2 "no first agent"
fi
# Stopped first: the CLI's config write is ETag-guarded, and a running agent
# writing its own keys every tick makes it lose the race. The stop itself can
# meet one of those writes in flight ("busy running an update"), so retry it.
retry() { # retry <cmd...> — up to 10 tries, a second apart
	local i
	for i in $(seq 1 10); do
		"$@" 2>/dev/null && return 0
		sleep 1
	done
	"$@"
}
retry incus stop -f "$(r "$G0")" &&
	incus config set "$(r "$G0")" \
		environment.AGENT_TICK_INTERVAL="$TICK" \
		environment.AGENT_HEARTBEAT_STALE_AFTER="${STALE_AFTER}s" \
		environment.AGENT_MIN_HEALTHY_DURATION="${MIN_HEALTHY}s" \
		environment.AGENT_UPGRADE_HEALTH_DEADLINE="$DEADLINE" &&
	incus start "$(r "$G0")" || fail_rest 2 "could not set the first agent's knobs"
g0_acts() { [ "$(cfg "$G0" user.homelab-ops.agent.acting)" = true ]; }
if wait_for 90 g0_acts; then
	record_pass "${_checks[2]}"
else
	record_fail "${_checks[2]}" "status: $(cfg "$G0" user.homelab-ops.agent.status)"
	fail_rest 3 "the first agent never acted"
fi

echo
echo "== 3. Bump the agent App to image b, and watch =="
sample_forever &
SAMPLER_PID=$!
sleep 1
bumped_at="$(date +%s)"
if ! commit_fleet "$IMAGE_B" "agent on image b"; then
	fail_rest 3 "could not commit the image bump"
fi
settled() {
	agents_json | jq -e --arg g1 "$G1" --arg b "$IMAGE_B" \
		'length == 1 and .[0].name == $g1 and .[0].config["user.homelab-ops.agent.acting"] == "true"
		 and (.[0].config["user.homelab-ops.image"] | fromjson | .alias) == $b' >/dev/null
}
if wait_for 180 settled; then
	echo "   settled $(($(date +%s) - bumped_at))s after the bump"
else
	echo "   not settled after 180s: $(agents_json | jq -c '[.[] | {name, status, acting: .config["user.homelab-ops.agent.acting"], status_key: .config["user.homelab-ops.agent.status"]}]')"
fi
sleep 3 # a few samples of the settled state
kill "$SAMPLER_PID" 2>/dev/null
wait "$SAMPLER_PID" 2>/dev/null
SAMPLER_PID=""

# first <jq predicate over one sample's agents> — the epoch of the first
# sample after the bump where it holds, or empty.
first() {
	awk -v s="$bumped_at" '$1 >= s' "$SAMPLES" |
		while read -r at json; do
			if jq -e --arg g0 "$G0" --arg g1 "$G1" "$1" <<<"$json" >/dev/null 2>&1; then
				echo "$at"
				break
			fi
		done
}
t_g1="$(first 'any(.[]; .name == $g0 and .status == "Running") and any(.[]; .name == $g1)')"
t_healthy="$(first 'any(.[]; .name == $g1 and (.config["user.homelab-ops.healthy-since"] // "") != "")')"
t_drain="$(first 'any(.[]; .name == $g0 and .config["user.homelab-ops.agent.draining"] == "true" and .config["user.homelab-ops.agent.drain-reason"] == "superseded")')"
t_acts="$(first 'any(.[]; .name == $g1 and .config["user.homelab-ops.agent.acting"] == "true")')"
t_gone="$(first 'all(.[]; .name != $g0)')"
echo "   seconds after the bump: g1 $((${t_g1:-0} - bumped_at)), healthy $((${t_healthy:-0} - bumped_at)), drain $((${t_drain:-0} - bumped_at)), g1 acts $((${t_acts:-0} - bumped_at)), g0 gone $((${t_gone:-0} - bumped_at))"

in_order() { # in_order <earlier> <later> — both set, earlier <= later
	[ -n "$1" ] && [ -n "$2" ] && [ "$1" -le "$2" ]
}
check "${_checks[3]}" test -n "$t_g1"
check "${_checks[4]}" in_order "$t_g1" "$t_healthy"
check "${_checks[5]}" in_order "$t_healthy" "$t_drain"

healthy_since="$(awk -v s="$bumped_at" '$1 >= s' "$SAMPLES" | cut -d' ' -f2- |
	jq -r --arg g1 "$G1" '.[] | select(.name == $g1) | .config["user.homelab-ops.healthy-since"] // empty' | head -1)"
if [ -n "$t_drain" ] && [ -n "$healthy_since" ] &&
	[ $((t_drain - $(date -d "$healthy_since" +%s))) -ge "$MIN_HEALTHY" ]; then
	record_pass "${_checks[6]}"
else
	record_fail "${_checks[6]}" "healthy-since=$healthy_since, drain seen at $t_drain, minimum ${MIN_HEALTHY}s"
fi
check "${_checks[7]}" in_order "$t_drain" "$t_acts"

if in_order "$t_acts" "$t_gone" && console_tail "$G1" 200 | grep -q "retired $G0"; then
	record_pass "${_checks[8]}"
else
	record_fail "${_checks[8]}" "g1 acts at $t_acts, g0 gone at $t_gone; g1's log: $(console_tail "$G1" 200 | grep -E 'upgrade|ACTING' | tail -5 | tr '\n' '|')"
fi
check "${_checks[9]}" settled

max_acting="$(cut -d' ' -f2- "$SAMPLES" |
	jq -s '[.[] | [.[] | select(.status == "Running" and .config["user.homelab-ops.agent.acting"] == "true")] | length] | max // 0')"
check_eq "${_checks[10]}" 1 "$max_acting"

# The largest heartbeat age, over every sample, of the freshest Running agent.
worst_gap="$(while read -r at json; do
	jq -r --argjson at "$at" '[.[] | select(.status == "Running") | .config["user.homelab-ops.agent.heartbeat"] // empty | fromdateiso8601]
		| if length == 0 then 999999 else $at - max end' <<<"$json"
done <"$SAMPLES" | sort -n | tail -1)"
samples="$(wc -l <"$SAMPLES")"
if [ -n "$worst_gap" ] && [ "$worst_gap" -lt "$STALE_AFTER" ]; then
	record_pass "${_checks[11]}"
	echo "   $samples samples; the freshest heartbeat was at most ${worst_gap}s old"
else
	record_fail "${_checks[11]}" "over $samples samples, the freshest heartbeat was once ${worst_gap:-?}s old (bound ${STALE_AFTER}s)"
fi

echo
summary
