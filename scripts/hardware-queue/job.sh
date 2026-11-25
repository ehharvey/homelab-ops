#! /bin/bash
# The validate/hardware queue (#664): runs the hardware validate groups on
# trusted PR heads, one at a time, and posts each result to the head commit as
# the status `validate/hardware`. It runs in a VM on the Incus host that
# setup.sh builds, from a copy of `main` taken when the VM was built, never
# from a PR head. scripts/validate/README.md § "The hardware queue" is the
# operator's view; docs/Decisions.md §20 and §21 record why.
#
#   job.sh serve    poll forever (the systemd unit)
#   job.sh tick     one pass: pick at most one PR head, run it, report it
#   job.sh sweep    remove what interrupted runs left on the Incus host; it
#                   doesn't take the lock itself, so by hand run it as
#                   lib-hardware-lock.sh hold -- job.sh sweep
#
# A pass:
#
# 1. Lists open PRs into main. A PR is eligible when it isn't cross-repository
#    and its author is in HQ_AUTHORS (spelled as `gh pr list` spells it: a bot
#    is app/<login>). Nothing else about a PR is read; its branch name and
#    title never reach a shell.
# 2. Reads each eligible head's `validate/hardware` status. GitHub's statuses
#    are the queue's only memory: success and failure are final; pending
#    means rerun (a crash, a reboot, or the operator asking for a retry); error
#    runs again once it is HQ_ERROR_BACKOFF old; no status means queued.
# 3. Picks one: PRs with auto-merge enabled first, then by PR number. The SHA
#    is fixed here, and the run checks out exactly it.
# 4. Checks /collaborators: anyone with write access but HQ_OPERATOR, and
#    nothing runs; the picked head gets `error`.
# 5. Runs, in a fresh detached worktree of one persistent clone (the IncusOS
#    image cache lives under the clone, found through the git common dir):
#
#      lib-hardware-lock.sh hold -- job.sh _locked
#        -> sweep, make incusos-base, prune old cached images, then
#           VALIDATE_OS=$HQ_BACKEND run.sh --group $HQ_GROUPS --strict --allow-skip $HQ_ALLOW_SKIP
#
#    with a scrubbed environment (env -i): the GitHub token is kept out of the
#    suite's environment and the logs, and handed to `gh` alone. (The suite
#    runs as the same user, so it could still read the token file; the README's
#    "trust model" says so.) `pending` goes up
#    once the lock is held. If `hold` exits 2 without having run anything (a
#    person holds the lock, or Incus is unreachable), no status is posted and
#    the head stays queued.
# 6. While it runs, checks the PR every HQ_WATCH_INTERVAL: a new head or a
#    closed PR stops the run (TERM to its process group, KILL after
#    HQ_KILL_GRACE) and the old SHA gets `error` "superseded". So does
#    HQ_TIMEOUT, with "timed out".
# 7. Maps the outcome: run.sh 0 or 3 -> success (3 says how many allow-listed
#    skips), 1 -> failure naming the scripts, 2 or anything else -> error. A
#    failed sweep, checkout or `make incusos-base` is a job fault: error.
#
# Descriptions stay within GitHub's 140 characters. Inside HQ_TOKEN_WARN_DAYS
# of the token's expiry (GitHub-Authentication-Token-Expiration, read from
# every response) each one ends "token expires YYYY-MM-DD" and the log warns.
#
# Config: config.env beside this file, then $HQ_ETC/setup.env (setup.sh's),
# then $HQ_ETC/config.env (the operator's). HQ_ETC defaults to
# /etc/hardware-queue. Logs: one per SHA in $HQ_STATE_DIR/logs, the newest
# HQ_LOG_KEEP kept; the job's own lines go to stderr (the journal).
#
# Proven by scripts/validate/hardware-queue-runs-trusted-prs-one-at-a-time.sh
# against a stub gh, a stub incus, a stub lock and a stub suite.

set -uo pipefail
export LC_ALL=C.UTF-8 GIT_TERMINAL_PROMPT=0 GH_PROMPT_DISABLED=1 GH_NO_UPDATE_NOTIFIER=1

HQ_CODE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HQ_SELF="$HQ_CODE_ROOT/scripts/hardware-queue/job.sh"
HQ_ETC="${HQ_ETC:-/etc/hardware-queue}"

# shellcheck source=scripts/hardware-queue/config.env
. "$HQ_CODE_ROOT/scripts/hardware-queue/config.env"
HQ_LOCK_LIB="$HQ_CODE_ROOT/scripts/validate/lib-hardware-lock.sh"
HQ_DOCKER_SWEEP=1
HQ_BRIDGE_PROJECT=hq-incus-bridge
for _f in "$HQ_ETC/setup.env" "$HQ_ETC/config.env"; do
	# shellcheck disable=SC1090 # instance config, written by setup.sh and the operator
	[ ! -r "$_f" ] || . "$_f"
