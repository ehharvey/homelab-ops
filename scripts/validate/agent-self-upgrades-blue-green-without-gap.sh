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
#      healthy (on -g0's own instance, the primary's bookkeeping); -g0 drains
#      with reason `superseded`, no sooner than AGENT_MIN_HEALTHY_DURATION
#      into the streak in force when it drained; -g1 acts; -g0 is gone,
#      retired by -g1 (its own log says so); exactly one agent instance is
#      left, on image b. At every sample at most one instance acts, and the
#      freshest heartbeat never goes STALE_AFTER without a newer one: the
#      literal "no gap".
#
# Which clocks the checks compare (#683, #533). The samples are stamped with
# this host's clock, when each read returned; the agents stamp their keys
# (heartbeat, healthy-since) with the Incus host's. The two can disagree by
# seconds (a queue VM's clock), so no check subtracts one from the other:
#   - The order checks (-g1 appears, is marked healthy, -g0 drains, -g1 acts,
#     -g0 is gone) compare sample stamps with sample stamps.
#   - The minimum healthy duration compares two stamps -g0 wrote: its
#     healthy-since for -g1, and its own heartbeat from the tick in which it
#     drained (the first sample where its status starts "superseded:", which
#     that tick writes after the drain). Slack: the agent judged the streak
#     at the tick's listing time, earlier in that tick than the heartbeat it
#     writes at the end, so a drain early by up to that tick's run time
#     (sync, election, the Incus calls) passes. Blind spot: if no sample
#     caught that tick, it falls back to -g0's first heartbeat after it,
#     about one tick (TICK) later, so a drain up to about a tick early can
#     pass. One earlier than that still fails.
#   - "No gap" compares heartbeats with heartbeats: the freshest Running
#     agent's heartbeat over the samples, and the largest step between its
#     successive values. A read's latency never counts as age. Nor does
#     write latency: heartbeats that land late but steadily (each visible
#     some seconds after its stamp) keep steps near TICK and pass, though a
#     peer judging age by its own clock could find them stale. That is the
#     tradeoff for one clock (#533, #683); clock_offset's log lines say how
#     far apart the two clocks were.
# Both hold on a standalone host, where every agent stamps with one clock
# (docs/Decisions.md §30); across cluster members they would need skew.
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
#   - The incus-socket profile (internal/seed). If the host lacks one, each
#     run creates it (user.created-by: validate-upg-<pid>) and deletes it on
#     exit, once none of the run's instances is left to use it; while it
#     exists, anything that uses it is root on the host's Incus. Only this
#     run's agents do. The hardware queue's sweep never deletes a profile
#     (docs/Decisions.md §21), so one a run leaves behind stays until
#     someone runs `incus profile delete <remote>:incus-socket`, and every
#     later run finds it present and leaves it alone.
#   - Everything else is prefixed validate-upg-<pid> (step 0 prints it) and
#     deleted on exit: the instances found by the App's label or by that
#     prefix, deleted and listed again until none is left or CLEANUP_WAIT
#     passes, then the images. Anything left gets a WARN: line naming it
#     and the command that deletes it. A run killed with KILL runs no
#     cleanup at all. The agents are an App of their own (validate-upg-<pid>),
#     so they never elect with, fence or touch real agents on the same Incus.
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
VALIDATE_SUPPORTED_OS="incusos"

validate_parse_args "$@"

REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
INCUS_CONF_DIR="${INCUS_CONF:-$HOME/.config/incus}"
TICK=2s
STALE_AFTER=10   # AGENT_HEARTBEAT_STALE_AFTER, seconds; also the "no gap" bound
MIN_HEALTHY=12   # AGENT_MIN_HEALTHY_DURATION, seconds
DEADLINE=2m      # AGENT_UPGRADE_HEALTH_DEADLINE
# Seconds cleanup keeps deleting and listing this run's instances; each Incus
# call in that loop is cut off at the deadline, and each image or profile call
# after it at CLEANUP_CALL_WAIT. Inside the hardware queue's HQ_KILL_GRACE
# (120s) unless the remote stops answering: then the worst case (the loop, a
# console dump per agent, every later call timing out) can run past it, and
# the KILL skips what's left.
CLEANUP_WAIT=60
CLEANUP_CALL_WAIT=10

