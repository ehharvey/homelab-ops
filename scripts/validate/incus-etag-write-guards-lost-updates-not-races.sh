#! /bin/bash
# Validates GH issue #160's spike: what Incus's ETag/If-Match conditional write
# actually guarantees, so the leader-election design doesn't lean on more.
#
# The original lease design (docs/Decisions.md §17) claimed "only one write ever
# lands per contested tick — Incus's own conflict rejection is the entire
# concurrency mechanism." A fake client can't test that (it would only encode
# the assumption), so this drives the real API over the real unix socket, the
# way internal/incuslocal will.
#
# Result, recorded in docs/Decisions.md §25: If-Match is a LOST-UPDATE GUARD, not
# a compare-and-swap. It does what the Incus docs promise — a stale ETag is
# rejected — but the check and the write are not atomic, so N writers holding
# one ETag can each land. Leader election therefore does not rest on it.
#
# ASSERTED (true, and worth guarding against regression):
#   1. GET returns an ETag.
#   2. A conditional PATCH/PUT on a matching ETag lands and moves the ETag —
#      including when only one user.* value changed.
#   3. A stale ETag is rejected with 412 and changes nothing.
#   4. Under N concurrent writers on one ETag, at least one lands every round.
#
# REPORTED, NOT ASSERTED (the finding — a change here is news, not a failure):
#   5. How many writers landed per round: >1 in a minority of rounds on an
#      instance, and most writers on a project.
#   6. Creating one unique name concurrently, for a profile: exactly one
#      succeeds. A database uniqueness constraint arbitrates, unlike the ETag
#      check. Not relied on today; it is the fallback lease primitive.
#
# Runs where the Incus unix socket is readable — the Incus host, not the
# devcontainer (which reaches the host over TLS as a remote). Elsewhere it
# skips, tag [incus-socket]. Override with VALIDATE_INCUS_SOCKET.
#
# Caveat: a non-clustered host proves the mechanism, not the clustered path 0.x
# runs (a one-member cluster). Clustering is printed so a run can't be mistaken
# for one.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="Incus's If-Match rejects stale writes but is not a compare-and-swap, so leader election must not rest on it (#160, #108)"
VALIDATE_GROUP="incus"
VALIDATE_NEEDS="read/write access to the Incus unix socket, curl, jq"
VALIDATE_DURATION="~10s"

validate_parse_args "$@"

SOCKET="${VALIDATE_INCUS_SOCKET:-/var/lib/incus/unix.socket}"
PROJECT="${VALIDATE_INCUS_PROJECT:-default}"
WRITERS=16 # concurrent writers per round
ROUNDS=10

require_cmd curl jq
check_prereqs

if [ ! -S "$SOCKET" ] || [ ! -r "$SOCKET" ] || [ ! -w "$SOCKET" ]; then
	skip_check "Incus conditional write is a CAS" incus-socket \
		"$SOCKET is not a readable/writable socket (run on the Incus host, or set VALIDATE_INCUS_SOCKET)"
	summary
fi

INST="validate-etag-inst-$$"
PROBE_PROJECT="validate-etag-proj-$$"
TMP="$(mktemp -d)"

cleanup() {
	api DELETE "/1.0/instances/$INST?project=$PROJECT" >/dev/null 2>&1
	api DELETE "/1.0/projects/$PROBE_PROJECT" >/dev/null 2>&1
	rm -rf "$TMP"
}
trap cleanup EXIT

# api <METHOD> <path> [curl args...] — body on stdout, headers discarded.
api() {
	local method="$1" path="$2"
	shift 2
	curl -s --unix-socket "$SOCKET" -X "$method" "$@" "http://unix$path"
}

# etag_of <path> — the ETag header exactly as sent, quotes and all: If-Match
# wants it back verbatim.
etag_of() {
	curl -si --unix-socket "$SOCKET" "http://unix$1" | tr -d '\r' |
		awk -F': ' 'tolower($1)=="etag"{print $2}'
}

# code_of <METHOD> <path> <etag> <body> — the HTTP status only.
code_of() {
	curl -s -o /dev/null -w '%{http_code}' --unix-socket "$SOCKET" -X "$1" \
		-H "If-Match: $3" -d "$4" "http://unix$2"
}

# wait_op <api response> — if the response is an async operation, wait for it.
wait_op() {
	local op
	op="$(echo "$1" | jq -r 'select(.type=="async") | .operation // empty')"
	[ -z "$op" ] || api GET "$op/wait?timeout=30" >/dev/null
}

read_key() { # <path> <key>
	api GET "$1" | jq -r --arg k "$2" '.metadata.config[$k] // empty'
}

# race <path> <key> — WRITERS concurrent PATCHes on one ETag, ROUNDS times.
# Sets RACE_WINNERS to the space-separated winner count per round, and
# RACE_LOST_UPDATES to the number of rounds whose surviving value wasn't the
# (sole) winner's. Globals, not stdout: a $(...) call would lose the counter to
# a subshell.
race() {
	local path="$1" key="$2" round i e winners=() wcount winner got
	RACE_LOST_UPDATES=0
	RACE_WINNERS=""
	for round in $(seq "$ROUNDS"); do
		e="$(etag_of "$path")"
		rm -f "$TMP"/code.*
		for i in $(seq "$WRITERS"); do
			code_of PATCH "$path" "$e" "{\"config\":{\"$key\":\"w$i\"}}" >"$TMP/code.$i" &
		done
		wait
		wcount=0
		winner=""
		for i in $(seq "$WRITERS"); do
			if [ "$(cat "$TMP/code.$i")" = 200 ]; then
				wcount=$((wcount + 1))
				winner="w$i"
			fi
		done
		winners+=("$wcount")
		got="$(read_key "$path" "$key")"
		if [ "$wcount" = 1 ] && [ "$got" != "$winner" ]; then
			RACE_LOST_UPDATES=$((RACE_LOST_UPDATES + 1))
		fi
	done
	RACE_WINNERS="${winners[*]}"
}

