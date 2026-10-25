#! /bin/bash
# Docker-compose helpers for the validate suite (#140). Source after lib.sh.
#
# Sourced by the scripts whose VALIDATE_GROUP is "compose" — the ones that
# bring up the real stack from docker-compose.yml (web + the dev/git-fixture
# git remote, and for the observability script, loki/prometheus/grafana) and
# assert against its HTTP API and logs.

# compose — docker compose anchored at the repo's compose file, plus a scoped
# override when the script needs one.
#
# The override idiom exists because the web app's *deployment* config (the
# break-glass cert, the base image path, the WireGuard endpoint) must not be
# baked into the committed docker-compose.yml. Scripts mktemp an override,
# export the vars it interpolates, and set VALIDATE_COMPOSE_OVERRIDE.
#
# Anchoring on $ROOT_DIR rather than relying on the caller's cwd: some scripts
# previously used a bare `docker compose`, which only worked because they cd'd
# to the repo root first.
#
# Before anything that may publish a port it runs _compose_ports_listed, so a
# script whose override publishes a host port missing from COMPOSE_HOST_PORTS
# fails instead of starting the stack (#271). It renders the config with the
# call's own global options (-f, --env-file, ...), so it checks the config the
# call would start.
compose() {
	_compose_parse "$@"
	case "$_COMPOSE_SUB" in
	# Subcommands that create nothing, so publish nothing; any other one, an
	# unknown one included, is checked.
	down | ps | logs | exec | config | version | ls | images | top | port | stop | kill | rm | pause | unpause | events | cp | wait | stats | attach) ;;
	*) _compose_ports_listed "$@" || return 1 ;;
	esac
	_compose "$@"
}

# _compose_parse <compose args...> — splits a compose command line into its
# global options (_COMPOSE_GLOBALS) and its subcommand (_COMPOSE_SUB): the
# first word that isn't a global option or its value. Globals instead of
# printing it, so the compose_down poll doesn't fork a subshell per call.
#
# The options are compose's own (`docker compose --help`). An option it doesn't
# know might take a value, which would then look like the subcommand, so an
# unknown one leaves _COMPOSE_SUB empty and the guard refuses the call rather
# than guess.
_compose_parse() {
	_COMPOSE_GLOBALS=()
	_COMPOSE_SUB=
	_COMPOSE_BAD_OPT=
	while [ $# -gt 0 ]; do
		case "$1" in
		-f | --file | -p | --project-name | --profile | --env-file | --project-directory | --ansi | --progress | --parallel)
			_COMPOSE_GLOBALS+=("$1" "${2-}")
			shift 2 || return
			;;
		--file=* | --project-name=* | --profile=* | --env-file=* | --project-directory=* | --ansi=* | --progress=* | --parallel=* | \
			--all-resources | --compatibility | --dry-run | -f?* | -p?*)
			_COMPOSE_GLOBALS+=("$1")
			shift
			;;
		-*)
			_COMPOSE_BAD_OPT="$1"
			return
			;;
		*)
			_COMPOSE_SUB="$1"
			return
			;;
		esac
	done
}

_compose() {
	if [ -n "${VALIDATE_COMPOSE_OVERRIDE:-}" ]; then
		docker compose -f "$ROOT_DIR/docker-compose.yml" -f "$VALIDATE_COMPOSE_OVERRIDE" "$@"
	else
		docker compose -f "$ROOT_DIR/docker-compose.yml" "$@"
	fi
}

# require_compose — the compose group's hard prerequisites: docker compose,
# and jq, which compose's port guard reads the rendered config with. Every
# compose-group script calls it before check_prereqs, so a host without jq
# exits 2 before doing any work rather than failing at its first `compose up`.
# It also probes the docker credential helper (lib.sh's
# require_docker_credential_helper, #300), so a helper that hangs with no
# VS Code client attached is bypassed before the first pull or build.
require_compose() {
	require_docker_compose
	require_cmd jq
	require_docker_credential_helper
}

# COMPOSE_HOST_PORTS — every host port, as port/protocol, that a compose-group
# script may publish: docker-compose.yml's own, plus any a script adds through
# its VALIDATE_COMPOSE_OVERRIDE (8081, the health port, from
# api-serves-operator-peers-over-tunnel-by-default.sh). This is the one list
# (#271): compose_down waits for these to be released, compose refuses to
# bring up a stack that publishes a port not named here, and
# scripts/validate/README.md points here rather than repeating it.
#
# A hand list rather than one derived from `docker compose config`: the
# derived set only sees the current script's override, so it can't say which
# ports the group as a whole contends over, and a guard that checks the config
# against itself can never fail.
COMPOSE_HOST_PORTS=(8080/tcp 8081/tcp 5000/tcp 3000/tcp 3100/tcp 9090/tcp 51820/udp)

