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
#
# A helper that cmd calls can say why it failed, through fail_why: the reason
# lands on check's FAIL line instead of in the discarded output.
check() {
	local desc="$1" _VALIDATE_IN_CHECK=1 _VALIDATE_REASON=
	shift
	if "$@" >/dev/null 2>&1; then
		record_pass "$desc"
	else
		record_fail "$desc" "$_VALIDATE_REASON"
	fi
}

# fail_why <desc> <why> — for a helper that refuses to go on. Under check, it
# appends "<desc>: <why>" to the FAIL line check records, so the failure is
# counted once and its reason isn't discarded; anywhere else it records the
# failure itself. It reaches check through check's locals, so call it from
# the shell check runs in, not from a subshell.
fail_why() {
	if [ -n "${_VALIDATE_IN_CHECK:-}" ]; then
		_VALIDATE_REASON="${_VALIDATE_REASON:+$_VALIDATE_REASON; }$1: $2"
	else
		record_fail "$1" "$2"
	fi
}

# json_matches <json> <jq-filter> — true when <json> holds a value and the
# filter is truthy on it. A blank body never matches: jq -e exits 0 on empty
# or whitespace-only input (there's no value, so nothing was false), and
# curl -s against a service that isn't up prints exactly that, so without
# this guard a down stack passed every JSON check (#302).
json_matches() {
	[[ $1 =~ [^[:space:]] ]] || return 1
	printf '%s\n' "$1" | jq -e "$2" >/dev/null 2>&1
}

# json_shown <json> — <json> for a failure message, or "<empty>" when it's
# blank, so an empty body reads as one rather than as a dangling "got: ".
json_shown() {
	if [[ $1 =~ [^[:space:]] ]]; then
		printf '%s' "$1"
	else
		printf '<empty>'
	fi
}

