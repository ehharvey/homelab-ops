#! /bin/bash
# Validates GH issue #664: the validate/hardware queue
# (scripts/hardware-queue/job.sh) runs trusted PR heads one at a time and
# reports each as a commit status, with GitHub's statuses as its only memory.
#
# Drives the real job.sh against stubs, never GitHub or an Incus server:
#   - a stub `gh` serving fixture PRs, collaborators and statuses, recording
#     what is posted and the token each call carried
#   - a stub `incus` holding fixture instances, volumes and networks, for the
#     sweep, recording every call
#   - a stub lock in place of lib-hardware-lock.sh hold (busy on demand)
#   - a stub suite: each fixture PR commit carries its own run.sh, which
#     records its environment and start/end times and behaves as that commit's
#     `behaviour` file says (pass, skip, fail, unmet prerequisites, sleep)
#
# It proves: which PRs are eligible (cross-repo, off-allowlist and non-main
# PRs never run or get a status; a Dependabot PR runs), the writer check,
# auto-merge priority, one run at a time, superseding by a push or a close,
# reruns of a stale pending and of an old error (not a fresh one, nor
# success/failure), the lock-busy path (no status), the token-expiry suffix
# and warning, the outcome-to-status mapping including a timeout and a failed
# sweep, that the sweep removes exactly the README's patterns, that the token
# stays out of the suite's environment and logs, and that the config refuses
# the github group, host-os on the allow-list, and a VM name the sweep would
# match. Last, scripts/hardware-queue/setup.sh, in a throwaway checkout,
# refuses a tracked or unignored .env, naming it, before contacting anything.
#
# What it can't prove: the real GitHub API, the real lock and Incus, and the
# real suite on the instance. Those are #664's real-instance checks.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="the validate/hardware queue runs eligible PR heads one at a time, auto-merge first, supersedes, reruns stale pending and old error, refuses a second writer, and maps outcomes to statuses without leaking its token (#664)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="git, jq, flock, setsid, timeout, make"
VALIDATE_DURATION="~2m"
VALIDATE_SUPPORTED_OS="any"

validate_parse_args "$@"

require_cmd git jq flock setsid timeout make
check_prereqs

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

JOB="$ROOT_DIR/scripts/hardware-queue/job.sh"
W="$(mktemp -d)"
# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
	local p
	for p in $(jobs -p); do kill "$p" 2>/dev/null; done
	rm -rf "$W"
}
trap cleanup EXIT

mkdir -p "$W/bin" "$W/gh" "$W/incus" "$W/lock" "$W/rec" "$W/etc" "$W/state"
TOKEN="fixture-token-$RANDOM$RANDOM$RANDOM"
printf '%s' "$TOKEN" >"$W/etc/token"
REPO=o/r

# --- the stubs ------------------------------------------------------------------

cat >"$W/bin/gh" <<'EOF'
#!/bin/bash
d=@W@/gh
echo "${GH_TOKEN:-<none>}" >>"$d/tokens"
if [ "$1 $2 $3" != "api -i -X" ]; then
	echo "stub gh: unexpected: $*" >&2
	exit 1
fi
method="$4" path="$5"
echo "$method $path" >>"$d/calls"
hdr() {
	printf 'HTTP/2.0 %s\n' "$1"
	printf 'Content-Type: application/json\r\n'
	printf 'Github-Authentication-Token-Expiration: %s\r\n' "$(cat "$d/expiry")"
	printf '\r\n'
}
case "$method $path" in
"GET repos/o/r/pulls?state=open&base=main&per_page=100&page=1")
	hdr "200 OK"
	cat "$d/pulls.json"
	;;
"GET repos/o/r/pulls/"*)
	pr="$(jq -c --argjson n "${path##*/}" '.[] | select(.number == $n) | . + {state: "open"}' "$d/pulls.json")"
	hdr "200 OK"
	if [ -n "$pr" ]; then echo "$pr"; else echo '{"state": "closed", "head": {"sha": ""}}'; fi
	;;
"GET repos/o/r/collaborators?per_page=100&page=1")
	hdr "200 OK"
	cat "$d/collaborators.json"
	;;