# _compose_ports_listed <compose args...> — fail (through fail_why, so the
# reason reaches the FAIL line of the check running `compose up`) for each host
# port the call would publish that COMPOSE_HOST_PORTS doesn't list: those of
# the effective compose config (every profile enabled), and for `run`, its
# -p/--publish ones too. `run` is held to the whole config, not just with
# --service-ports, because it starts the service's depends_on with their
# ports. A port with no fixed host side is ephemeral and contends with
# nothing, so it's ignored.
#
# It fails closed: if the command line won't parse, the config won't render
# or jq is missing, it says why and the stack doesn't start, rather than
# starting unchecked. Expects _compose_parse to have run on the same args.
_compose_ports_listed() {
	local desc="compose publishes only listed host ports" config errf published p spec ok=0
	if [ -z "$_COMPOSE_SUB" ]; then
		fail_why "$desc" "can't tell the subcommand${_COMPOSE_BAD_OPT:+ (unknown global option $_COMPOSE_BAD_OPT; add it to _compose_parse in scripts/validate/lib-compose.sh)}"
		return 1
	fi
	if ! command -v jq >/dev/null 2>&1; then
		fail_why "$desc" "jq not found; the script should call require_compose"
		return 1
	fi
	errf=$(mktemp)
	# stderr kept out of the JSON: Compose warns there about unset variables.
	if ! config=$(_compose ${_COMPOSE_GLOBALS[@]+"${_COMPOSE_GLOBALS[@]}"} --profile '*' config --format json 2>"$errf"); then
		fail_why "$desc" "could not render the compose config: $(tail -n 3 "$errf")"
		rm -f "$errf"
		return 1
	fi
	rm -f "$errf"
	if ! published=$(jq -r '.services[]?.ports[]? | select((.published // "") != "")
		| "\(.published)/\(.protocol // "tcp")"' <<<"$config" 2>&1); then
		fail_why "$desc" "could not read the published ports: $published"
		return 1
	fi
	if [ "$_COMPOSE_SUB" = run ]; then
		# Every -p/--publish in run's arguments. That includes the container
		# command's, which run's options can't be told apart from without
		# copying run's option list here; a false hit fails closed and names
		# the port.
		shift $((${#_COMPOSE_GLOBALS[@]} + 1))
		while [ $# -gt 0 ]; do
			spec=
			case "$1" in
			-p | --publish) spec="${2-}" && shift ;;
			--publish=*) spec="${1#*=}" ;;
			-p?*) spec="${1#-p}" ;;
			esac
			shift
			[ -n "$spec" ] && published+=$'\n'$(_compose_publish_port "$spec")
		done
	fi
	for p in $published; do
		case " ${COMPOSE_HOST_PORTS[*]} " in
		*" $p "*) ;;
		*)
			fail_why "$desc" "$p is not in COMPOSE_HOST_PORTS (scripts/validate/lib-compose.sh); add it there"
			ok=1
			;;
		esac
	done
	return "$ok"
}

# _compose_publish_port <spec> — the host side of a -p/--publish spec,
# [ip:][host:]container[/proto], as port/proto; nothing when there's no fixed
# host side.
_compose_publish_port() {
	local spec="$1" proto=tcp
	case "$spec" in */*)
		proto="${spec##*/}"
		spec="${spec%/*}"
		;;
	esac
	spec="${spec##*]:}" # a bracketed IPv6 host address
	case "$spec" in *:*:*) spec="${spec#*:}" ;; esac
	case "$spec" in *:*) [ -n "${spec%%:*}" ] && echo "${spec%%:*}/$proto" ;; esac
	return 0
}

# compose_down — tear the stack down and block until it is really gone. Every
# compose script's cleanup trap calls this instead of a bare `compose down`, so
# the barrier lives here once rather than as a sleep copied into each script.
#
# Why a barrier at all: run.sh runs the compose group back-to-back, so one
# script's teardown is immediately followed by the next script's `compose up`.
# On this devcontainer's Compose (v5), `down` releases the published ports and
# the project network *before* it returns, so the two don't actually race —
# measured directly while closing #153, and 13 consecutive suite runs stayed
# green. Older Compose tore those down more loosely, letting the next `up`
# collide with a half-removed network or a still-bound port; that is the
# nondeterminism #153 set out to kill. This barrier makes the suite robust to
# that difference — including on a CI runner whose Compose version we don't
# control (#146) — and is a no-op when `down` is already synchronous.
compose_down() {
	compose down --remove-orphans >/dev/null 2>&1
	# Bounded, best-effort: wait until no project container remains and none of
	# the stack's published host ports are still bound, then return. Never fails
	# the caller — a stuck teardown gives up after ~15s rather than hanging the
	# suite, and a port held by some unrelated process is not ours to free.
	local _
	for _ in $(seq 1 30); do
		[ -n "$(compose ps -aq 2>/dev/null)" ] && {
			sleep 0.5
			continue
		}
		_compose_ports_free && return 0
		sleep 0.5
	done
	return 0
}