WORK_DIR="$(mktemp -d)"
SAMPLES="$WORK_DIR/samples" # one line per sample: <epoch> <json array of this run's agents>
PREFIX="validate-upg-$$"
APP="$PREFIX"
IMAGE_A="$PREFIX-a"
IMAGE_B="$PREFIX-b"
FIXTURE="$PREFIX-git"
FIXTURE_IMAGE="$PREFIX-git"
PROFILE_CREATED=""
CREATED_ANY="" # set before the run's first import: until then cleanup has nothing on the remote to delete
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
	"the freshest heartbeat never goes STALE_AFTER without a newer one"
)

r() { printf '%s:%s' "$REMOTE" "$1"; } # r <name> — name on the remote

# agents_json — this run's agent instances as [{name, status, config}].
agents_json() {
	incus query "$REMOTE:/1.0/instances?recursion=1" 2>/dev/null |
		jq -c --arg a "$APP" '[.[] | select(.config["user.homelab-ops.app"] == $a) | {name, status, config}]' 2>/dev/null
}

# sample_arrays — each sample's agents array, one per line, dropping any line
# that isn't one: a sample the sampler's kill cut short, or a failed read. One
# bad line then costs that sample, not every check that reads them all.
sample_arrays() {
	cut -d' ' -f2- "$SAMPLES" | jq -c -R 'fromjson? | arrays'
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

# clock_offset — the Incus host's clock less this host's, as the range
# "-0.216s..+0.200s": five reads in the fixture (a container keeps its host's
# clock), each between two reads of this host's, the ranges they allow
# intersected. The fixture's busybox date has no %N, so its reads are whole
# seconds, which each widen by a second. Informational: no check compares the
# two clocks (the header), but a log that shows how far apart they were says
# what a check that did would have seen (#683).
clock_offset() {
	local _ reads=""
	for _ in 1 2 3 4 5; do
		reads+="$(date +%s.%N) $(incus exec "$(r "$FIXTURE")" -- date +%s.%N 2>/dev/null) $(date +%s.%N)"$'\n'
	done
	awk 'NF == 3 && $2 ~ /^[0-9]+(\.[0-9]*)?$/ {
			w = $2 ~ /\.[0-9]+$/ ? 0 : 1
			l = $2 - $3; h = $2 + w - $1
			if (!n++ || l > lo) lo = l
			if (n == 1 || h < hi) hi = h
		}
		END { if (!n) exit 1; printf "%+.3fs..%+.3fs", lo, hi }' <<<"$reads"
}

# run_instances — the names of this run's instances: its agents, by their
# App's label, and anything named $PREFIX-*, which takes in the fixture and an
# agent a create left unlabelled. Fails, with the error on stderr, when the
# listing does: an empty answer then is not "none left". The listing is cut
# off after <secs>.
# shellcheck disable=SC2329 # run by cleanup
run_instances() { # run_instances <secs>
	local out
	if ! out="$(timeout "$1" incus query "$REMOTE:/1.0/instances?recursion=1" 2>&1)"; then
		printf '%s\n' "${out:-timed out after $1s}" >&2
		return 1
	fi
	jq -r --arg a "$APP" --arg p "$PREFIX-" \
		'.[] | select(.config["user.homelab-ops.app"] == $a or (.name | startswith($p))) | .name' <<<"$out"
}

# present <api path> — 0 if the remote has that object, 1 if Incus says it's
# not found, 2 if it couldn't tell (the error on stderr).
# shellcheck disable=SC2329 # run by cleanup
present() {
	local out
	out="$(timeout "$CLEANUP_CALL_WAIT" incus query "$REMOTE:$1" 2>&1)" && return 0
	grep -qi 'not found' <<<"$out" && return 1
	printf '%s\n' "${out:-timed out after ${CLEANUP_CALL_WAIT}s}" >&2
	return 2
}