"GET repos/o/r/collaborators?per_page=100&page=2")
	hdr "200 OK"
	cat "$d/collaborators.p2.json" 2>/dev/null || echo '[]'
	;;
"GET repos/o/r/commits/"*"/status?per_page=100")
	sha="${path#repos/o/r/commits/}"
	sha="${sha%%/*}"
	hdr "200 OK"
	jq -s -c --arg s "$sha" '{statuses: ([.[] | select(.sha == $s)] | group_by(.context) | map(.[-1]))}' "$d/statuses.jsonl"
	;;
"POST repos/o/r/statuses/"*)
	if [ -e "$d/fail-post" ]; then
		printf 'HTTP/2.0 403 Forbidden\n\r\n{}'
		echo "gh: Resource not accessible (HTTP 403)" >&2
		exit 1
	fi
	[ "$6" = --input ] && [ "$7" = - ] || exit 1
	sha="${path##*/}"
	body="$(cat)"
	jq -c --arg s "$sha" --arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '. + {sha: $s, updated_at: $t}' <<<"$body" >>"$d/statuses.jsonl"
	jq -r --arg s "$sha" '"\($s) \(.state) \(.description)"' <<<"$body" >>"$d/posts"
	hdr "201 Created"
	echo '{}'
	;;
*)
	printf 'HTTP/2.0 404 Not Found\n\r\n{}'
	echo "gh: Not Found (HTTP 404)" >&2
	exit 1
	;;
esac
EOF

cat >"$W/bin/incus" <<'EOF'
#!/bin/bash
d=@W@/incus
echo "${VALIDATE_HARDWARE_LOCK_HELD:-<unlocked>}|$*" >>"$d/calls"
drop() { # drop <file> <name>
	jq -c --arg n "$2" '[.[] | select(.name != $n)]' "$1" >"$1.new" && mv "$1.new" "$1"
}
fail_if() {
	if [ "$(cat "$d/fail-delete" 2>/dev/null)" = "$1" ]; then
		echo "Error: stub refuses to delete $1" >&2
		exit 1
	fi
}
case "$*" in
"list homelab-host: --project default -f json") cat "$d/instances.json" ;;
"delete --force --project default homelab-host:"*)
	n="${5#homelab-host:}"
	fail_if "$n"
	drop "$d/instances.json" "$n"
	;;
"image list homelab-host: --project default -f json") cat "$d/images.json" ;;
"image delete --project default homelab-host:"*)
	n="${5#homelab-host:}"
	fail_if "$n"
	jq -c --arg n "$n" '[.[] | select(.fingerprint != $n)]' "$d/images.json" >"$d/images.json.new" && mv "$d/images.json.new" "$d/images.json"
	;;
"storage list homelab-host: -f json") cat "$d/pools.json" ;;
"storage volume list homelab-host:"*" --project default -f json")
	p="${4#homelab-host:}"
	cat "$d/volumes-$p.json"
	;;
"storage volume delete homelab-host:"*)
	p="${4#homelab-host:}"
	fail_if "$5"
	drop "$d/volumes-$p.json" "$5"
	;;
"network list homelab-host: --project default -f json") cat "$d/networks.json" ;;
"network delete homelab-host:"*)
	n="${3#homelab-host:}"
	fail_if "$n"
	drop "$d/networks.json" "$n"
	;;
*)
	echo "UNEXPECTED|$*" >>"$d/calls"
	exit 1
	;;
esac
EOF

cat >"$W/lock/hold.sh" <<'EOF'
#!/bin/bash
d=@W@/lock
[ "$1" = hold ] || exit 2
shift
[ "$1" != -- ] || shift
echo "${VALIDATE_HARDWARE_LOCK_SHA:-<unset>}" >>"$d/shas"
if [ -e "$d/busy" ]; then
	echo "hardware lock: held: validate-hardware-lock-7 by someone@elsewhere" >&2
	exit 2
fi
export VALIDATE_HARDWARE_LOCK_HELD=validate-hardware-lock-1
"$@"
EOF
sed -i "s|@W@|$W|" "$W/bin/gh" "$W/bin/incus" "$W/lock/hold.sh"
chmod +x "$W/bin/gh" "$W/bin/incus" "$W/lock/hold.sh"
export PATH="$W/bin:$PATH"