done
unset _f

HQ_RUNS="$HQ_STATE_DIR/runs"
HQ_LOGS="$HQ_STATE_DIR/logs"
HQ_CLONE="$HQ_STATE_DIR/repo"
HQ_INCUS_SOCKET="$HQ_STATE_DIR/incus-bridge/incus.sock"

# The README's naming table (§ "What the scripts name things on the Incus
# host"): a validate- name with the PID as a whole segment, which may be
# followed by more (validate-upg-<pid>-pc0-g0). The same shape picks the
# images to sweep, so a pinned alias must not have a numeric segment. Profiles
# are never swept: the lock's validate-hardware-lock-<n> has the pattern's shape.
HQ_SWEEP_NAME_RE='^validate-[a-z0-9]+(-[a-z0-9]+)*-[0-9]+(-[a-z0-9]+)*$'
HQ_SWEEP_NET_RE='^valwan-[0-9]+$'

HQ_TOKEN=""
HQ_TOKEN_EXPIRES=""
HQ_TOKEN_DAYS=""
HQ_WARNED_AT=""
GH_CODE=""
GH_BODY=""

log() {
	printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

# check_config — 0, or says what's wrong and returns 2.
check_config() {
	local v bad=0
	for v in HQ_POLL_INTERVAL HQ_WATCH_INTERVAL HQ_TIMEOUT HQ_ERROR_BACKOFF HQ_KILL_GRACE HQ_TOKEN_WARN_DAYS HQ_LOG_KEEP; do
		[[ "${!v}" =~ ^[0-9]+$ ]] || {
			log "ERROR: $v=${!v}: want whole seconds or a count"
			bad=1
		}
	done
	[[ "$HQ_REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || {
		log "ERROR: HQ_REPO=$HQ_REPO: want owner/name"
		bad=1
	}
	case ",$HQ_GROUPS," in
	*,github,*)
		log "ERROR: HQ_GROUPS=$HQ_GROUPS: the github group never runs here (it writes to the real repository)"
		bad=1
		;;
	esac
	case ",$HQ_ALLOW_SKIP," in
	*,host-os,*)
		log "ERROR: HQ_ALLOW_SKIP=$HQ_ALLOW_SKIP: host-os must never be allowed, or a backend the scripts don't support would pass by skipping"
		bad=1
		;;
	esac
	[ -n "$HQ_BACKEND" ] && [ -n "$HQ_GROUPS" ] || {
		log "ERROR: HQ_BACKEND and HQ_GROUPS must be set"
		bad=1
	}
	if [[ "$HQ_VM_NAME" =~ $HQ_SWEEP_NAME_RE ]]; then
		log "ERROR: HQ_VM_NAME=$HQ_VM_NAME matches the sweep's instance pattern, so a sweep would delete the job's own VM"
		bad=1
	fi
	[ "$bad" = 0 ] || return 2
}

# --- GitHub -------------------------------------------------------------------

# note_expiry <header value> — record the token's expiry, and warn about it at
# most hourly while it is inside the warning window.
note_expiry() {
	local e now
	e="$(date -u -d "$1" +%s 2>/dev/null)" || return 0
	now="$(date -u +%s)"
	HQ_TOKEN_EXPIRES="$(date -u -d "@$e" +%Y-%m-%d)"
	HQ_TOKEN_DAYS=$(((e - now) / 86400))
	if [ "$HQ_TOKEN_DAYS" -lt "$HQ_TOKEN_WARN_DAYS" ] &&
		{ [ -z "$HQ_WARNED_AT" ] || [ $((SECONDS - HQ_WARNED_AT)) -ge 3600 ]; }; then
		HQ_WARNED_AT=$SECONDS
		log "WARNING: the GitHub token expires $HQ_TOKEN_EXPIRES ($HQ_TOKEN_DAYS days); make a new one and rerun scripts/hardware-queue/setup.sh"
	fi
}

# gh_api <method> <path> [<json body>] — one REST call with the job's token.
# GH_CODE is the HTTP status (000 without a response) and GH_BODY the body.
# Returns 0 on a 2xx.
gh_api() {
	local method="$1" path="$2" body="${3-}" raw headers exp
	if [ -n "$body" ]; then
		raw="$(printf '%s' "$body" | GH_TOKEN="$HQ_TOKEN" gh api -i -X "$method" "$path" --input - 2>"$HQ_TMP/gh.err")"
	else
		raw="$(GH_TOKEN="$HQ_TOKEN" gh api -i -X "$method" "$path" 2>"$HQ_TMP/gh.err")"
	fi
	raw="${raw//$'\r'/}"
	GH_CODE="$(sed -n '1s/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' <<<"$raw")"
	GH_CODE="${GH_CODE:-000}"
	headers="$(awk 'NR > 1 && /^$/ { exit } NR > 1' <<<"$raw")"
	GH_BODY="$(awk 'b; /^$/ && !b { b = 1 }' <<<"$raw")"
	exp="$(grep -i -m 1 '^github-authentication-token-expiration:' <<<"$headers" | cut -d: -f2-)"
	[ -z "$exp" ] || note_expiry "${exp# }"
	case "$GH_CODE" in
	2??) return 0 ;;
	esac
	return 1
}