# capped <secs> <errfile> <cmd...> — cmd with its stderr in errfile, cut off
# after secs (then errfile says so).
# shellcheck disable=SC2329 # run by cleanup
capped() {
	local secs="$1" errf="$2" rc
	shift 2
	timeout "$secs" "$@" >/dev/null 2>"$errf"
	rc=$?
	[ "$rc" -eq 124 ] && echo "timed out after ${secs}s" >"$errf"
	return "$rc"
}

# shellcheck disable=SC2329 # run by cleanup
warn() { echo "WARN: $*"; }

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
	local n img left err="" errf="$WORK_DIR/cleanup.err" deadline=$((SECONDS + CLEANUP_WAIT)) failed
	local -A why=()
	[ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null
	# A run that stopped before its first import (a SKIP, a build failure)
	# has nothing on the remote, which may not even exist.
	if [ -z "$CREATED_ANY" ]; then
		rm -rf "$WORK_DIR"
		return
	fi
	if [ "$VALIDATE_FAIL" -gt 0 ]; then
		for n in $(agents_json | jq -r '.[].name'); do
			echo "--- $n console (last 20 lines)"
			timeout "$CLEANUP_CALL_WAIT" incus console "$(r "$n")" --show-log 2>/dev/null | tail -n 20
		done
	fi
	# remaining <floor> — seconds to the deadline, at least floor: the cap on
	# each call. A listing gets CLEANUP_CALL_WAIT even at the deadline, since
	# the WARNs and the profile delete go by the last one.
	remaining() { local s=$((deadline - SECONDS)); echo $((s > $1 ? s : $1)); }
	# List, delete, repeat: an acting agent can create a successor after a
	# listing, and a delete can fail while an instance is busy (#677). The
	# deadline is checked between a listing and its deletes, so the last
	# listing, which the WARNs report, has no deletes after it.
	while :; do
		failed=""
		if left="$(run_instances "$(remaining "$CLEANUP_CALL_WAIT")" 2>"$errf")"; then
			err=""
			[ -z "$left" ] && break
			[ "$SECONDS" -ge "$deadline" ] && break
			for n in $left; do
				if capped "$(remaining 1)" "$errf" incus delete -f "$(r "$n")"; then
					unset 'why[$n]'
				else
					why[$n]="$(tr '\n' ' ' <"$errf")"
					failed=1
				fi
			done
		else
			err="$(tr '\n' ' ' <"$errf")"
			[ "$SECONDS" -ge "$deadline" ] && break
			failed=1
		fi
		[ -n "$failed" ] && sleep 2
	done
	if [ -n "$err" ]; then
		warn "could not list $REMOTE's instances to delete this run's (${err% }); find them with: incus list $REMOTE: $PREFIX- and delete each with: incus delete -f $REMOTE:<name>"
	else
		for n in $left; do
			warn "instance $REMOTE:$n is still there after ${CLEANUP_WAIT}s${why[$n]:+ (${why[$n]% })}; delete it with: incus delete -f $REMOTE:$n"
		done
	fi
	# Image b is an alias of image a's image: one delete takes both.
	for img in "$IMAGE_A" "$IMAGE_B" "$FIXTURE_IMAGE"; do
		capped "$CLEANUP_CALL_WAIT" "$errf" incus image delete "$(r "$img")" && continue
		present "/1.0/images/aliases/$img" 2>"$errf.present"
		case $? in
		0) warn "image $REMOTE:$img is still there ($(tr '\n' ' ' <"$errf")); delete it with: incus image delete $REMOTE:$img" ;;
		2) warn "could not tell whether image $REMOTE:$img is gone ($(tr '\n' ' ' <"$errf.present")); delete it with: incus image delete $REMOTE:$img" ;;
		esac
	done
	# The profile last, and only once no instance of this run is left to use it.
	if [ -n "$PROFILE_CREATED" ]; then
		if [ -n "$err" ] || [ -n "$left" ]; then
			warn "kept the incus-socket profile this run created, since its instances may still use it; once they're gone, delete it with: incus profile delete $REMOTE:incus-socket"
		elif ! capped "$CLEANUP_CALL_WAIT" "$errf" incus profile delete "$(r incus-socket)"; then
			present "/1.0/profiles/incus-socket" 2>"$errf.present"
			case $? in
			0) warn "the incus-socket profile this run created is still on $REMOTE ($(tr '\n' ' ' <"$errf")); delete it with: incus profile delete $REMOTE:incus-socket" ;;
			2) warn "could not tell whether the incus-socket profile this run created is gone ($(tr '\n' ' ' <"$errf.present")); delete it with: incus profile delete $REMOTE:incus-socket" ;;
			esac
		fi
	fi
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
echo "   this run's prefix: $PREFIX (its instances and images; deleted on exit)"