: >"$W/gh/statuses.jsonl"
: >"$W/gh/posts"
: >"$W/gh/tokens"
date -u -d '+100 days' '+%Y-%m-%d %H:%M:%S UTC' >"$W/gh/expiry"
cat >"$W/gh/collaborators.json" <<'EOF'
[{"login": "op", "permissions": {"admin": true, "maintain": true, "push": true, "triage": true, "pull": true}},
 {"login": "reader", "permissions": {"admin": false, "maintain": false, "push": false, "triage": false, "pull": true}}]
EOF

cat >"$W/incus/instances.json" <<'EOF'
[{"name": "validate-nodeboot-123"}, {"name": "validate-tunnel-gw-31337"},
 {"name": "validate-upg-4242-git"}, {"name": "validate-upg-4242-pc0-g1"}, {"name": "validate-agent-4242-1-g0"},
 {"name": "hardware-queue"}, {"name": "keepme"}, {"name": "validate-nodeboot-sockcheck"}]
EOF
cat >"$W/incus/images.json" <<'EOF'
[{"fingerprint": "aaaa1111", "aliases": [{"name": "validate-upg-4242-a"}, {"name": "validate-upg-4242-b"}]},
 {"fingerprint": "bbbb2222", "aliases": [{"name": "validate-upg-4242-git"}]},
 {"fingerprint": "cccc3333", "aliases": [{"name": "validate-alpine"}]},
 {"fingerprint": "dddd4444", "aliases": [{"name": "validate-alpine-vm"}, {"name": "other"}]},
 {"fingerprint": "eeee5555", "aliases": []}]
EOF
echo '[{"name": "default"}, {"name": "other"}]' >"$W/incus/pools.json"
cat >"$W/incus/volumes-default.json" <<'EOF'
[{"name": "validate-nodeboot-seeded-img-31337", "type": "custom"},
 {"name": "validate-tunnel-1", "type": "container"},
 {"name": "validate-scratch", "type": "custom"}]
EOF
echo '[{"name": "validate-seedmedia-seed-iso-9", "type": "custom"}]' >"$W/incus/volumes-other.json"
cat >"$W/incus/networks.json" <<'EOF'
[{"name": "valwan-31337", "managed": true}, {"name": "valwan-abc", "managed": true},
 {"name": "home-lan", "managed": true}, {"name": "valwan-1", "managed": false}]
EOF
: >"$W/incus/calls"

# --- the fixture repository: main, and one commit per fixture PR head ------------

git_fixture_repos "$W/origin.git" "$W/src"
mkdir -p "$W/src/scripts/validate"
printf 'incusos-base:\n\t@echo "stub incusos-base"\n' >"$W/src/Makefile"
echo 202611182248 >"$W/src/scripts/incusos-base.version"
cat >"$W/src/scripts/validate/run.sh" <<EOF
#!/bin/bash
rec=$W/rec
sha="\$(git rev-parse HEAD)"
echo "\$sha start \$(date +%s%N)" >>"\$rec/runs"
env >"\$rec/env.\$sha"
echo "\$*" >"\$rec/args.\$sha"
RC=0 SLEEP=2 FAIL=""
. ./behaviour
[ "\$SLEEP" = 0 ] || sleep "\$SLEEP"
echo "======== summary ========"
printf '%-52s %-14s %s\n' a-passes.sh ok "1 passed, 0 failed, 0 skipped"
for f in \$FAIL; do
	printf '%-52s %-14s %s\n' "\$f" FAILED "0 passed, 1 failed, 0 skipped"
done
[ "\$RC" != 2 ] || printf '%-52s %-14s %s\n' needs-host.sh "prereqs unmet" "(no summary)"
echo "\$sha end \$(date +%s%N)" >>"\$rec/runs"
exit "\$RC"
EOF
chmod +x "$W/src/scripts/validate/run.sh"
echo 'RC=0' >"$W/src/behaviour"
git -C "$W/src" add -A
git -C "$W/src" commit -q -m main
git -C "$W/src" push -q origin main