gh_err() {
	echo "HTTP $GH_CODE: $(tr '\n' ' ' <"$HQ_TMP/gh.err" | head -c 200)"
}

token_suffix() {
	if [ -n "$HQ_TOKEN_EXPIRES" ] && [ "$HQ_TOKEN_DAYS" -lt "$HQ_TOKEN_WARN_DAYS" ]; then
		printf '; token expires %s' "$HQ_TOKEN_EXPIRES"
	fi
}

# A head whose final status couldn't be posted is remembered here, by SHA, so
# an unwritable token costs one run per HQ_ERROR_BACKOFF, not one per pass.
unposted() { echo "$HQ_STATE_DIR/unposted/$1"; }

# final_status <sha> <state> <description> — post_status for a run's last word.
final_status() {
	if post_status "$@"; then
		rm -f "$(unposted "$1")"
	else
		mkdir -p "$HQ_STATE_DIR/unposted" && : >"$(unposted "$1")"
	fi
}

# post_status <sha> <state> <description>
post_status() {
	local sha="$1" state="$2" desc="$3" suffix max body
	suffix="$(token_suffix)"
	max=$((140 - ${#suffix}))
	[ "${#desc}" -le "$max" ] || desc="${desc:0:$((max - 3))}..."
	desc="$desc$suffix"
	body="$(jq -nc --arg s "$state" --arg c "$HQ_CONTEXT" --arg d "$desc" \
		'{state: $s, context: $c, description: $d}')"
	if gh_api POST "repos/$HQ_REPO/statuses/$sha" "$body"; then
		log "status ${sha:0:12}: $state: $desc"
	else
		log "ERROR: posting $state to ${sha:0:12} failed, $(gh_err)"
		return 1
	fi
}

# The GitHub calls below run in this shell, never in $(...), so the token
# expiry gh_api reads from each response is the one post_status reports.

# gh_pages <path with a query> — GET every page (100 per page) of a JSON
# array into GH_BODY, as one array. Returns 1 on a failed page. Not `gh api
# --paginate`: gh_api reads each response's status and headers itself.
gh_pages() {
	local path="$1" page all='[]'
	for page in $(seq 1 50); do
		gh_api GET "$path&page=$page" || return 1
		all="$(jq -c -s '.[0] + .[1]' <(printf '%s' "$all") <(printf '%s' "$GH_BODY"))" || return 1
		[ "$(jq 'length' <<<"$GH_BODY")" -ge 100 ] || break
	done
	GH_BODY="$all"
}

# eligible — HQ_ELIGIBLE: "<number> <sha> <auto-merge 0|1> <author>" per
# eligible open PR.
eligible() {
	HQ_ELIGIBLE=""
	gh_pages "repos/$HQ_REPO/pulls?state=open&base=main&per_page=100" || {
		log "ERROR: listing open PRs failed, $(gh_err)"
		return 1
	}
	HQ_ELIGIBLE="$(jq -r --arg repo "$HQ_REPO" --arg authors "$HQ_AUTHORS" '
		($authors | split(" ") | map(select(. != ""))) as $ok
		| .[]
		| select(.base.ref == "main")
		| select(.head.repo != null
			and (.head.repo.full_name | ascii_downcase) == ($repo | ascii_downcase))
		| (if .user.type == "Bot" then "app/" + (.user.login | sub("\\[bot\\]$"; ""))
		   else .user.login end) as $a
		| select(any($ok[]; . == $a))
		| "\(.number) \(.head.sha) \(if .auto_merge != null then 1 else 0 end) \($a)"' <<<"$GH_BODY")"
}

# head_status <sha> — HS_STATE (none when it has no validate/hardware status)
# and HS_AT (its epoch, 0 for none).
head_status() {
	local line at
	HS_STATE="" HS_AT=0
	gh_api GET "repos/$HQ_REPO/commits/$1/status?per_page=100" || return 1
	line="$(jq -r --arg c "$HQ_CONTEXT" \
		'[.statuses[]? | select(.context == $c)] | first // empty | "\(.state) \(.updated_at)"' <<<"$GH_BODY")"
	if [ -z "$line" ]; then
		HS_STATE=none
		return 0
	fi
	read -r HS_STATE at <<<"$line"
	HS_AT="$(date -u -d "$at" +%s 2>/dev/null || echo 0)"
}

# pick — heads never run go before error retries (rank 0 before 1), so a head
# that keeps erroring can't starve newer ones; then auto-merge, then PR number.
# PICKED: "<number> <sha> <why>" for the head to run next, or empty.
pick() {
	local number sha auto now ready=()
	PICKED=""
	now="$(date -u +%s)"
	eligible || return 1
	while read -r number sha auto _; do
		[ -n "$number" ] || continue
		if ! [[ "$number" =~ ^[1-9][0-9]*$ && "$sha" =~ ^[0-9a-f]{40}$ ]]; then
			log "WARNING: skipping a PR whose number or head isn't well formed"
			continue
		fi
		if [ -e "$(unposted "$sha")" ] &&
			[ $((now - $(stat -c %Y "$(unposted "$sha")" 2>/dev/null || echo 0))) -lt "$HQ_ERROR_BACKOFF" ]; then
			log "WARNING: PR #$number ${sha:0:12}: its last result couldn't be posted; waiting out HQ_ERROR_BACKOFF before rerunning it"
			continue
		fi
		if ! head_status "$sha"; then
			log "WARNING: reading the status of PR #$number failed, $(gh_err)"
			continue
		fi
		case "$HS_STATE" in
		success | failure) continue ;;
		none) ready+=("0 $auto $number $sha queued") ;;
		pending) ready+=("0 $auto $number $sha stale-pending") ;;
		error)
			[ $((now - HS_AT)) -ge "$HQ_ERROR_BACKOFF" ] || continue
			ready+=("1 $auto $number $sha error-retry")
			;;
		*) log "WARNING: PR #$number has an unknown $HQ_CONTEXT state '$HS_STATE'" ;;
		esac
	done <<<"$HQ_ELIGIBLE"
	[ "${#ready[@]}" -gt 0 ] || return 0
	PICKED="$(printf '%s\n' "${ready[@]}" | sort -k1,1n -k2,2nr -k3,3n | head -n 1 | cut -d' ' -f3-)"
}