echo
echo "== 1. Build and import the agent image (as a and b) and the git fixture =="
if ! go -C "$ROOT_DIR" build -o "$WORK_DIR/bootstrap" ./cmd/bootstrap; then
	fail_rest 0 "bootstrap did not build"
fi
# shellcheck disable=SC2329 # run through check
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
CREATED_ANY=1 # from here an import may have landed, even one that then fails
check "${_checks[0]}" build_fixture
[ "$VALIDATE_FAIL" -gt 0 ] && fail_rest 1 "no images"

if ! out="$(incus launch "$(r "$FIXTURE_IMAGE")" "$(r "$FIXTURE")" 2>&1)"; then
	skip_all incus-instances "'$REMOTE' could not start a container: $out"
fi
fixture_ip() {
	incus query "$REMOTE:/1.0/instances/$FIXTURE/state" 2>/dev/null |
		jq -r '[.network // {} | to_entries[] | select(.key != "lo") | .value.addresses[] | select(.family == "inet") | .address][0] // empty'
}
# shellcheck disable=SC2329 # polled through wait_for
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
echo "   $REMOTE's clock is $(clock_offset || echo '(unread)') from this host's"

if ! incus profile show "$(r incus-socket)" >/dev/null 2>&1; then
	# Marked created as soon as it exists, so cleanup deletes a half-made one
	# too: left behind without its proxy device, every later run and deploy
	# would find it present and use it.
	if incus profile create "$(r incus-socket)" >/dev/null; then
		PROFILE_CREATED=1
	fi
	if [ -z "$PROFILE_CREATED" ] ||
		! incus profile set "$(r incus-socket)" user.created-by="$PREFIX" ||
		! incus profile device add "$(r incus-socket)" incus-socket proxy bind=instance \
			listen=unix:/dev/incus-host.sock connect=unix:/var/lib/incus/unix.socket >/dev/null; then
		fail_rest 1 "could not create the incus-socket profile"
	fi
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
	local _
	for _ in $(seq 1 10); do
		"$@" 2>/dev/null && return 0
		sleep 1
	done
	"$@"
}
{ retry incus stop -f "$(r "$G0")" &&
	incus config set "$(r "$G0")" \
		environment.AGENT_TICK_INTERVAL="$TICK" \
		environment.AGENT_HEARTBEAT_STALE_AFTER="${STALE_AFTER}s" \
		environment.AGENT_MIN_HEALTHY_DURATION="${MIN_HEALTHY}s" \
		environment.AGENT_UPGRADE_HEALTH_DEADLINE="$DEADLINE" &&
	incus start "$(r "$G0")"; } || fail_rest 2 "could not set the first agent's knobs"