# _compose_ports_free — false while any of COMPOSE_HOST_PORTS is still bound.
# These are the ports two consecutive compose scripts contend over.
_compose_ports_free() {
	local p flags
	for p in "${COMPOSE_HOST_PORTS[@]}"; do
		case "${p#*/}" in
		udp) flags=-Hlun ;;
		*) flags=-Hltn ;;
		esac
		ss "$flags" "sport = :${p%/*}" 2>/dev/null | grep -q . && return 1
	done
	return 0
}

# wait_web_ready <base_url> [tries] — poll /healthz until the web app answers,
# then assert it. Every compose-family script had its own copy of this loop.
#
# Note this is a real assertion, not just a sleep: if the stack never comes up
# the run should say "web is reachable" failed, not cascade into a dozen
# confusing HTTP errors further down.
wait_web_ready() {
	local base_url="$1" tries="${2:-20}"
	local _
	for _ in $(seq 1 "$tries"); do
		curl -s -o /dev/null "$base_url/healthz" && break
		sleep 0.5
	done
	check "web is reachable" curl -sf -o /dev/null "$base_url/healthz"
}

# wait_http <desc> <url> [tries] — retry until the URL returns 2xx.
wait_http() {
	local desc="$1" url="$2" tries="${3:-30}"
	local _
	for _ in $(seq 1 "$tries"); do
		if curl -sf -o /dev/null "$url"; then
			record_pass "$desc"
			return 0
		fi
		sleep 1
	done
	record_fail "$desc" "unreachable after ${tries}s: $url"
	return 1
}

# wait_json <desc> <url> <jq-filter> [tries] — retry until the response
# satisfies the filter. Reports the last body seen on failure, which is what
# makes a timeout here diagnosable rather than just "didn't happen".
wait_json() {
	local desc="$1" url="$2" filter="$3" tries="${4:-30}"
	local body="" _
	for _ in $(seq 1 "$tries"); do
		body=$(curl -s "$url")
		if echo "$body" | jq -e "$filter" >/dev/null 2>&1; then
			record_pass "$desc"
			return 0
		fi
		sleep 1
	done
	record_fail "$desc" "condition not met after ${tries}s; last response: $body"
	return 1
}

# _grep_web_log <pattern> — fixed-string search of the web service's logs.
_grep_web_log() {
	compose logs web 2>/dev/null | grep -qF -- "$1"
}

# check_log <desc> <pattern> — one-shot log assertion, for lines that must
# already have been emitted by the time the check runs.
check_log() {
	local desc="$1" pattern="$2"
	if _grep_web_log "$pattern"; then
		record_pass "$desc"
	else
		record_fail "$desc" "pattern not found: $pattern"
	fi
}

# wait_log <desc> <pattern> [tries] — retrying log assertion, for lines a
# background loop will emit eventually (the config-sync poller, for instance).
wait_log() {
	local desc="$1" pattern="$2" tries="${3:-30}"
	local _
	for _ in $(seq 1 "$tries"); do
		if _grep_web_log "$pattern"; then
			record_pass "$desc"
			return 0
		fi
		sleep 1
	done
	record_fail "$desc" "pattern not found after ${tries}s: $pattern"
	return 1
}

# push_fleet <committer> <fleet-yaml> — replace fleet.yaml in the fixture git
# remote and push it, so the next sync sees a new commit.
#
# The fixture repo is rebuilt from its image on every `compose up --build`, so
# scripts that push their own fleet stay re-runnable. Scripts that must not
# disturb the committed dev/git-fixture/fleet.yaml baseline (which several
# assert exact counts against) use this instead of editing the file in-tree.
push_fleet() {
	local committer="$1" fleet_yaml="$2"
	compose exec -T config-repo sh -c "
		set -e
		rm -rf /tmp/validate-work
		git clone --no-hardlinks /srv/git/fleet.git /tmp/validate-work
		cd /tmp/validate-work
		git config user.email dev@homelab-ops.local
		git config user.name '$committer'
		cat > fleet.yaml <<'FLEET_EOF'
$fleet_yaml
FLEET_EOF
		git add fleet.yaml
		git commit -m '$committer: fleet update' >/dev/null
		git push origin main >/dev/null 2>&1
	" >/dev/null 2>&1
}
