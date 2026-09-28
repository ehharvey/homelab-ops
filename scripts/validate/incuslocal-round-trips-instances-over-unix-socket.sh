#! /bin/bash
# Validates GH issue #160: internal/incuslocal, the unix-socket Incus client
# the app-manager agent's reconciler (#98) and its leaderelection.Registry
# (#101) both talk to Incus through.
#
# Unit tests cover the package against a fake served over a real unix socket,
# which proves the wire handling but not the wire. This proves the wire: every
# assertion below drives the actual package against an actual Incus daemon.
# #160's done-when is "incuslocal.Dial connects to a real Incus unix socket",
# and a fake cannot answer that.
#
# The split matters. Fixtures are created and verified with curl and the incus
# CLI; only the operation under test goes through
# cmd/validate-incuslocal-harness. So the package is never both the actor and
# the oracle — a broken CreateInstance fails its own check rather than
# silently making every other check meaningless.
#
# Runs wherever an Incus unix socket is readable/writable; elsewhere every
# check skips, tag [incus-socket]. Two ways to get one:
#
#   - On the Incus host itself, against /var/lib/incus/unix.socket.
#   - From the devcontainer, which only reaches the host over TLS, via the
#     `incus-bridge` compose service: it bridges the host's TLS endpoint to a
#     local socket with verification kept on. See its comment in
#     docker-compose.yml, then:
#
#       VALIDATE_INCUS_SOCKET=/tmp/incus-bridge/incus.sock ./scripts/validate/<this>
#
# (The devcontainer's own socket is not the host's — it belongs to a separate,
# uninitialized incusd — so do not point this at it expecting the host.)
#
# internal/incuslocal targets Incus's DEFAULT project by design (see its
# package doc: 0.x puts the agent's instances there, so a project parameter
# would be a knob with one value). This script therefore has no project knob
# either — its fixtures go in the default project, because that is the only
# place the client would look.
#
# Every check needs a daemon that can actually create an instance; a
# prerequisite probe below asks it directly and skips with the daemon's own
# reason if not, tag [incus-instances]. The devcontainer's own incusd fails
# that probe (no storage pool, no idmap), which is another reason to reach the
# real host — directly, or through the bridge above.
#
# The three ReadFile checks need a container image, because they need a real
# filesystem to read from — a stopped container is enough, since Incus serves
# the files API off the storage volume either way. Everything else uses source
# type "none" instances, which are instant and need nothing cached. With no
# image the three skip together, tag [ct-image], and the other eight still run.
#
# Clustering is printed, not asserted: #178 made a seeded node a one-member
# cluster, and one socket is supposed to reach every member. Printing it keeps
# a run on a non-clustered dev host from being mistaken for the clustered path
# 0.x actually runs.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="internal/incuslocal round-trips instances, user.* config and file reads over a real Incus unix socket (#160)"
VALIDATE_GROUP="incus"
VALIDATE_NEEDS="read/write access to the Incus unix socket, an Incus that can create instances, go, incus, curl, jq, [a container image]"
VALIDATE_DURATION="~30s"

validate_parse_args "$@"

SOCKET="${VALIDATE_INCUS_SOCKET:-/var/lib/incus/unix.socket}"
CT_IMAGE="${VALIDATE_ALPINE_CT:-validate-alpine}"

WORK_DIR="$(mktemp -d)"
HARNESS_BIN="$WORK_DIR/harness"

# A unique tag value per run, so assertions can pick this run's fixtures out
# of a listing that may legitimately also contain real agent instances.
APP="validate-incuslocal-$$"
TAGGED="validate-incuslocal-tagged-$$"
UNTAGGED="validate-incuslocal-untagged-$$"
CREATED="validate-incuslocal-created-$$"
DOOMED="validate-incuslocal-doomed-$$"
FSINST="validate-incuslocal-fs-$$"
HEARTBEAT_PATH="/run/heartbeat"

# Every check this script records, in order and worded identically to its
# assertion. skip_check and record_pass/record_fail feed the same tally, so a
# skip label with no corresponding check would describe a run that never
# happened, and a check with no skip entry would silently shrink the total.
_core_checks=(
	"harness builds"
	"ListInstances returns a tagged instance with its status"
	"ListInstances excludes an instance carrying no app tag"
	"SetUserKeys writes a user.* key that the next ListInstances sees"
	"SetUserKeys refuses a key outside the user.* namespace"
	"CreateInstance creates an instance the incus CLI can then see"
	"CreateInstance reports an operation Incus accepted and then failed (#161)"
	"DeleteInstance removes an instance the incus CLI then cannot see"
)
_readfile_checks=(
	"ReadFile returns a file pushed into an instance"
	"ReadFile reports a missing file as an error, not empty content"
	"ReadFile reports a directory as an error"
)