# pr_head <number> <behaviour> — a commit on main whose behaviour file says
# what its stub suite does, pushed as refs/pull/<number>/head; prints its SHA.
pr_head() {
	git -C "$W/src" checkout -q --detach main
	printf '%s\n# %s\n' "$2" "$RANDOM$RANDOM" >"$W/src/behaviour"
	git -C "$W/src" commit -q -am "pr $1"
	git -C "$W/src" push -q -f origin "HEAD:refs/pull/$1/head"
	git -C "$W/src" rev-parse HEAD
}

# pr <number> <sha> [login] [type] [auto 0|1] [head repo|null] [base] — one PR,
# as REST's /pulls lists it.
pr() {
	jq -nc --argjson n "$1" --arg sha "$2" --arg login "${3:-op}" --arg type "${4:-User}" \
		--arg auto "${5:-0}" --arg repo "${6:-$REPO}" --arg base "${7:-main}" \
		--arg evil "\$(touch $W/pwned); \`touch $W/pwned\`" '
		{number: $n, title: $evil, base: {ref: $base},
		 head: {sha: $sha, ref: $evil, repo: (if $repo == "null" then null else {full_name: $repo} end)},
		 user: {login: $login, type: $type},
		 auto_merge: (if $auto == "1" then {merge_method: "rebase"} else null end)}'
}

prs() { # prs <pr json>... — the open PR list
	jq -s -c . <<<"$(printf '%s\n' "$@")" >"$W/gh/pulls.json"
}

cat >"$W/etc/config.env" <<EOF
HQ_REPO=$REPO
HQ_CLONE_URL=file://$W/origin.git
HQ_OPERATOR=op
HQ_AUTHORS="op app/dependabot app/github-actions"
HQ_STATE_DIR=$W/state
HQ_TOKEN_FILE=$W/etc/token
HQ_LOCK_LIB=$W/lock/hold.sh
HQ_BRIDGE=0
HQ_DOCKER_SWEEP=0
HQ_POLL_INTERVAL=1
HQ_WATCH_INTERVAL=1
HQ_KILL_GRACE=5
HQ_TIMEOUT=60
EOF
export HQ_ETC="$W/etc"

tick() {
	timeout 120 "$JOB" tick >>"$W/job.log" 2>&1
}

posts_for() { # posts_for <sha> — "<state> <description>" lines posted to it
	awk -v s="$1" '$1 == s { $1 = ""; sub(/^ /, ""); print }' "$W/gh/posts"
}

states_for() {
	posts_for "$1" | cut -d' ' -f1 | tr '\n' ' ' | sed 's/ $//'
}

ran() { # ran <sha> — how many times its stub suite started
	grep -c "^$1 start" "$W/rec/runs" 2>/dev/null
}

wait_for() { # wait_for <seconds> <cmd...>
	local n=0 limit=$(($1 * 5))
	shift
	until "$@"; do
		n=$((n + 1))
		[ "$n" -lt "$limit" ] || return 1
		sleep 0.2
	done
}

: >"$W/rec/runs"

# --- 1. Eligibility, priority, order --------------------------------------------
echo "== 1. eligibility and order"

A=$(pr_head 11 'RC=0')
B=$(pr_head 12 'RC=0')
C=$(pr_head 13 'RC=0')
D=$(pr_head 14 'RC=0')
E=$(pr_head 15 'RC=0')
F=$(pr_head 16 'RC=0')
G=$(pr_head 17 'RC=0')
H=$(pr_head 18 'RC=0')
prs "$(pr 11 "$A")" "$(pr 12 "$B" op User 1)" "$(pr 13 "$C" op User 0 fork/r)" \
	"$(pr 14 "$D" stranger)" "$(pr 15 "$E" 'dependabot[bot]' Bot)" \
	"$(pr 16 "$F" op User 0 "$REPO" release)" "$(pr 17 "$G" op User 0 null)" \
	"$(pr 18 "$H" 'github-actions[bot]' Bot)"
for _ in 1 2 3 4 5; do tick; done