# writers — 0 when HQ_OPERATOR is the only collaborator with write access;
# 1 when not (HQ_WRITERS says who else); 2 when it can't tell.
writers() {
	local others
	HQ_WRITERS=""
	gh_pages "repos/$HQ_REPO/collaborators?per_page=100" || {
		HQ_WRITERS="$(gh_err)"
		return 2
	}
	others="$(jq -r --arg op "$HQ_OPERATOR" '
		[.[] | select(.permissions.push or .permissions.maintain or .permissions.admin) | .login] as $w
		| ($w | map(select(. != $op))) + (if any($w[]; . == $op) then [] else ["(not " + $op + ")"] end)
		| join(", ")' <<<"$GH_BODY")" || {
		HQ_WRITERS="the collaborators list isn't JSON"
		return 2
	}
	[ -z "$others" ] && return 0
	HQ_WRITERS="$others"
	return 1
}

# check_head <number> <sha> — 0 when the run must stop (HQ_STOP_WHY says why).
check_head() {
	local st head
	gh_api GET "repos/$HQ_REPO/pulls/$1" || return 1
	read -r st head <<<"$(jq -r '"\(.state) \(.head.sha)"' <<<"$GH_BODY")"
	if [ "$st" != open ]; then
		HQ_STOP_WHY="superseded: PR #$1 closed"
		return 0
	fi
	if [ "$head" != "$2" ]; then
		HQ_STOP_WHY="superseded by ${head:0:7}"
		return 0
	fi
	return 1
}

# --- The instance ---------------------------------------------------------------