# shellcheck disable=SC2329 # polled through wait_for
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
# shellcheck disable=SC2329 # polled through wait_for, then run through check
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
# End the watch on a heartbeat newer than any so far, so a gap still open when
# it ends is closed in the samples, and measured below like any other. The
# wait is generous (two STALE_AFTERs, for slow reads): a gap it outlasts fails
# outright, and a shorter one is measured on the agents' clock.
# shellcheck disable=SC2329 # polled through wait_for
freshest_hb() { # the newest heartbeat of a Running agent of this run, epoch s by the agent's clock
	agents_json | jq -r '[.[] | select(.status == "Running") | .config["user.homelab-ops.agent.heartbeat"] // empty | fromdateiso8601] | max // empty'
}
hb_settled="$(freshest_hb)"
# shellcheck disable=SC2329 # polled through wait_for
hb_advanced() {
	local h
	h="$(freshest_hb)"
	[ -n "$h" ] && [ "$h" -gt "${hb_settled:-0}" ]
}
hb_stuck=""
wait_for $((2 * STALE_AFTER)) hb_advanced || hb_stuck=1
kill "$SAMPLER_PID" 2>/dev/null
wait "$SAMPLER_PID" 2>/dev/null
SAMPLER_PID=""
# A kill mid-write leaves the last line cut short: end it, so the final
# sample gets a line of its own rather than joining it (sample_arrays drops
# the cut one).
[ -n "$(tail -c1 "$SAMPLES")" ] && echo >>"$SAMPLES"
printf '%s %s\n' "$(date +%s)" "$(agents_json)" >>"$SAMPLES"
echo "   $REMOTE's clock is $(clock_offset || echo '(unread)') from this host's"

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
# shellcheck disable=SC2016 # a jq program; $g0/$g1 are jq --arg variables
t_g1="$(first 'any(.[]; .name == $g0 and .status == "Running") and any(.[]; .name == $g1)')"
# The primary's bookkeeping for this host, on its own instance (-g0's).
HEALTHY_KEY="user.homelab-ops.upgrade.$NODE.healthy-since"
# shellcheck disable=SC2016 # a jq program; $g0/$g1 are jq --arg variables
t_healthy="$(first 'any(.[]; .name == $g0 and (.config["'"$HEALTHY_KEY"'"] // "") != "")')"
# shellcheck disable=SC2016 # a jq program; $g0/$g1 are jq --arg variables
t_drain="$(first 'any(.[]; .name == $g0 and .config["user.homelab-ops.agent.draining"] == "true" and .config["user.homelab-ops.agent.drain-reason"] == "superseded")')"
# shellcheck disable=SC2016 # a jq program; $g0/$g1 are jq --arg variables
t_acts="$(first 'any(.[]; .name == $g1 and .config["user.homelab-ops.agent.acting"] == "true")')"
# shellcheck disable=SC2016 # a jq program; $g0/$g1 are jq --arg variables
t_gone="$(first 'all(.[]; .name != $g0)')"
echo "   seconds after the bump (at $(date -u -d "@$bumped_at" +%FT%TZ), this host's clock): g1 $((${t_g1:-0} - bumped_at)), healthy $((${t_healthy:-0} - bumped_at)), drain $((${t_drain:-0} - bumped_at)), g1 acts $((${t_acts:-0} - bumped_at)), g0 gone $((${t_gone:-0} - bumped_at))"

in_order() { # in_order <earlier> <later> — both set, earlier <= later
	[ -n "$1" ] && [ -n "$2" ] && [ "$1" -le "$2" ]
}
check "${_checks[3]}" test -n "$t_g1"
check "${_checks[4]}" in_order "$t_g1" "$t_healthy"
check "${_checks[5]}" in_order "$t_healthy" "$t_drain"