order="$(awk '$2 == "start" { print $1 }' "$W/rec/runs" | tr '\n' ' ' | sed 's/ $//')"
check_eq "auto-merge PR #12 runs first, then #11, #15 (Dependabot) and #18 by number" \
	"$B $A $E $H" "$order"
for s in "$B" "$A" "$E" "$H"; do
	check_eq "head ${s:0:7}: pending, then success" "pending success" "$(states_for "$s")"
done
check_eq "a cross-repo PR gets no status and never runs" "0/" "$(ran "$C")/$(posts_for "$C")"
check_eq "an author off the allowlist gets no status and never runs" "0/" "$(ran "$D")/$(posts_for "$D")"
check_eq "a PR into another branch gets no status and never runs" "0/" "$(ran "$F")/$(posts_for "$F")"
check_eq "a PR whose head repository is gone gets no status and never runs" "0/" "$(ran "$G")/$(posts_for "$G")"
check_eq "a fifth pass finds nothing left to run" 4 "$(grep -c ' start ' "$W/rec/runs")"
check_eq "the suite ran as run.sh --group incus,incus-vm --strict --allow-skip incus-cluster" \
	"--group incus,incus-vm --strict --allow-skip incus-cluster" "$(cat "$W/rec/args.$A")"
check "the suite ran with VALIDATE_OS=incusos set explicitly" grep -qx 'VALIDATE_OS=incusos' "$W/rec/env.$A"
check_eq "the lock was taken with VALIDATE_HARDWARE_LOCK_SHA set to each PR head" \
	"$B $A $E $H" "$(tr '\n' ' ' <"$W/lock/shas" | sed 's/ $//')"
check "the suite ran holding the lock (VALIDATE_HARDWARE_LOCK_HELD)" \
	grep -qx 'VALIDATE_HARDWARE_LOCK_HELD=validate-hardware-lock-1' "$W/rec/env.$A"
check_eq "the success description counts the scripts" \
	"success passed: 1 scripts (incus,incus-vm on incusos) in 1m" "$(posts_for "$A" | tail -n 1)"
check "no description carries a token-expiry note 100 days out" bash -c "! grep -q 'token expires' '$W/gh/posts'"
check "a PR title or branch name never reached a shell (both would touch a file)" test ! -e "$W/pwned"

# --- the sweep (it ran before the first run, under the lock) ---------------------
echo
echo "== 2. the sweep"
check_eq "instances left: the job's VM, a non-matching name, and a name without a pid" \
	"hardware-queue keepme validate-nodeboot-sockcheck" "$(jq -r '[.[].name] | join(" ")' "$W/incus/instances.json")"
check_eq "images left: the pinned aliases, and one with no alias" \
	"cccc3333 dddd4444 eeee5555" "$(jq -r '[.[].fingerprint] | join(" ")' "$W/incus/images.json")"
check_eq "volumes left in default: a container volume and a name without a pid" \
	"validate-tunnel-1 validate-scratch" "$(jq -r '[.[].name] | join(" ")' "$W/incus/volumes-default.json")"
check_eq "volumes left in another pool: none" "" "$(jq -r '[.[].name] | join(" ")' "$W/incus/volumes-other.json")"
check_eq "networks left: everything but valwan-<pid>" \
	"valwan-abc home-lan valwan-1" "$(jq -r '[.[].name] | join(" ")' "$W/incus/networks.json")"
check "the sweep never touched profiles (so not the lock) or an unexpected object" \
	bash -c "! grep -Eq '\\|profile( |\$)|UNEXPECTED' '$W/incus/calls'"
check "every sweep call ran holding the lock" bash -c "! grep -q '^<unlocked>' '$W/incus/calls'"

# --- 3. The token stays out of the suite ------------------------------------------
echo
echo "== 3. the token"
check "every gh call carried the job's token" bash -c "[ -s '$W/gh/tokens' ] && ! grep -vqx '$TOKEN' '$W/gh/tokens'"
check "the token is absent from the suite's environment" bash -c "! grep -qF '$TOKEN' '$W/rec/'env.*"
check "the token is absent from the run logs and the job's log" \
	bash -c "! grep -rqF '$TOKEN' '$W/state/logs' '$W/job.log'"
check "one run log per SHA" test -s "$W/state/logs/$A.log"