# ensure_bridge — the incus-bridge compose service is up and its socket there.
ensure_bridge() {
	local n
	[ "$HQ_BRIDGE" = 1 ] || return 0
	if [ -S "$HQ_INCUS_SOCKET" ] &&
		[ -n "$(docker ps -q --filter "label=com.docker.compose.project=$HQ_BRIDGE_PROJECT" --filter status=running)" ]; then
		return 0
	fi
	[ -n "$HQ_BRIDGE_HOST_ADDR" ] || {
		HQ_FAULT="HQ_BRIDGE_HOST_ADDR is unset (rerun setup.sh)"
		return 1
	}
	mkdir -p "$(dirname "$HQ_INCUS_SOCKET")"
	log "starting incus-bridge -> $HQ_BRIDGE_HOST_ADDR"
	if ! INCUS_BRIDGE_HOST_ADDR="$HQ_BRIDGE_HOST_ADDR" \
		INCUS_BRIDGE_SERVER_NAME="$HQ_BRIDGE_SERVER_NAME" \
		INCUS_BRIDGE_SERVER_CERT="/certs/servercerts/$HQ_INCUS_REMOTE.crt" \
		INCUS_BRIDGE_CONFIG_DIR="$HOME/.config/incus" \
		INCUS_BRIDGE_SOCKET_DIR="$(dirname "$HQ_INCUS_SOCKET")" \
		docker compose -p "$HQ_BRIDGE_PROJECT" -f "$HQ_CODE_ROOT/docker-compose.yml" \
		--profile incus-bridge up -d --build incus-bridge >&2; then
		HQ_FAULT="incus-bridge did not start"
		return 1
	fi
	for n in $(seq 1 30); do
		[ ! -S "$HQ_INCUS_SOCKET" ] || return 0
		sleep 1
	done
	HQ_FAULT="incus-bridge started but $HQ_INCUS_SOCKET never appeared"
	return 1
}

# checkout <number> <sha> <worktree> — the persistent clone, fetched, and a
# detached worktree at exactly <sha>.
checkout() {
	local number="$1" sha="$2" wt="$3"
	if [ ! -d "$HQ_CLONE/.git" ]; then
		rm -rf "$HQ_CLONE"
		git clone --quiet --no-checkout "$HQ_CLONE_URL" "$HQ_CLONE" >&2 || return 1
	fi
	git -C "$HQ_CLONE" fetch --quiet --no-tags origin \
		"+refs/heads/main:refs/remotes/origin/main" "+refs/pull/$number/head:refs/hq/pr" >&2 || return 1
	git -C "$HQ_CLONE" cat-file -e "$sha^{commit}" 2>/dev/null ||
		git -C "$HQ_CLONE" fetch --quiet --no-tags origin "$sha" >&2 || return 1
	git -C "$HQ_CLONE" worktree add --quiet --detach "$wt" "$sha" >&2 || return 1
	[ "$(git -C "$wt" rev-parse HEAD)" = "$sha" ]
}

# drop_run <state dir> — its worktree and everything else it left here.
drop_run() {
	local state="$1"
	[ -d "$HQ_CLONE/.git" ] && git -C "$HQ_CLONE" worktree remove --force "$state/wt" >/dev/null 2>&1
	rm -rf "$state" 2>/dev/null || log "WARNING: could not remove all of $state"
	[ -d "$HQ_CLONE/.git" ] && git -C "$HQ_CLONE" worktree prune >/dev/null 2>&1
	return 0
}