echo "== 0. Target =="
info="$(api GET /1.0)"
echo "   server $(echo "$info" | jq -r '.metadata.environment.server_version'), clustered=$(echo "$info" | jq -r '.metadata.environment.server_clustered'), socket $SOCKET"

echo
echo "== 1. Lease object: a never-started instance =="
created="$(api POST "/1.0/instances?project=$PROJECT" \
	-d "{\"name\":\"$INST\",\"source\":{\"type\":\"none\"}}")"
wait_op "$created"
check "created never-started instance $INST" bash -c "curl -sf --unix-socket '$SOCKET' 'http://unix/1.0/instances/$INST?project=$PROJECT' >/dev/null"

P="/1.0/instances/$INST?project=$PROJECT"
KEY="user.homelab-ops.lease.owner"

E0="$(etag_of "$P")"
check "GET returns an ETag" test -n "$E0"

echo
echo "== 2. Matching ETag lands; the ETag moves =="
code="$(code_of PATCH "$P" "$E0" "{\"config\":{\"$KEY\":\"a\"}}")"
check_eq "PATCH with matching If-Match returns 200" 200 "$code"
check_eq "the write landed" a "$(read_key "$P" "$KEY")"
E1="$(etag_of "$P")"
if [ -n "$E0" ] && [ "$E0" != "$E1" ]; then
	record_pass "ETag changed after a change to one user.* value (a renewal is visible to the next CAS)"
else
	record_fail "ETag changed after a change to one user.* value" "before $E0, after $E1"
fi

# PUT too: #108 leaves PUT-vs-PATCH open. PUT replaces the writable fields, so
# build it from a fresh GET.
put_body() { # <value>
	api GET "$P" | jq -c --arg k "$KEY" --arg v "$1" \
		'.metadata | {architecture, config: (.config + {($k): $v}), devices, ephemeral, profiles, stateful, description}'
}
E1="$(etag_of "$P")"
body="$(put_body b)"
resp="$(curl -s --unix-socket "$SOCKET" -X PUT -H "If-Match: $E1" -d "$body" "http://unix$P")"
wait_op "$resp"
check_eq "PUT with matching If-Match lands" b "$(read_key "$P" "$KEY")"

echo
echo "== 3. Stale ETag is rejected and changes nothing =="
code="$(code_of PATCH "$P" "$E0" "{\"config\":{\"$KEY\":\"stale\"}}")"
check_eq "PATCH with a stale If-Match returns 412" 412 "$code"
code="$(code_of PUT "$P" "$E0" "$(put_body stale)")"
check_eq "PUT with a stale If-Match returns 412" 412 "$code"
check_eq "the stale writes changed nothing" b "$(read_key "$P" "$KEY")"

echo
echo "== 4. $WRITERS concurrent writers, one shared ETag, $ROUNDS rounds =="
race "$P" "$KEY"
echo "   winners per round: $RACE_WINNERS"
min="$(echo "$RACE_WINNERS" | tr ' ' '\n' | sort -n | head -1)"
if [ "$min" -ge 1 ] 2>/dev/null; then
	record_pass "at least one writer lands in every round"
else
	record_fail "at least one writer lands in every round" "winners per round: $RACE_WINNERS"
fi
multi="$(echo "$RACE_WINNERS" | tr ' ' '\n' | awk '$1>1' | wc -l)"
echo "   (informational) $multi of $ROUNDS rounds admitted more than one writer:"
echo "   the ETag check and the write are not atomic, so If-Match is not a CAS"

echo
echo "== 5. Informational: the same race against a project =="
api POST /1.0/projects -d "{\"name\":\"$PROBE_PROJECT\"}" >/dev/null
race "/1.0/projects/$PROBE_PROJECT" "user.race"
echo "   winners per round: $RACE_WINNERS"

echo
echo "== 6. Informational: concurrent create of one profile name =="
uniq_rounds=0
for round in $(seq "$ROUNDS"); do
	name="validate-etag-prof-$$-$round"
	rm -f "$TMP"/code.*
	for i in $(seq "$WRITERS"); do
		curl -s -o /dev/null -w '%{http_code}' --unix-socket "$SOCKET" -X POST \
			-d "{\"name\":\"$name\"}" "http://unix/1.0/profiles" >"$TMP/code.$i" &
	done
	wait
	created="$(cat "$TMP"/code.* | tr ' ' '\n' | grep -o '201' | wc -l)"
	[ "$created" = 1 ] && uniq_rounds=$((uniq_rounds + 1))
	api DELETE "/1.0/profiles/$name" >/dev/null 2>&1
done
echo "   exactly one create succeeded in $uniq_rounds of $ROUNDS rounds"

summary