# --- 4. One at a time across processes ---------------------------------------------
echo
echo "== 4. one at a time"
I=$(pr_head 21 'SLEEP=6')
prs "$(pr 21 "$I")"
tick &
tick_pid=$!
wait_for 15 grep -q "^$I start" "$W/rec/runs"
"$JOB" tick >"$W/second.log" 2>&1
check "a second pass while one runs exits without running anything" grep -q 'another job process holds' "$W/second.log"
wait "$tick_pid"
check_eq "the PR ran once, to success" "1 pending success" "$(ran "$I") $(states_for "$I")"
# shellcheck disable=SC2016 # awk's own fields
check "no two runs overlapped" awk '
	$2 == "start" { if (open != "") bad = 1; open = $1 }
	$2 == "end" { open = "" }
	END { exit bad }' "$W/rec/runs"

# --- 5. Superseding ----------------------------------------------------------------
echo
echo "== 5. superseding"
J1=$(pr_head 31 'SLEEP=40')
prs "$(pr 31 "$J1")"
tick &
tick_pid=$!
wait_for 15 grep -q "^$J1 pending" "$W/gh/posts"
J2=$(pr_head 31 'RC=0')
prs "$(pr 31 "$J2")"
SECONDS=0
wait "$tick_pid"
check "a push to the PR being run stops the run within seconds" test "$SECONDS" -lt 20
check_eq "the old head gets error, superseded by the new one" \
	"error superseded by ${J2:0:7}" "$(posts_for "$J1" | tail -n 1)"
check "the stopped run never finished" bash -c "! grep -q '^$J1 end' '$W/rec/runs'"
tick
check_eq "the next pass runs only the newest head" "1 1" "$(ran "$J1") $(ran "$J2")"
check_eq "the newest head passes" "pending success" "$(states_for "$J2")"
K=$(pr_head 32 'SLEEP=40')
prs "$(pr 32 "$K")"
tick &
tick_pid=$!
wait_for 15 grep -q "^$K pending" "$W/gh/posts"
prs
wait "$tick_pid"
check_eq "closing the PR being run stops it: error, superseded" \
	"error superseded: PR #32 closed" "$(posts_for "$K" | tail -n 1)"

# --- 6. What GitHub's statuses say -------------------------------------------------
echo
echo "== 6. statuses as the queue's memory"
L1=$(pr_head 41 'RC=0')
L2=$(pr_head 42 'RC=0')
L3=$(pr_head 43 'RC=0')
L4=$(pr_head 44 'RC=0')
L5=$(pr_head 45 'RC=0')
st() { # st <sha> <state> <age>
	jq -nc --arg s "$1" --arg st "$2" --arg t "$(date -u -d "$3 ago" +%Y-%m-%dT%H:%M:%SZ)" \
		'{sha: $s, state: $st, context: "validate/hardware", description: "fixture", updated_at: $t}' >>"$W/gh/statuses.jsonl"
}
st "$L1" pending '3 hours'
st "$L2" error '10 minutes'
st "$L3" error '40 minutes'
st "$L4" success '1 hour'
st "$L5" failure '1 hour'
prs "$(pr 41 "$L1")" "$(pr 42 "$L2")" "$(pr 43 "$L3")" "$(pr 44 "$L4")" "$(pr 45 "$L5")"
for _ in 1 2 3; do tick; done
check_eq "a stale pending reruns, to success" "1 pending success" "$(ran "$L1") $(states_for "$L1")"
check_eq "an error 10 minutes old waits out its backoff" "0" "$(ran "$L2")"
check_eq "an error 40 minutes old is retried" "1 pending success" "$(ran "$L3") $(states_for "$L3")"
check_eq "success and failure are final" "0 0" "$(ran "$L4") $(ran "$L5")"

# --- 7. Outcomes ---------------------------------------------------------------------
echo
echo "== 7. outcomes"
one() { # one <number> <behaviour> — run one PR alone; prints its SHA
	local s
	s=$(pr_head "$1" "$2")
	prs "$(pr "$1" "$s")"
	tick
	echo "$s"
}
M=$(one 51 $'RC=3\necho "SKIP: targeted create [incus-cluster] — one member"\necho "SKIP: target placement [incus-cluster] — one member"')
check_eq "exit 3 with only allow-listed skips is success, counting them" \
	"success passed: 1 scripts, 2 allow-listed skip(s) [incus-cluster] in 1m" "$(posts_for "$M" | tail -n 1)"