check_json() {
	local desc="$1" json="$2" filter="$3"
	if json_matches "$json" "$filter"; then
		record_pass "$desc"
	else
		record_fail "$desc" "got: $(json_shown "$json")"
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

# require_docker_credential_helper — keep a hung docker credential helper from
# hanging every pull and build (#300). In the devcontainer, credsStore names
# VS Code's dev-containers helper, which forwards each lookup to the attached
# VS Code client; with no client attached (a headless session, an
# issue-worker) it never answers, and `docker pull` / `compose up --build`
# fail with DeadlineExceeded, which reads as the thing under test being broken.
#
# It asks the helper what a pull from Docker Hub asks it, under a short
# timeout. If the helper answers at all, found or not, nothing changes. If it
# doesn't, it exports DOCKER_CONFIG as a scratch copy of the config dir minus
# credsStore (Docker then pulls anonymously, as it does with a client attached
# and nothing stored) and says so in one NOTE line. If it can't build that
# copy, that's a hard prerequisite naming the helper: a registry the script
# can't pull from is not a skip.
#
# The user's own config is never edited, so forwarding still works whenever a
# client is attached. The scratch dir holds symlinks to everything else in the
# config dir (cli-plugins, contexts, buildx state), so only credsStore
# changes. It's removed by a watcher that outlives the script by at most a
# second, not an EXIT trap, so it can't clobber the script's own trap; and it
# goes even if the script alone is killed (a kill of the whole process tree
# takes the watcher too, leaving a small dir in $TMPDIR). Only credsStore is
# probed: per-registry
# credHelpers are left as they are.
#
# require_compose calls it, so it runs before any compose script pulls or
# builds. Call it after validate_parse_args, in the shell that later runs
# docker. An attached client answers in about a second; the probe's timeout,
# in seconds, is VALIDATE_CREDENTIAL_HELPER_TIMEOUT (the validate script for
# this sets 1 to stay fast).
VALIDATE_CREDENTIAL_HELPER_TIMEOUT="${VALIDATE_CREDENTIAL_HELPER_TIMEOUT:-5}"

require_docker_credential_helper() {
	local src="${DOCKER_CONFIG:-$HOME/.docker}" cfg store helper rc dir e why
	cfg="$src/config.json"
	[ -f "$cfg" ] || return 0
	# Without jq there's nothing to read credsStore with; require_cmd jq
	# reports that already.
	command -v jq >/dev/null 2>&1 || return 0
	store=$(jq -r '.credsStore // empty' "$cfg" 2>/dev/null) || return 0
	[ -n "$store" ] || return 0
	helper="docker-credential-$store"
	if ! command -v timeout >/dev/null 2>&1; then
		_prereq_missing "required command not found: timeout (to probe $helper, credsStore in $cfg)"
		return
	fi
	# A helper that isn't installed fails at once, and Docker reports that
	# itself; only one that doesn't answer is bypassed. The answer goes to
	# /dev/null: this asks only whether the helper responds. timeout signals
	# the whole process group, so a shim's grandchild (the dev-containers
	# helper's node) goes too.
	printf '%s\n' https://index.docker.io/v1/ |
		timeout -k 1 "$VALIDATE_CREDENTIAL_HELPER_TIMEOUT" "$helper" get >/dev/null 2>&1
	rc=$?
	[ "$rc" -eq 124 ] || [ "$rc" -eq 137 ] || return 0

	why=
	if ! dir=$(mktemp -d "${TMPDIR:-/tmp}/validate-docker-config.XXXXXX" 2>&1); then
		why="mktemp: $dir"
	else
		for e in "$src"/* "$src"/.[!.]*; do
			[ -e "$e" ] || [ -L "$e" ] || continue
			[ "$(basename "$e")" = config.json ] && continue
			ln -s "$e" "$dir/" 2>/dev/null || {
				why="could not link $e into $dir"
				break
			}
		done
		if [ -z "$why" ] &&
			! (umask 077 && jq 'del(.credsStore)' "$cfg" >"$dir/config.json") 2>/dev/null; then
			why="could not write $dir/config.json from $cfg"
		fi
		if [ -n "$why" ]; then
			rm -rf "$dir"
		fi
	fi
	if [ -n "$why" ]; then
		_prereq_missing "docker credential helper $helper (credsStore in $cfg) did not answer within ${VALIDATE_CREDENTIAL_HELPER_TIMEOUT}s, and a scratch DOCKER_CONFIG without it could not be built: $why — attach a VS Code client, or export DOCKER_CONFIG naming a config without credsStore"
		return
	fi
	# $$ is the script's PID even here in the subshell.
	(
		while kill -0 "$$" 2>/dev/null; do sleep 1; done
		rm -rf "$dir"
	) </dev/null >/dev/null 2>&1 &
	disown 2>/dev/null || true
	export DOCKER_CONFIG="$dir"
	echo "NOTE: docker credential helper $helper did not answer within ${VALIDATE_CREDENTIAL_HELPER_TIMEOUT}s (no VS Code client attached?) — bypassing it with a scratch DOCKER_CONFIG without credsStore: $dir"
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

# require_env_file <VARNAME> [hint] — the variable must be set AND name a real
# file. The hint, if given, is appended to the diagnostic (what to run to fix it).
require_env_file() {
	local name="$1" value="${!1:-}" hint="${2:+ — $2}"
	if [ -z "$value" ]; then
		_prereq_missing "$name not set$hint"
	elif [ ! -f "$value" ]; then
		_prereq_missing "$name=$value not found$hint"
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

# default_incusos_base_image — when INCUSOS_BASE_IMAGE is unset, point it at
# the image `make incusos-base` cached for THIS checkout's pinned version
# (scripts/incusos-base.version), if it is there, and say so in one NOTE (#296).
# Call it once, after validate_parse_args, in any script that reads the
# variable; then gate on have_env_file / require_env_file as before.
#
# Pinned version only. After a pin bump, or when only some other version was
# fetched with INCUSOS_VERSION, the variable stays unset and the checks skip
# until `make incusos-base` has run: two machines on one commit must not end up
# testing different IncusOS builds without anyone asking for it (the drift
# docs/Decisions.md §21 was written against). Any other image takes an explicit
# `export INCUSOS_BASE_IMAGE=...`, which this never overrides.
#
# It never fetches (a ~610 MB download has no business hiding in a test run)
# and never re-hashes 3.2 GB per run; the sidecar check is the fetch script's.
# That script also owns where the cache is, so the layout lives in one place.
default_incusos_base_image() {
	[ -z "${INCUSOS_BASE_IMAGE:-}" ] || return 0
	local img version
	# INCUSOS_VERSION is the fetch script's override, not the harness's.
	img=$(env -u INCUSOS_VERSION "$(dirname "${BASH_SOURCE[0]}")/../fetch-incusos-base.sh" --path 2>/dev/null) || return 0
	# The fetch script moves image and sidecar into place together, so an image
	# without its sidecar is not something it produced.
	[ -f "$img" ] && [ -f "$img.sha256" ] || return 0
	version=$(basename "$(dirname "$img")")
	export INCUSOS_BASE_IMAGE="$img"
	echo "NOTE: INCUSOS_BASE_IMAGE unset — using the cached pinned IncusOS $version: $img"
}

# base_image_skip_reason — the reason every [base-image] skip carries, so each
# one names the command that makes it run.
base_image_skip_reason() {
	if [ -n "${INCUSOS_BASE_IMAGE:-}" ]; then
		echo "INCUSOS_BASE_IMAGE=$INCUSOS_BASE_IMAGE not found — fix the path, or unset it and run 'make incusos-base'"
	else
		echo "INCUSOS_BASE_IMAGE unset and the pinned IncusOS image is not cached — run 'make incusos-base'"
	fi
}

# ---------------------------------------------------------------------------
# The agent image, built from this tree
# ---------------------------------------------------------------------------
#
# CI publishes ghcr.io/ehharvey/homelab-ops/agent:latest from main only, so a
# script that deploys :latest proves main's agent, not the branch under test.
# #100 hit exactly that: its agent waits for incus-socket's late-created
# socket, main's didn't, and both VM scripts failed on main's agent. These
# helpers put the tree's agent into a node's own image store instead, and the
# App names it by a local alias (no image.server), which Incus resolves there.

# AGENT_IMAGE_ALIAS — the alias the helpers below import the image under.
AGENT_IMAGE_ALIAS="homelab-ops-agent-validate"

# build_agent_incus_image <root_dir> <out_dir> — builds Dockerfile.agent and
# repacks it as the split image Incus's own OCI pull uploads (the incus
# client's oci_images.go): <out_dir>/agent-meta.tar.gz holding metadata.yaml
# and a runtime-spec config.json, and <out_dir>/agent-rootfs.tar.gz. Incus
# reads only the spec's process args, env, cwd and user; the image property
# type=oci, which import_agent_incus_image sets, is what makes it start an
# application container (oci.entrypoint, volatile.container.oci).
build_agent_incus_image() {
	build_incus_oci_image "$1/Dockerfile.agent" "$1" "$2" agent
}

# build_incus_oci_image <dockerfile> <context> <out_dir> <name> — as
# build_agent_incus_image, for any Dockerfile: <out_dir>/<name>-meta.tar.gz
# and <out_dir>/<name>-rootfs.tar.gz. Import them with
# `incus image import <meta> <rootfs>` and mark the image type=oci.
build_incus_oci_image() {
	local dockerfile="$1" context="$2" out="$3" name="$4" tag="homelab-ops-$4:validate-$$" cid arch
	mkdir -p "$out/$name-meta" || return 1
	if ! docker build -q -f "$dockerfile" -t "$tag" "$context" >/dev/null 2>&1; then
		fail_why "docker build" "$dockerfile failed to build"
		return 1
	fi
	# The image's USER isn't mapped: distroless/static runs as root, and a
	# change there should fail here rather than run as the wrong user.
	if ! docker image inspect "$tag" | jq -e '.[0].Config.User // "" | . == "" or . == "0" or . == "root"' >/dev/null; then
		fail_why "config.json" "image sets a non-root USER; map it into process.user"
		return 1
	fi
	docker image inspect "$tag" | jq '.[0].Config | {
		ociVersion: "1.0.2",
		process: {
			args: ((.Entrypoint // []) + (.Cmd // [])),
			env: (.Env // []),
			cwd: (if (.WorkingDir // "") == "" then "/" else .WorkingDir end),
			user: {uid: 0, gid: 0}
		},
		root: {path: "rootfs"}
	}' >"$out/$name-meta/config.json" || return 1
	case "$(docker image inspect -f '{{.Architecture}}' "$tag")" in
	amd64) arch=x86_64 ;;
	arm64) arch=aarch64 ;;
	*) arch="$(docker image inspect -f '{{.Architecture}}' "$tag")" ;;
	esac
	printf '{"architecture":"%s","creation_date":%s}\n' "$arch" "$(date +%s)" >"$out/$name-meta/metadata.yaml"
	tar -czf "$out/$name-meta.tar.gz" -C "$out/$name-meta" config.json metadata.yaml || return 1
	cid=$(docker create "$tag") || return 1
	docker export "$cid" | gzip >"$out/$name-rootfs.tar.gz"
	local rc=$?
	docker rm "$cid" >/dev/null 2>&1
	docker image rm "$tag" >/dev/null 2>&1
	return "$rc"
}

# import_agent_incus_image <remote> <project> <probe> <node_ip> <dir> —
# pushes build_agent_incus_image's tarballs into <probe>, an Alpine instance
# on the node's LAN holding the break-glass cert at /root/client.{crt,key},
# and from there imports them into the node as $AGENT_IMAGE_ALIAS, marked
# type=oci. It goes through the incus CLI rather than a curl multipart POST
# to /1.0/images: against a real Incus the latter's import never landed, with
# no error the operation kept long enough to read (#100).
import_agent_incus_image() {
	local remote="$1" project="$2" probe="$3" ip="$4" dir="$5" f out
	for f in agent-meta.tar.gz agent-rootfs.tar.gz; do
		if ! incus file push --project "$project" "$dir/$f" "$remote:$probe/root/$f" >/dev/null 2>&1; then
			fail_why "push" "$f into $probe"
			return 1
		fi
	done
	if ! out=$(incus exec --project "$project" "$remote:$probe" -- sh -c "
		set -e
		command -v incus >/dev/null || apk add --no-cache incus-client >/dev/null
		mkdir -p /root/.config/incus
		cp /root/client.crt /root/client.key /root/.config/incus/
		incus remote list -f csv | grep -q '^agent-import' ||
			incus remote add agent-import https://$ip:8443 --accept-certificate >/dev/null
		incus image import /root/agent-meta.tar.gz /root/agent-rootfs.tar.gz agent-import: --alias $AGENT_IMAGE_ALIAS >/dev/null
		incus image set-property agent-import:$AGENT_IMAGE_ALIAS type=oci
	" 2>&1); then
		fail_why "image import" "$out"
		return 1
	fi
}

# check_agent_registered <remote> <project> <probe> <node_ip> <instance> —
# records whether the node's agent <instance> runs with the incus-socket
# profile and the app tag deploy-agent set, and has published its
# user.homelab-ops.agent.* registry keys. Running alone would pass with a
# container whose agent never reached Incus; those keys can only come from
# the agent itself, through incus-socket, after finding its own instance by
# that tag. Read with the break-glass cert over the LAN from <probe>, as for
# import_agent_incus_image, polling up to two minutes for the keys. On a
# failure it attaches the agent's console tail, which is what says why an
# agent stopped or never reached Incus; the instance JSON alone cannot.
check_agent_registered() {
	local remote="$1" project="$2" probe="$3" ip="$4" instance="$5" json="" console
	local name="$instance runs with incus-socket and publishes its registry keys through it"
	for _ in $(seq 1 24); do
		json=$(incus exec --project "$project" "$remote:$probe" -- \
			curl --cert /root/client.crt --key /root/client.key -k -s "https://$ip:8443/1.0/instances/$instance" 2>/dev/null)
		if jq -e '[.metadata.config // {} | keys[] | select(startswith("user.homelab-ops.agent."))] | length > 0' <<<"$json" >/dev/null 2>&1; then
			break
		fi
		sleep 5
	done
	if jq -e '.metadata.status == "Running" and (.metadata.profiles | index("incus-socket")) != null and .metadata.config["user.homelab-ops.app"] == "agent" and ([.metadata.config | keys[] | select(startswith("user.homelab-ops.agent."))] | length > 0)' <<<"$json" >/dev/null 2>&1; then
		record_pass "$name"
		return
	fi
	console=$(incus exec --project "$project" "$remote:$probe" -- \
		curl --cert /root/client.crt --key /root/client.key -k -s "https://$ip:8443/1.0/instances/$instance/console" 2>/dev/null | tail -20)
	record_fail "$name" \
		"instance: $(jq -c '{status: .metadata.status, profiles: .metadata.profiles, config: .metadata.config}' <<<"$json" 2>/dev/null || echo "$json"); console tail: $(tr '\n' '|' <<<"$console")"
}

# ---------------------------------------------------------------------------
# Git fixtures, for scripts that drive git in throwaway repos. Callers also
# export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1, so the operator's
# own config (aliases, signing, hooks) can't change what the repos do.
# ---------------------------------------------------------------------------

# git_fixture_config <repo>: an identity, and no signing (CI has no key).
git_fixture_config() {
	git -C "$1" config user.name validate
	git -C "$1" config user.email validate@example.invalid
	git -C "$1" config commit.gpgsign false
}

# git_fixture_repos <origin> <work>: a bare origin on main, and a configured
# work repo on main with it as "origin". Nothing is committed or pushed.
git_fixture_repos() {
	git init -q --bare -b main "$1"
	git init -q -b main "$2"
	git_fixture_config "$2"
	git -C "$2" remote add origin "$1"
}

# commit_file <repo> <file> <content> [message]
commit_file() {
	echo "$3" >"$1/$2"
	git -C "$1" add "$2"
	git -C "$1" commit -q -m "${4:-touch $2}"
}

# pre_push_under_test: the path of the pre-push hook that checks a push from
# the current checkout. git starts the copy core.hooksPath resolves to; when
# that copy sources .githooks/run-own-copy.sh (#279) on an uncommented line,
# and the helper sits beside it, it hands off to this checkout's own
# executable copy, which is then the one under test.
pre_push_under_test() {
	local started own
	started=$(git rev-parse --path-format=absolute --git-path hooks/pre-push) || return
	own="$(git rev-parse --show-toplevel)/.githooks/pre-push"
	if [ -x "$own" ] && [ -f "${started%/*}/run-own-copy.sh" ] &&
		grep -v '^[[:space:]]*#' "$started" 2>/dev/null | grep -q '\. .*run-own-copy\.sh'; then
		echo "$own"
	else
		echo "$started"
	fi
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