prune_logs() {
	local f n=0
	while read -r f; do
		n=$((n + 1))
		[ "$n" -le "$HQ_LOG_KEEP" ] || rm -f "$f"
	done < <(ls -1t "$HQ_LOGS"/*.log 2>/dev/null)
}

# --- The run ------------------------------------------------------------------

# outcome <rc> <output file> <minutes> — "<state> <description>" for run.sh's
# exit code, from its summary block.
outcome() {
	local rc="$1" out="$2" min="$3" summary n names skips
	summary="$(awk '/^======== summary ========$/ { s = 1; buf = ""; next } s { buf = buf $0 "\n" } END { printf "%s", buf }' "$out")"
	n="$(grep -c '\.sh ' <<<"$summary")"
	case "$rc" in
	0) echo "success passed: $n scripts ($HQ_GROUPS on $HQ_BACKEND) in ${min}m" ;;
	3)
		skips="$(grep -c '^SKIP: ' "$out")"
		echo "success passed: $n scripts, $skips allow-listed skip(s) [$HQ_ALLOW_SKIP] in ${min}m"
		;;
	1)
		names="$(awk '$2 == "FAILED" || $2 == "exit" { printf "%s%s", sep, $1; sep = ", " }' <<<"$summary")"
		echo "failure failed: ${names:-see the run log on the instance} (${min}m)"
		;;
	2)
		names="$(awk '$2 == "prereqs" { printf "%s%s", sep, $1; sep = ", " }' <<<"$summary")"
		echo "error run.sh exit 2, prerequisites unmet${names:+: $names}; says nothing about the PR"
		;;
	*) echo "error run.sh exit $rc; says nothing about the PR" ;;
	esac
}

# stop_group <pgid> — TERM, so the lock is released and the scripts' cleanup
# traps run; KILL after HQ_KILL_GRACE.
stop_group() {
	local n=0
	kill -TERM -- "-$1" 2>/dev/null
	while kill -0 "$1" 2>/dev/null && [ "$n" -lt "$HQ_KILL_GRACE" ]; do
		sleep 1
		n=$((n + 1))
	done
	kill -KILL -- "-$1" 2>/dev/null
}

# run_pr <number> <sha> <why>
run_pr() {
	local number="$1" sha="$2" why="$3"
	local state="$HQ_RUNS/$sha" wt log offset pid rc started last_watch stop="" posted="" min result
	wt="$state/wt"
	log="$HQ_LOGS/$sha.log"
	HQ_FAULT=""
	drop_run "$state"
	mkdir -p "$state/tmp" "$HQ_LOGS"
	touch "$log"
	offset="$(stat -c %s "$log")"
	printf '==== %s PR #%s %s (%s)\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$number" "$sha" "$why" >>"$log"
	log "PR #$number ${sha:0:12} ($why): starting"

	if ! checkout "$number" "$sha" "$wt" 2>>"$log"; then
		final_status "$sha" error "job fault: could not check out ${sha:0:7}; says nothing about the PR"
		drop_run "$state"
		return
	fi
	if ! ensure_bridge 2>>"$log"; then
		final_status "$sha" error "job fault: $HQ_FAULT; says nothing about the PR"
		drop_run "$state"
		return
	fi

	# A fresh process with only these variables: never the token. TMPDIR keeps
	# the scripts' scratch (multi-GB images) inside the run's directory, so a
	# stopped run's leftovers go with it.
	(
		cd "$wt" || exit 70
		exec setsid env -i \
			PATH="$PATH" HOME="$HOME" USER="${USER:-$(id -un)}" LOGNAME="${USER:-$(id -un)}" \
			LANG=C.UTF-8 TERM=dumb TMPDIR="$state/tmp" HQ_ETC="$HQ_ETC" \
			VALIDATE_INCUS_REMOTE="$HQ_INCUS_REMOTE" VALIDATE_HARDWARE_LOCK_SHA="$sha" \
			bash "$HQ_LOCK_LIB" hold -- "$HQ_SELF" _locked "$state" "$wt"
	) >>"$log" 2>&1 </dev/null 9>&- &
	pid=$!
	HQ_CHILD=$pid
	started=$SECONDS
	last_watch=$SECONDS
	while kill -0 "$pid" 2>/dev/null; do
		if [ -z "$posted" ] && [ -e "$state/locked" ]; then
			posted=1
			post_status "$sha" pending "running $HQ_GROUPS on $HQ_BACKEND since $(date -u +%H:%MZ)"
		fi
		if [ $((SECONDS - started)) -ge "$HQ_TIMEOUT" ]; then
			stop="timed out after $(((SECONDS - started) / 60))m; says nothing about the PR"
		elif [ $((SECONDS - last_watch)) -ge "$HQ_WATCH_INTERVAL" ]; then
			last_watch=$SECONDS
			check_head "$number" "$sha" && stop="$HQ_STOP_WHY"
		fi
		if [ -n "$stop" ]; then
			log "PR #$number ${sha:0:12}: stopping the run: $stop"
			echo "==== stopping: $stop" >>"$log"
			stop_group "$pid"
			break
		fi
		sleep 1
	done
	# Whatever is left in the group (the heartbeat, a background incus exec).
	kill -KILL -- "-$pid" 2>/dev/null
	wait "$pid"
	rc=$?
	HQ_CHILD=""
	min=$(((SECONDS - started + 59) / 60))
	tail -c +"$((offset + 1))" "$log" >"$state/out" 2>/dev/null

	if [ -n "$stop" ]; then
		final_status "$sha" error "$stop"
	elif [ ! -e "$state/locked" ]; then
		if [ "$rc" = 2 ]; then
			log "PR #$number ${sha:0:12}: the hardware lock is held or Incus is unreachable; it stays queued: $(grep 'hardware lock' "$state/out" | tail -n 1)"
		else
			final_status "$sha" error "job fault: the lock helper exited $rc before running anything; says nothing about the PR"
		fi
	elif [ -s "$state/fault" ]; then
		final_status "$sha" error "job fault: $(head -n 1 "$state/fault"); says nothing about the PR"
	elif [ -s "$state/suite-rc" ]; then
		result="$(outcome "$(cat "$state/suite-rc")" "$state/out" "$min")"
		final_status "$sha" "${result%% *}" "${result#* }"
	else
		final_status "$sha" error "job fault: the run ended (exit $rc) without a result; says nothing about the PR"
	fi
	log "PR #$number ${sha:0:12}: done after ${min}m (exit $rc), log $log"
	echo "==== done after ${min}m (exit $rc)" >>"$log"
	drop_run "$state"
	prune_logs
}

# --- The sweep ----------------------------------------------------------------

_incus() {
	timeout 300 incus "$@" 2>"$HQ_TMP/incus.err"
}

incus_err() {
	tr '\n' ' ' <"$HQ_TMP/incus.err" | sed 's/ *$//' | head -c 200
}

# sweep — delete every instance and custom volume named like a validate
# script's, and every valwan-<pid> network, in HQ_INCUS_PROJECT on
# HQ_INCUS_REMOTE, and every image whose aliases all have that shape (the
# scripts import their own and alias them by it; an image that also carries
# another alias, such as a pinned one, is kept). Never profiles, other images,
# nor HQ_VM_NAME. Every
# match goes, whatever its age: a PID means nothing across runs, and an
# interrupted run's leftovers hold home-lan's fixed addresses. 1 on any
# failure, with HQ_SWEEP_ERR saying which.
sweep() {
	local r="$HQ_INCUS_REMOTE" p="$HQ_INCUS_PROJECT" out name pool n=0
	HQ_SWEEP_ERR=""
	out="$(_incus list "$r:" --project "$p" -f json)" || {
		HQ_SWEEP_ERR="listing instances: $(incus_err)"
		return 1
	}
	for name in $(jq -r --arg vm "$HQ_VM_NAME" --arg re "$HQ_SWEEP_NAME_RE" \
		'.[].name | select(test($re)) | select(. != $vm)' <<<"$out"); do
		echo "sweep: deleting instance $name"
		_incus delete --force --project "$p" "$r:$name" >/dev/null || {
			HQ_SWEEP_ERR="deleting instance $name: $(incus_err)"
			return 1
		}
		n=$((n + 1))
	done
	out="$(_incus image list "$r:" --project "$p" -f json)" || {
		HQ_SWEEP_ERR="listing images: $(incus_err)"
		return 1
	}
	for name in $(jq -r --arg re "$HQ_SWEEP_NAME_RE" \
		'.[] | select((.aliases | length) > 0 and all(.aliases[]; .name | test($re))) | .fingerprint | select(test("^[0-9a-f]+$"))' <<<"$out"); do
		echo "sweep: deleting image ${name:0:12}"
		_incus image delete --project "$p" "$r:$name" >/dev/null || {
			HQ_SWEEP_ERR="deleting image ${name:0:12}: $(incus_err)"
			return 1
		}
		n=$((n + 1))
	done
	out="$(_incus storage list "$r:" -f json)" || {
		HQ_SWEEP_ERR="listing storage pools: $(incus_err)"
		return 1
	}
	for pool in $(jq -r '.[].name | select(test("^[A-Za-z0-9_.-]+$"))' <<<"$out"); do
		out="$(_incus storage volume list "$r:$pool" --project "$p" -f json)" || {
			HQ_SWEEP_ERR="listing volumes in $pool: $(incus_err)"
			return 1
		}
		for name in $(jq -r --arg re "$HQ_SWEEP_NAME_RE" \
			'.[] | select(.type == "custom") | .name | select(test($re))' <<<"$out"); do
			echo "sweep: deleting volume $pool/$name"
			_incus storage volume delete "$r:$pool" "$name" --project "$p" >/dev/null || {
				HQ_SWEEP_ERR="deleting volume $pool/$name: $(incus_err)"
				return 1
			}
			n=$((n + 1))
		done
	done
	out="$(_incus network list "$r:" --project "$p" -f json)" || {
		HQ_SWEEP_ERR="listing networks: $(incus_err)"
		return 1
	}
	for name in $(jq -r --arg re "$HQ_SWEEP_NET_RE" '.[] | select(.managed) | .name | select(test($re))' <<<"$out"); do
		echo "sweep: deleting network $name"
		_incus network delete "$r:$name" --project "$p" >/dev/null || {
			HQ_SWEEP_ERR="deleting network $name: $(incus_err)"
			return 1
		}
		n=$((n + 1))
	done
	echo "sweep: removed $n object(s) from $r ($p)"
}

# sweep_docker — on this VM only: every container but the bridge's, which a
# stopped run's compose stack could leave holding ports.
sweep_docker() {
	local id keep
	[ "$HQ_DOCKER_SWEEP" = 1 ] || return 0
	keep=" $(docker ps -aq --filter "label=com.docker.compose.project=$HQ_BRIDGE_PROJECT" | tr '\n' ' ') "
	for id in $(docker ps -aq); do
		case "$keep" in *" $id "*) continue ;; esac
		echo "sweep: removing container $id"
		docker rm -f "$id" >/dev/null || return 1
	done
	docker network prune -f >/dev/null 2>&1
	return 0
}

# prune_incusos_cache <worktree> — keep the cached IncusOS images this PR and
# main pin; remove the rest, and any fetch an interrupted run left.
prune_incusos_cache() {
	local cache="$HQ_CLONE/bootstrap-output/incusos" pr main d
	[ -d "$cache" ] || return 0
	pr="$(cat "$1/scripts/incusos-base.version" 2>/dev/null)"
	main="$(git -C "$HQ_CLONE" show origin/main:scripts/incusos-base.version 2>/dev/null)"
	for d in "$cache"/* "$cache"/.fetch.*; do
		[ -d "$d" ] || continue
		case "${d##*/}" in "$pr" | "$main") continue ;; esac
		rm -rf "$d" && echo "pruned cached IncusOS ${d##*/}"
	done
}