N=$(one 52 'RC=1 FAIL=node-boots-broken.sh')
check_eq "exit 1 is failure naming the script" \
	"failure failed: node-boots-broken.sh (1m)" "$(posts_for "$N" | tail -n 1)"
O=$(one 53 'RC=2')
check_eq "exit 2 is error naming the unmet script" \
	"error run.sh exit 2, prerequisites unmet: needs-host.sh; says nothing about the PR" "$(posts_for "$O" | tail -n 1)"
echo "HQ_TIMEOUT=3" >>"$W/etc/config.env"
P=$(one 54 'SLEEP=40')
sed -i '/^HQ_TIMEOUT=3$/d' "$W/etc/config.env"
got="$(posts_for "$P" | tail -n 1)"
check "a run past HQ_TIMEOUT is stopped and gets error, timed out" test "${got#error timed out after }" != "$got"
check "the timed-out run never finished" bash -c "! grep -q '^$P end' '$W/rec/runs'"
jq -c '. + [{name: "validate-tunnel-node0-77"}]' "$W/incus/instances.json" >"$W/incus/i.new" && mv "$W/incus/i.new" "$W/incus/instances.json"
echo validate-tunnel-node0-77 >"$W/incus/fail-delete"
Q=$(one 55 'RC=0')
rm -f "$W/incus/fail-delete"
got="$(posts_for "$Q" | tail -n 1)"
want="error job fault: sweep failed: deleting instance validate-tunnel-node0-77: Error: stub refuses"
check_eq "a failed sweep is a job fault: error naming what it couldn't delete" "$want" "${got:0:${#want}}"
check_eq "after a failed sweep the suite never starts" 0 "$(ran "$Q")"

# --- 8. The lock held by someone else ---------------------------------------------
echo
echo "== 8. the lock"
touch "$W/lock/busy"
R=$(one 61 'RC=0')
check_eq "while a person holds the lock nothing runs and no status is posted" "0/" "$(ran "$R")/$(posts_for "$R")"
check "the job says the PR stays queued" grep -q 'stays queued' "$W/job.log"
rm -f "$W/lock/busy"
tick
check_eq "once the lock is free the queued PR runs" "1 pending success" "$(ran "$R") $(states_for "$R")"

# --- 9. A second writer ----------------------------------------------------------------
echo
echo "== 9. the writer check"
cp "$W/gh/collaborators.json" "$W/gh/collaborators.ok"
jq -c '. + [{login: "mallory", permissions: {push: true, pull: true}}]' "$W/gh/collaborators.ok" >"$W/gh/collaborators.json"
S=$(one 71 'RC=0')
cp "$W/gh/collaborators.ok" "$W/gh/collaborators.json"
check_eq "with a second writer nothing runs and the PR gets error naming them" \
	"0 error refused: write access is not op's alone (mallory); nothing ran" "$(ran "$S") $(posts_for "$S" | tail -n 1)"

# A writer past the first page of collaborators is still seen.
jq -c -n '[range(100) | {login: "reader\(.)", permissions: {pull: true}}]' >"$W/gh/collaborators.json"
jq -c -n '[{login: "mallory", permissions: {push: true, pull: true}}]' >"$W/gh/collaborators.p2.json"
S2=$(one 72 'RC=0')
cp "$W/gh/collaborators.ok" "$W/gh/collaborators.json"
rm -f "$W/gh/collaborators.p2.json"
check_eq "a second writer on page 2 of the collaborators is seen" \
	"0 error refused: write access is not op's alone (mallory, (not op)); nothing ran" "$(ran "$S2") $(posts_for "$S2" | tail -n 1)"