api() {
	local method="$1" path="$2"
	shift 2
	curl -s --unix-socket "$SOCKET" -X "$method" "$@" "http://unix$path"
}

# await <response-json> — succeeds on a sync response, or on an async one
# whose operation finishes successfully. Incus answers a create with
# "Operation created" before the work has happened, so not waiting here would
# let a fixture look established when it failed (the shape of #161).
await() {
	local op
	op="$(jq -r 'select(.type=="async") | .operation // empty' <<<"$1" 2>/dev/null)"
	[ -z "$op" ] && return 0
	curl -s --unix-socket "$SOCKET" "http://unix${op}/wait" |
		jq -e '.metadata.status_code == 200' >/dev/null 2>&1
}

# create_fixture <name> [app-tag-value] — a source type "none" instance, via
# curl rather than the harness, so fixture setup never depends on the code
# under test.
create_fixture() {
	local name="$1" tag="${2:-}" body
	if [ -n "$tag" ]; then
		body="$(jq -nc --arg n "$name" --arg t "$tag" \
			'{name: $n, source: {type: "none"}, config: {"user.homelab-ops.app": $t}}')"
	else
		body="$(jq -nc --arg n "$name" '{name: $n, source: {type: "none"}}')"
	fi
	await "$(api POST /1.0/instances -H 'Content-Type: application/json' -d "$body")"
}

# create_fixture_error <name> — creates a source-type-none instance and prints
# the daemon's own failure text, or nothing on success. Used by the prerequisite
# probe, which needs the reason rather than just the outcome.
create_fixture_error() {
	local name="$1" body resp op
	body="$(jq -nc --arg n "$name" '{name: $n, source: {type: "none"}}')"
	resp="$(api POST /1.0/instances -H 'Content-Type: application/json' -d "$body")"
	if [ "$(jq -r '.type // empty' <<<"$resp" 2>/dev/null)" = "error" ]; then
		jq -r '.error' <<<"$resp"
		return
	fi
	op="$(jq -r 'select(.type=="async") | .operation // empty' <<<"$resp" 2>/dev/null)"
	[ -z "$op" ] && return
	curl -s --unix-socket "$SOCKET" "http://unix${op}/wait" |
		jq -r 'select(.metadata.status_code != 200) | .metadata.err // "unknown failure"'
}

instance_exists() {
	api GET "/1.0/instances/$1" | jq -e '.type == "sync"' >/dev/null 2>&1
}

# harness_list_entry <instance-name> — this run's listing entry for one
# instance, as compact JSON, or empty if the harness did not return it.
harness_list_entry() {
	"$HARNESS_BIN" -mode=list -socket="$SOCKET" 2>/dev/null |
		jq -c --arg n "$1" '.[] | select(.Name == $n)' 2>/dev/null
}