# _locked <state dir> <worktree> — the run, under the lock (`hold` starts it).
locked_main() {
	local state="$1" wt="$2" rc
	: >"$state/locked"
	echo "== sweep (lock ${VALIDATE_HARDWARE_LOCK_HELD:-?})"
	if ! sweep; then
		echo "sweep failed: $HQ_SWEEP_ERR" | tee "$state/fault"
		exit 70
	fi
	if ! sweep_docker; then
		echo "sweeping this VM's containers failed" | tee "$state/fault"
		exit 70
	fi
	cd "$wt" || exit 70
	echo "== make incusos-base"
	if ! make incusos-base; then
		echo "make incusos-base failed" | tee "$state/fault"
		exit 70
	fi
	prune_incusos_cache "$wt"
	echo "== VALIDATE_OS=$HQ_BACKEND run.sh --group $HQ_GROUPS --strict --allow-skip $HQ_ALLOW_SKIP"
	if [ "$HQ_BRIDGE" = 1 ]; then
		export VALIDATE_INCUS_SOCKET="$HQ_INCUS_SOCKET"
	fi
	VALIDATE_OS="$HQ_BACKEND" ./scripts/validate/run.sh \
		--group "$HQ_GROUPS" --strict --allow-skip "$HQ_ALLOW_SKIP"
	rc=$?
	echo "$rc" >"$state/suite-rc"
	exit "$rc"
}