echo
echo "== 9b. a status that can't be posted"
touch "$W/gh/fail-post"
U=$(one 73 'RC=0')
tick
check_eq "with statuses unwritable the head ran once, not once per pass" "1" "$(ran "$U")"
rm -f "$W/gh/fail-post"
touch -d '1 hour ago' "$W/state/unposted/$U"
tick
check_eq "after the backoff it reruns, and posts" "2 success" "$(ran "$U") $(posts_for "$U" | tail -n 1 | cut -d" " -f1)"
check "a posted result clears the record" test ! -e "$W/state/unposted/$U"

# --- 10. Token expiry ----------------------------------------------------------------
echo
echo "== 10. token expiry"
date -u -d '+10 days' '+%Y-%m-%d %H:%M:%S UTC' >"$W/gh/expiry"
exp="$(date -u -d '+10 days' +%Y-%m-%d)"
long="$(printf 'a-long-script-name-that-keeps-going-%s.sh ' 1 2 3 4 5)"
T=$(one 81 "RC=1 FAIL=\"$long\"")
check "within 30 days of expiry every description ends 'token expires $exp'" bash -c \
	"[ \"\$(awk -v s='$T' '\$1 == s' '$W/gh/posts' | grep -vc 'token expires $exp\$')\" = 0 ] && grep -q '^$T failure' '$W/gh/posts'"
check "the job's log warns about the expiry" grep -q "WARNING: the GitHub token expires $exp" "$W/job.log"
check "every description is at most 140 characters" bash -c \
	"jq -r 'select(.context == \"validate/hardware\" and .description != \"fixture\") | .description | length' '$W/gh/statuses.jsonl' | awk '\$1 > 140 { bad = 1 } END { exit bad }'"

# --- 11. Config the job refuses --------------------------------------------------------
echo
echo "== 11. refused config"
# shellcheck disable=SC2329 # run by check
refused() { # refused <line> — the job exits 2 with that line appended
	local rc
	cp "$W/etc/config.env" "$W/etc/config.ok"
	echo "$1" >>"$W/etc/config.env"
	"$JOB" tick >"$W/refused.log" 2>&1
	rc=$?
	cp "$W/etc/config.ok" "$W/etc/config.env"
	[ "$rc" = 2 ]
}
check "the github group is refused" refused 'HQ_GROUPS=incus,github'
check "host-os on the allow-list is refused" refused 'HQ_ALLOW_SKIP=incus-cluster,host-os'
check "a VM name the sweep would match is refused" refused 'HQ_VM_NAME=validate-queue-1'

# --- 12. setup.sh refuses a committable .env --------------------------------------
echo
echo "== 12. setup.sh and .env"
# A throwaway checkout holding setup.sh and its config, and stub gh and incus
# that fail loudly: each refusal must come before either is called.
S="$W/setup-repo"
git init -q -b main "$S"
git_fixture_config "$S"
mkdir -p "$S/scripts/hardware-queue"
cp "$ROOT_DIR/scripts/hardware-queue/setup.sh" "$ROOT_DIR/scripts/hardware-queue/config.env" "$S/scripts/hardware-queue/"
printf 'ISSUE664_VALIDATE_BOT_PAT=%s\n' "$TOKEN" >"$S/.env"
cp "$W/gh/calls" "$W/gh/calls.before"
setup() { # setup — run it with no token in the environment; output in setup.out
	(cd "$S" && env -u ISSUE664_VALIDATE_BOT_PAT ./scripts/hardware-queue/setup.sh) >"$W/setup.out" 2>&1
}
setup
check "with .env not ignored, setup.sh refuses and names the file" bash -c \
	"grep -q 'ERROR: $S/.env is not ignored by git' '$W/setup.out'"
git -C "$S" add -f .env
echo '/.env' >"$S/.gitignore"
setup
check "with .env tracked, setup.sh refuses and names the file" bash -c \
	"grep -q 'ERROR: $S/.env is tracked by git' '$W/setup.out'"
git -C "$S" rm -q --cached .env
: >"$S/.env"
setup
check "with .env ignored and untracked, it gets past the guard (to the missing token)" \
	grep -q 'ERROR: ISSUE664_VALIDATE_BOT_PAT is not set' "$W/setup.out"
check "no refusal contacted GitHub" cmp -s "$W/gh/calls" "$W/gh/calls.before"

summary