cleanup() {
	local name
	for name in "$TAGGED" "$UNTAGGED" "$CREATED" "$DOOMED" "$FSINST"; do
		incus delete --force "$name" >/dev/null 2>&1
		# Belt and braces: a source-type-none instance the CLI cannot see
		# (wrong default remote, say) still goes through the socket.
		api DELETE "/1.0/instances/$name" >/dev/null 2>&1
	done
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

echo "== 0. Prerequisites =="
require_cmd go incus curl jq
check_prereqs

if [ ! -S "$SOCKET" ] || [ ! -r "$SOCKET" ] || [ ! -w "$SOCKET" ]; then
	for _desc in "${_core_checks[@]}" "${_readfile_checks[@]}"; do
		skip_check "$_desc" incus-socket \
			"$SOCKET is not a readable/writable socket (run on the Incus host, or set VALIDATE_INCUS_SOCKET)"
	done
	summary
fi

info="$(api GET /1.0)"
# Not `// "?"`: jq's // yields its right side for false as well as null, so
# an honestly-unclustered daemon would print as "?" and hide the thing this
# line exists to disclose.
echo "   server $(jq -r '.metadata.environment.server_version // "?"' <<<"$info"), clustered=$(jq -r '.metadata.environment.server_clustered | if . == null then "?" else tostring end' <<<"$info"), socket $SOCKET"

# A soft prerequisite, found the hard way, and deliberately a *probe* rather
# than a checklist of causes. Two different daemon-level impediments turned up
# while writing this, both of which made every check below go red for reasons
# that say nothing about incuslocal: no storage pool ("Failed getting root
# disk: No root device could be found"), and a devcontainer with no subuid
# mapping ("System doesn't have a functional idmap setup"). Rather than
# enumerate those, ask the daemon the only question that matters — can it
# create an instance? — and report its own words when it cannot. That is the
# #136 lesson: an unmet prerequisite skips, and says why.
PROBE="validate-incuslocal-probe-$$"
probe_err="$(create_fixture_error "$PROBE")"
api DELETE "/1.0/instances/$PROBE" >/dev/null 2>&1
if [ -n "$probe_err" ]; then
	for _desc in "${_core_checks[@]}" "${_readfile_checks[@]}"; do
		skip_check "$_desc" incus-instances \
			"the Incus at $SOCKET cannot create an instance: $probe_err"
	done
	summary
fi

echo
echo "== 1. Build the harness =="
check "harness builds" go -C "$ROOT_DIR" build -o "$HARNESS_BIN" ./cmd/validate-incuslocal-harness
if [ ! -x "$HARNESS_BIN" ]; then
	echo "ERROR: the harness didn't build; nothing downstream can be meaningful" >&2
	for _desc in "${_core_checks[@]:1}" "${_readfile_checks[@]}"; do
		record_fail "$_desc" "harness did not build"
	done
	summary
fi

echo
echo "== 2. ListInstances: what it includes, and what it must not =="
create_fixture "$TAGGED" "$APP"
create_fixture "$UNTAGGED"

entry="$(harness_list_entry "$TAGGED")"
# Status is asserted, not just presence: leaderelection.Registry.Peers has to
# exclude non-running instances and cannot if status never survives the
# projection. A source-type-none instance is "Stopped".
if [ -n "$entry" ] && jq -e --arg a "$APP" \
	'.Status == "Stopped" and .Config["user.homelab-ops.app"] == $a' <<<"$entry" >/dev/null 2>&1; then
	record_pass "ListInstances returns a tagged instance with its status"
else
	record_fail "ListInstances returns a tagged instance with its status" \
		"entry: ${entry:-<$TAGGED absent from the listing>}"
fi

# The untagged fixture exists and is visible to Incus, so its absence here is
# the filter working rather than the instance missing.
if instance_exists "$UNTAGGED" && [ -z "$(harness_list_entry "$UNTAGGED")" ]; then
	record_pass "ListInstances excludes an instance carrying no app tag"
else
	record_fail "ListInstances excludes an instance carrying no app tag" \
		"$UNTAGGED exists=$(instance_exists "$UNTAGGED" && echo yes || echo no), listed=$(harness_list_entry "$UNTAGGED")"
fi

echo
echo "== 3. SetUserKeys: the unconditional user.* write =="
STAMP="2026-09-28T00:00:00Z"
if "$HARNESS_BIN" -mode=set-user-keys -socket="$SOCKET" -instance-name="$TAGGED" \
	-keys="user.homelab-ops.epoch=7,user.homelab-ops.healthy-since=$STAMP" >/dev/null 2>&1 &&
	jq -e --arg s "$STAMP" \
		'.Config["user.homelab-ops.epoch"] == "7" and .Config["user.homelab-ops.healthy-since"] == $s' \
		<<<"$(harness_list_entry "$TAGGED")" >/dev/null 2>&1; then
	record_pass "SetUserKeys writes a user.* key that the next ListInstances sees"
else
	record_fail "SetUserKeys writes a user.* key that the next ListInstances sees" \
		"entry after write: $(harness_list_entry "$TAGGED")"
fi

# The guardrail: this method exists to write agent metadata, not to become a
# general config writer able to change limits or devices.
if "$HARNESS_BIN" -mode=set-user-keys -socket="$SOCKET" -instance-name="$TAGGED" \
	-keys="limits.cpu=64" >/dev/null 2>&1; then
	record_fail "SetUserKeys refuses a key outside the user.* namespace" \
		"the harness accepted limits.cpu=64"
else
	record_pass "SetUserKeys refuses a key outside the user.* namespace"
fi

echo
echo "== 4. CreateInstance and DeleteInstance, including a create that fails late =="
created_ok=false
if "$HARNESS_BIN" -mode=create -socket="$SOCKET" -instance-name="$CREATED" -app-tag="$APP" >/dev/null 2>&1 &&
	instance_exists "$CREATED"; then
	created_ok=true
	record_pass "CreateInstance creates an instance the incus CLI can then see"
else
	record_fail "CreateInstance creates an instance the incus CLI can then see" \
		"exists=$(instance_exists "$CREATED" && echo yes || echo no)"
fi

# #161's lesson, asserted rather than assumed: Incus accepts the request and
# returns an operation, and the real error only surfaces on the wait. A client
# that treated "Operation created" as success would report this as a pass.
if ! "$created_ok"; then
	# Without a create that works, "a create that fails late" proves nothing —
	# it would pass off any early failure as the late one. A cascade, not a
	# missing prerequisite.
	skip_check "CreateInstance reports an operation Incus accepted and then failed (#161)" upstream \
		"a plain CreateInstance already failed, so a late failure is not distinguishable"
elif create_err="$("$HARNESS_BIN" -mode=create -socket="$SOCKET" -instance-name="$DOOMED" \
	-storage-pool="validate-incuslocal-nonexistent-pool" 2>&1)"; then
	record_fail "CreateInstance reports an operation Incus accepted and then failed (#161)" \
		"the harness reported success: $(tr '\n' '|' <<<"$create_err")"
elif grep -qi "storage pool" <<<"$create_err"; then
	record_pass "CreateInstance reports an operation Incus accepted and then failed (#161)"
else
	record_fail "CreateInstance reports an operation Incus accepted and then failed (#161)" \
		"failed, but not with Incus's own pool error: $(tr '\n' '|' <<<"$create_err")"
fi

if "$HARNESS_BIN" -mode=delete -socket="$SOCKET" -instance-name="$CREATED" >/dev/null 2>&1 &&
	! instance_exists "$CREATED"; then
	record_pass "DeleteInstance removes an instance the incus CLI then cannot see"
else
	record_fail "DeleteInstance removes an instance the incus CLI then cannot see" \
		"still exists=$(instance_exists "$CREATED" && echo yes || echo no)"
fi

echo
echo "== 5. ReadFile against a real instance filesystem =="
# A *stopped* container, deliberately. Incus serves
# GET /1.0/instances/{name}/files off the storage volume whether or not the
# instance is running (verified: http 200 on a stopped container), and the
# client path under test is byte-identical either way — so requiring a running
# instance would only add an environmental dependency without testing more
# code. In production the file is a running agent's heartbeat; here what
# matters is that the GET, the directory guard and the missing-file error all
# behave against a real filesystem over the real socket.
#
# Gated as a group, and that gate is load-bearing: the two negative checks
# assert that ReadFile *errors*, and an absent instance makes it error too, so
# ungated they would both pass in an environment that never produced an
# instance at all — a false pass of exactly the kind this suite exists to
# remove (#129).
readfile_ready=false
readfile_skip_reason=""
if ! incus image info "$CT_IMAGE" >/dev/null 2>&1; then
	readfile_skip_reason="container image '$CT_IMAGE' not available locally (set VALIDATE_ALPINE_CT)"
elif ! create_out="$(incus create "$CT_IMAGE" "$FSINST" 2>&1)"; then
	readfile_skip_reason="could not create a container from '$CT_IMAGE': $(tr '\n' '|' <<<"$create_out" | tail -c 200)"
elif ! push_out="$(printf '%s\n' "$STAMP" >"$WORK_DIR/heartbeat" && incus file push "$WORK_DIR/heartbeat" "$FSINST$HEARTBEAT_PATH" 2>&1)"; then
	readfile_skip_reason="created $FSINST but could not push $HEARTBEAT_PATH into it: $(tr '\n' '|' <<<"$push_out" | tail -c 200)"
else
	readfile_ready=true
fi

if ! "$readfile_ready"; then
	for _desc in "${_readfile_checks[@]}"; do
		skip_check "$_desc" ct-image "$readfile_skip_reason"
	done
	summary
fi

got="$("$HARNESS_BIN" -mode=read-file -socket="$SOCKET" -instance-name="$FSINST" -path="$HEARTBEAT_PATH" 2>&1)"
check_eq "ReadFile returns a file pushed into an instance" "$STAMP" "$got"

# A heartbeat check that read "absent" as "fine" would invert the thing it
# tests, so this asserts an error rather than empty output.
if missing_out="$("$HARNESS_BIN" -mode=read-file -socket="$SOCKET" -instance-name="$FSINST" -path=/run/definitely-absent 2>&1)"; then
	record_fail "ReadFile reports a missing file as an error, not empty content" \
		"exited 0, returning: $(tr '\n' '|' <<<"$missing_out")"
elif [ -n "$missing_out" ]; then
	record_pass "ReadFile reports a missing file as an error, not empty content"
else
	record_fail "ReadFile reports a missing file as an error, not empty content" \
		"failed but said nothing about why"
fi

# /etc rather than /run: guaranteed to exist in any container image, and a
# directory the listing-vs-content distinction is unambiguous for.
if dir_out="$("$HARNESS_BIN" -mode=read-file -socket="$SOCKET" -instance-name="$FSINST" -path=/etc 2>&1)"; then
	record_fail "ReadFile reports a directory as an error" \
		"exited 0, returning: $(tr '\n' '|' <<<"$dir_out")"
elif grep -qi "directory" <<<"$dir_out"; then
	record_pass "ReadFile reports a directory as an error"
else
	record_fail "ReadFile reports a directory as an error" \
		"failed, but not as a directory error: $(tr '\n' '|' <<<"$dir_out")"
fi

echo
summary