# --- The loop -------------------------------------------------------------------

tick_once() {
	local next number sha why
	HQ_TOKEN="$(cat "$HQ_TOKEN_FILE" 2>/dev/null)"
	if [ -z "$HQ_TOKEN" ]; then
		log "ERROR: no token in $HQ_TOKEN_FILE (rerun setup.sh)"
		return 1
	fi
	# Holding the job lock, anything under runs/ is a dead run's.
	for next in "$HQ_RUNS"/*; do
		[ -d "$next" ] && drop_run "$next"
	done
	pick || return 0
	[ -n "$PICKED" ] || return 0
	read -r number sha why <<<"$PICKED"
	writers
	case $? in
	0) ;;
	1)
		log "ERROR: write access is not $HQ_OPERATOR's alone ($HQ_WRITERS); running nothing"
		final_status "$sha" error "refused: write access is not $HQ_OPERATOR's alone ($HQ_WRITERS); nothing ran"
		return 0
		;;
	*)
		log "WARNING: could not check the collaborators ($HQ_WRITERS); running nothing this pass"
		return 0
		;;
	esac
	run_pr "$number" "$sha" "$why"
}

# with_job_lock <cmd> — one job process per instance: a second tick or serve
# exits rather than run beside it.
with_job_lock() {
	mkdir -p "$HQ_STATE_DIR" "$HQ_RUNS" "$HQ_LOGS" || exit 1
	exec 9>"$HQ_STATE_DIR/job.lock"
	if ! flock -n 9; then
		log "another job process holds $HQ_STATE_DIR/job.lock; exiting"
		exit 0
	fi
	"$@"
}

serve() {
	log "serving: $HQ_REPO PRs into main by $HQ_AUTHORS; $HQ_GROUPS on $HQ_BACKEND, allowing $HQ_ALLOW_SKIP; polling every ${HQ_POLL_INTERVAL}s"
	while :; do
		tick_once
		# In the background, so TERM's trap runs at once, not after the sleep.
		sleep "$HQ_POLL_INTERVAL" &
		wait $!
	done
}

main() {
	check_config || exit 2
	HQ_TMP="$(mktemp -d)"
	HQ_CHILD=""
	# A job stopped mid-run (systemctl, Ctrl-C) stops its run too; the head
	# keeps its pending, which the next pass reruns.
	trap '[ -z "$HQ_CHILD" ] || stop_group "$HQ_CHILD"; rm -rf "$HQ_TMP"' EXIT
	trap 'exit 143' TERM
	trap 'exit 130' INT
	case "${1:-}" in
	serve) with_job_lock serve ;;
	tick) with_job_lock tick_once ;;
	sweep)
		sweep || {
			echo "ERROR: $HQ_SWEEP_ERR" >&2
			exit 1
		}
		;;
	_locked)
		shift
		locked_main "$@"
		;;
	*)
		sed -n '9,13p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
		exit 2
		;;
	esac
}

main "$@"