# drained_g0 <jq filter on -g0's status> — "<heartbeat>|<healthy-since>" from
# the first sample with -g0 drained as superseded and its status matching. The
# streak is the one that sample shows, in force when -g0 drained: a streak
# that restarted makes the first one ever seen too early a baseline.
drained_g0() {
	sample_arrays | jq -r --arg g0 "$G0" --arg k "$HEALTHY_KEY" '.[]
		| select(.name == $g0 and .config["user.homelab-ops.agent.draining"] == "true"
			and .config["user.homelab-ops.agent.drain-reason"] == "superseded"
			and (.config["user.homelab-ops.agent.status"] // "" | '"$1"'))
		| "\(.config["user.homelab-ops.agent.heartbeat"] // "")|\(.config[$k] // "")"' 2>/dev/null | head -1
}
# Both stamps are -g0's, on the Incus host's clock (the header). The drain
# tick's heartbeat says "superseded:", stamped at the end of the tick that
# judged the streak at its start: slack of that tick's run time. Failing that,
# the first one -g0 wrote after the drain (no longer "leader:"), a tick or so
# late: the blind spot.
drain_tick="the drain tick's"
IFS='|' read -r drain_hb healthy_since <<<"$(drained_g0 'startswith("superseded:")')"
if [ -z "$drain_hb" ]; then
	drain_tick="the first after the drain; no sample caught the drain tick's"
	IFS='|' read -r drain_hb healthy_since <<<"$(drained_g0 'startswith("leader:") | not')"
fi
healthy_for=""
if [ -n "$drain_hb" ] && [ -n "$healthy_since" ]; then
	healthy_for="$(jq -rn --arg hb "$drain_hb" --arg since "$healthy_since" \
		'($hb | fromdateiso8601) - ($since | fromdateiso8601)' 2>/dev/null)"
fi
if [ -n "$healthy_for" ] && [ "$healthy_for" -ge "$MIN_HEALTHY" ]; then
	record_pass "${_checks[6]}"
	echo "   healthy ${healthy_for}s when -g0 drained (healthy-since $healthy_since, -g0's heartbeat $drain_hb, $drain_tick), minimum ${MIN_HEALTHY}s"
else
	record_fail "${_checks[6]}" "healthy-since=${healthy_since:-?}, -g0's heartbeat ${drain_hb:-?} ($drain_tick): healthy ${healthy_for:-?}s, minimum ${MIN_HEALTHY}s"
fi
check "${_checks[7]}" in_order "$t_drain" "$t_acts"

if in_order "$t_acts" "$t_gone" && console_tail "$G1" 200 | grep -q "retired $G0"; then
	record_pass "${_checks[8]}"
else
	record_fail "${_checks[8]}" "g1 acts at $t_acts, g0 gone at $t_gone; g1's log: $(console_tail "$G1" 200 | grep -E 'upgrade|ACTING' | tail -5 | tr '\n' '|')"
fi
check "${_checks[9]}" settled

max_acting="$(sample_arrays |
	jq -s '[.[] | [.[] | select(.status == "Running" and .config["user.homelab-ops.agent.acting"] == "true")] | length] | max // 0')"
check_eq "${_checks[10]}" 1 "$max_acting"

# The longest the freshest heartbeat went without a newer one, on the agents'
# clock alone (#683): per sample, the newest heartbeat of a Running agent;
# over the samples, the largest step from one new value of it to the next.
# The age therefore comes from agent-written stamps only, and excludes the
# read's latency (#533): a slow read can make the sampler miss a value, which
# lengthens a step, never shortens it. The stamps are whole seconds, so the
# step is to the second. A sample with no Running agent's heartbeat adds no
# value; the gap it sits in shows as the step to the next one, and the watch
# above ends on a new heartbeat (or hb_stuck).
# The worst step's ends are named (agent and stamp), so a FAIL says when in
# the upgrade the gap fell and whose heartbeat closed it.
read -r worst_gap no_hb worst_at <<<"$(sample_arrays | jq -rs '
	[.[] | [.[] | select(.status == "Running") | {n: .name, h: (.config["user.homelab-ops.agent.heartbeat"] // empty | fromdateiso8601)}] | max_by(.h)]
	| reduce .[] as $s ({m: null, worst: 0, none: 0, at: "-"};
		if $s == null then .none += 1
		elif .m == null then .m = $s
		elif $s.h > .m.h then
			(if $s.h - .m.h > .worst then .worst = $s.h - .m.h | .at = "\(.m.n)@\(.m.h | todate)..\($s.n)@\($s.h | todate)" else . end) | .m = $s
		else . end)
	| "\(.worst) \(.none) \(.at)"' 2>/dev/null)"
samples="$(wc -l <"$SAMPLES")"
if [ -z "$hb_stuck" ] && [ -n "$worst_gap" ] && [ "$worst_gap" -lt "$STALE_AFTER" ]; then
	record_pass "${_checks[11]}"
	echo "   $samples samples; the freshest heartbeat went at most ${worst_gap}s without a newer one (bound ${STALE_AFTER}s; $worst_at); $no_hb samples had no Running agent's heartbeat"
elif [ -n "$hb_stuck" ]; then
	record_fail "${_checks[11]}" "no heartbeat newer than ${hb_settled:-none} within $((2 * STALE_AFTER))s of the watch's end (bound ${STALE_AFTER}s)"
else
	record_fail "${_checks[11]}" "over $samples samples, the freshest heartbeat once went ${worst_gap:-?}s without a newer one (bound ${STALE_AFTER}s; ${worst_at:-?}); $no_hb samples had no Running agent's heartbeat"
fi

echo
summary
