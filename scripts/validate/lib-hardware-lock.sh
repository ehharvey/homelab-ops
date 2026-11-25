#! /bin/bash
# The hardware lock (#665): one run of the `incus`/`incus-vm` validate scripts
# at a time on the Incus server they share. Source it (run.sh does), or run it:
#
#   lib-hardware-lock.sh hold [--] <cmd> [args...]
#       Take the lock, run <cmd> holding it, release it, exit with <cmd>'s
#       code. <cmd> sees VALIDATE_HARDWARE_LOCK_HELD=<generation>, which tells
#       a run.sh it starts that its caller holds the lock. Exits 2, running
#       nothing, when the lock is held (naming the holder), the remote can't be
#       reached, or Incus errors (a permission error included). For #664's job, which holds it across its
#       cleanup sweep and its run.
#   lib-hardware-lock.sh status
#       The lock's current generation and holder.
#
# Why on the Incus server: the scripts share one server, `homelab-host`, and the
# `incus-vm` ones its `home-lan` bridge (docs/Decisions.md §20). People run from
# a devcontainer and #664's job runs in a VM, so a host-side flock (§20's
# assumption) reaches neither; the server is the one thing both can reach.
#
# How (docs/Decisions.md §25, "Incus profile-create arbiter"):
#
# - Each generation is a profile `<base>-<n>` in the `default` project. Base
#   `validate-hardware-lock`, or VALIDATE_HARDWARE_LOCK_NAME. Creating a name
#   that exists fails on a database uniqueness constraint, so of several
#   concurrent creators exactly one wins. Not ETag/If-Match: §25 measured those
#   writes landing for several writers holding one ETag.
# - The lock is the highest generation, parsed as a number, never sorted as a
#   string. It is held unless that profile has user.released=true or its
#   heartbeat is stale.
# - Acquiring and taking over are one operation: create highest+1. Never
#   delete-then-create, and never delete the highest generation: either would
#   let a fresh acquirer and a waiter taking over both win, each at a
#   different generation.
# - A losing create comes back as a raw 500 ("The profile already exists"),
#   not a clean conflict, so a loss is confirmed by re-reading. A failed create
#   whose generation is there with this process's user.pid/host/since landed
#   anyway (a timeout after the commit): that is a win. A failed create with no
#   generation at or above it visible is an Incus error.
# - The winner re-lists. If a higher generation exists (its create was stale:
#   pruning had already removed the name it took), it deletes its own. If the
#   re-list fails it can't tell, so it marks its own released and errors.
#   Otherwise it prunes every generation below n-1, so at most two lock
#   profiles exist.
# - A loser, or a winner that deferred, looks at the lock again rather than
#   refusing outright: the generation it lost to may already be released.
# - Signals are held back from just before a create until its outcome is
#   recorded, so release always knows whether this process holds a generation.
# - Release sets user.released=true on the holder's own generation.
# - Liveness: the holder bumps user.heartbeat every VALIDATE_HARDWARE_LOCK_HEARTBEAT
#   seconds (default 30). A waiter calls the holder dead when that value hasn't
#   changed for VALIDATE_HARDWARE_LOCK_STALE seconds (default 300) on the
#   waiter's own clock, so no two clocks are compared. A value that changes
#   while it watches means a live holder: it refuses. So a refusal takes up to one
#   heartbeat interval plus a poll (a fifth of it), about 36s at the defaults,
#   plus however long the remote takes to answer (each call is cut off at
#   VALIDATE_HARDWARE_LOCK_CALL_TIMEOUT, default 20s); a takeover takes the
#   full stale period. The stale period must exceed 2×heartbeat + 4×timeout
#   (hwlock_check_config says why), or a waiter could take over a live holder.
# - Once a waiter has listed the lock, losing the remote is a refusal, not
#   "unreachable": the holder it saw may be live. Only a first listing that
#   fails (a timeout or a reset retried twice) reads as unreachable.
# - Release runs on normal exit and on INT/TERM/HUP. It stops the heartbeat
#   and waits for it to end before its own PATCH: Incus merges a PATCH into
#   the config it read earlier, so a heartbeat PATCH in flight could otherwise
#   land after release's and drop user.released. SIGKILL or a host reboot
#   falls through to the heartbeat rule; the heartbeat loop exits once the
#   process that took the lock is gone (by pid and, where /proc has it, start
#   time, so a reused pid doesn't keep a dead holder's lock beating).
#
# The holder is in the profile's user.* keys: user.who, user.host, user.where
# (working directory), user.pid, user.since, user.sha (VALIDATE_HARDWARE_LOCK_SHA,
# else this checkout's HEAD; #664's job passes the PR's SHA), user.command,
# user.heartbeat, user.released. See them with
# `incus profile list <remote>: --project default` and
# `incus profile show <remote>:<base>-<n> --project default`.
#
# Assumes the server has one database member. §25's exactly-one-creator
# evidence is from an unclustered server; this suite's proof
# (hardware-lock-admits-one-holder-and-recovers-from-a-dead-one.sh) runs
# against homelab-host, which is a one-member cluster. Neither covers a
# cluster of several members.
#
# Test hook, for that proof only: with _HWLOCK_TEST_BARRIER set to a directory,
# each acquirer touches arrived.<pid> there just before its create, then waits
# (up to 120s) for a file named go, so several creates race for real.

HWLOCK_REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
HWLOCK_PROJECT=default
HWLOCK_BASE="${VALIDATE_HARDWARE_LOCK_NAME:-validate-hardware-lock}"
HWLOCK_HEARTBEAT_S="${VALIDATE_HARDWARE_LOCK_HEARTBEAT:-30}"
HWLOCK_STALE_S="${VALIDATE_HARDWARE_LOCK_STALE:-300}"
# Incus caps profile names at 64 characters (§25: 64 accepted, 65 rejected).
# The base must leave room for "-" and a ten-digit generation; reaching ten
# digits is a hard stop, not a wrap.
HWLOCK_NAME_MAX=64
HWLOCK_GEN_DIGITS=10
HWLOCK_GEN_MAX=9999999999
# Each Incus call is bounded, so an unreachable remote is a note, not a hang.
HWLOCK_CALL_TIMEOUT_S="${VALIDATE_HARDWARE_LOCK_CALL_TIMEOUT:-20}"

HWLOCK_COMMAND="${HWLOCK_COMMAND:-$0}"
HWLOCK_NAME=""   # the generation this process holds, once acquired
HWLOCK_HB_PID="" # its heartbeat loop
HWLOCK_HB_START="" # and that loop's start time, from /proc (empty without it)
HWLOCK_HOLDER="" # who holds it, when hwlock_acquire refuses
HWLOCK_ERROR=""  # why, when it returns 2 or 3
HWLOCK_GENS="[]" # the last listing: [{gen, name, config}], sorted by gen

_HWLOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# hwlock_check_config — 0, or prints why the lock's settings are unusable and
# returns 2 (a usage error: run.sh exits 2 on it).
hwlock_check_config() {
	local room=$((HWLOCK_NAME_MAX - 1 - HWLOCK_GEN_DIGITS)) v
	if ! [[ "$HWLOCK_BASE" =~ ^[A-Za-z0-9][A-Za-z0-9-]*$ ]]; then
		echo "ERROR: VALIDATE_HARDWARE_LOCK_NAME=$HWLOCK_BASE: use letters, digits and '-', starting with a letter or digit" >&2
		return 2
	fi
	if [ "${#HWLOCK_BASE}" -gt "$room" ]; then
		echo "ERROR: VALIDATE_HARDWARE_LOCK_NAME=$HWLOCK_BASE is ${#HWLOCK_BASE} characters; at most $room leaves room for a $HWLOCK_GEN_DIGITS-digit generation within Incus's $HWLOCK_NAME_MAX-character profile names" >&2
		return 2
	fi
	for v in HWLOCK_HEARTBEAT_S HWLOCK_STALE_S HWLOCK_CALL_TIMEOUT_S; do
		if ! [[ "${!v}" =~ ^[1-9][0-9]{0,5}$ ]]; then
			echo "ERROR: VALIDATE_HARDWARE_LOCK_HEARTBEAT, _STALE and _CALL_TIMEOUT take whole seconds (got $v=${!v})" >&2
			return 2
		fi
	done
	# A live holder's heartbeat lands at most heartbeat + timeout after the
	# one a waiter read (a sleep, then a PATCH cut off at the timeout). The
	# waiter's first listing to read it starts within a poll and two calls of
	# it landing, and returns within one more: heartbeat + poll + 4×timeout,
	# and poll is at most the heartbeat. A stale period no longer than that
	# could see a live holder's heartbeat stand still.
	v=$((2 * HWLOCK_HEARTBEAT_S + 4 * HWLOCK_CALL_TIMEOUT_S))
	if [ "$HWLOCK_STALE_S" -le "$v" ]; then
		echo "ERROR: VALIDATE_HARDWARE_LOCK_STALE=$HWLOCK_STALE_S must exceed 2×heartbeat + 4×call timeout ($HWLOCK_HEARTBEAT_S, $HWLOCK_CALL_TIMEOUT_S: $v), or a waiter could take over a live holder" >&2
		return 2
	fi
	return 0
}

_hwlock_query() {
	timeout "$HWLOCK_CALL_TIMEOUT_S" incus query "$@"
}

_hwlock_url() {
	echo "$HWLOCK_REMOTE:/1.0/profiles$1?project=$HWLOCK_PROJECT"
}

# _hwlock_list — refresh HWLOCK_GENS from the server. On failure HWLOCK_ERROR
# says why, and the code says what kind: 3 when the remote can't be reached (no
# such remote, a connection-level error, or the call timing out); 2 for anything
# else, such as a permission or API error or a reply that isn't a listing. So a
# client that reaches the server but can't see the lock (a certificate
# restricted to another project, say) is refused rather than run without it.
# _HWLOCK_TRANSIENT=1 marks a 3 worth retrying: a timeout or a reset.
# Only `<base>-<n>` with n a plain positive number of at most ten digits counts.
_hwlock_list() {
	local raw gens rc
	_HWLOCK_TRANSIENT=0
	raw="$(_hwlock_query "$(_hwlock_url "")&recursion=1" 2>&1)"
	rc=$?
	if [ "$rc" -ne 0 ]; then
		# Its Error: line; some errors (no such remote) come after usage text.
		raw="$(grep -m 1 '^Error: ' <<<"$raw" || head -n 1 <<<"$raw")"
		if [ "$rc" -eq 124 ] || grep -qiE "connection reset|i/o timeout|tls handshake timeout|context deadline exceeded" <<<"$raw"; then
			_HWLOCK_TRANSIENT=1
		fi
		if [ "$_HWLOCK_TRANSIENT" = 1 ] || grep -qiE "doesn't exist|unable to connect|dial tcp|connection refused|connection reset|no route to host|network is unreachable|no such host|i/o timeout|tls handshake timeout|context deadline exceeded" <<<"$raw"; then
			HWLOCK_ERROR="cannot reach Incus remote '$HWLOCK_REMOTE' (${raw:-timed out after ${HWLOCK_CALL_TIMEOUT_S}s})"
			return 3
		fi
		HWLOCK_ERROR="cannot list profiles on Incus remote '$HWLOCK_REMOTE' (${raw:-exit $rc, no error text})"
		return 2
	fi
	if ! gens="$(jq -c --arg p "$HWLOCK_BASE-" '
		[ .[] | select(.name | startswith($p))
		  | (.name | ltrimstr($p)) as $n
		  | select($n | test("^[1-9][0-9]{0,9}$"))
		  | {gen: ($n | tonumber), name, config: (.config // {})} ]
		| sort_by(.gen)' <<<"$raw" 2>/dev/null)"; then
		HWLOCK_ERROR="listing profiles on Incus remote '$HWLOCK_REMOTE' returned something that isn't a profile list"
		return 2
	fi
	HWLOCK_GENS="$gens"
}

# _hwlock_top [field] — the highest generation in the last listing, as JSON, or
# one of its fields (gen, name, released, heartbeat). Empty when there is none.
_hwlock_top() {
	jq -r --arg f "${1:-}" '
		(.[-1] // empty)
		| if $f == "" then tojson
		  elif $f == "released" then (.config["user.released"] // "")
		  elif $f == "heartbeat" then (.config["user.heartbeat"] // "")
		  else .[$f] end' <<<"$HWLOCK_GENS"
}

# _hwlock_describe <profile json> — one line naming the holder.
_hwlock_describe() {
	jq -r '
		def k($x): .config["user." + $x] // "?";
		"\(.name) by \(k("who"))@\(k("host")) pid \(k("pid")) since \(k("since")), in \(k("where")) at \(k("sha")), running: \(k("command"))"' <<<"$1"
}

_hwlock_patch() {
	_hwlock_query -X PATCH "$(_hwlock_url "/$1")" --data "$2" >/dev/null 2>&1
}

_hwlock_delete() {
	_hwlock_query -X DELETE "$(_hwlock_url "/$1")" >/dev/null 2>&1
}

_hwlock_barrier() {
	local dir="${_HWLOCK_TEST_BARRIER:-}" waited=0
	[ -n "$dir" ] || return 0
	touch "$dir/arrived.$BASHPID"
	while [ ! -e "$dir/go" ] && [ "$waited" -lt 600 ]; do
		sleep 0.2
		waited=$((waited + 1))
	done
}

# _hwlock_proc <pid> — "<state> <start time>" (fields 3 and 22 of
# /proc/<pid>/stat), or a failure when there's no such process or no /proc.
_hwlock_proc() {
	local s f
	s="$(cat "/proc/$1/stat" 2>/dev/null)" || return 1
	# Fields after the parenthesised command name, which may hold spaces.
	read -ra f <<<"${s##*) }"
	echo "${f[0]} ${f[19]}"
}

# _hwlock_running <pid> <start> — <pid> is still the process that started at
# <start>, and not a zombie. With <start> empty (no /proc when it was
# recorded), only that some process has <pid>.
# _hwlock_start <pid> — its start time, or nothing without /proc.
_hwlock_start() {
	local p
	p="$(_hwlock_proc "$1")" && echo "${p#* }"
}

_hwlock_running() {
	local p
	if [ -z "$2" ]; then
		kill -0 "$1" 2>/dev/null
		return
	fi
	p="$(_hwlock_proc "$1")" || return 1
	[ "${p%% *}" != Z ] && [ "${p#* }" = "$2" ]
}

# _hwlock_heartbeat <owner pid> <owner start> <name> — runs detached until the
# owner is gone or hwlock_release sends it TERM. The owner check comes after
# the sleep and before the write, so a SIGKILLed owner's lock stops beating
# within one interval. TERM ends it between PATCHes, never during one: the
# sleep is a waited-on background job, which TERM interrupts, while a PATCH in
# the foreground runs to its end before the trap does.
_hwlock_heartbeat() {
	local owner="$1" start="$2" name="$3" i=0 stop=""
	trap 'stop=1' TERM
	while :; do
		sleep "$HWLOCK_HEARTBEAT_S" &
		wait "$!"
		if [ -n "$stop" ]; then
			kill "$!" 2>/dev/null
			exit 0
		fi
		_hwlock_running "$owner" "$start" || exit 0
		i=$((i + 1))
		_hwlock_patch "$name" "{\"config\":{\"user.heartbeat\":\"$i\"}}" || :
		[ -z "$stop" ] || exit 0
	done
}

# _hwlock_defer_signals / _hwlock_resume_signals — hold INT/TERM/HUP back
# across a create and the recording of its outcome, then re-raise one that
# arrived, so the caller's trap runs with HWLOCK_NAME naming what this process
# holds. Otherwise a signal landing just after a create that won would exit
# before HWLOCK_NAME named it: the EXIT trap's release would miss it, and every
# later caller would wait out the stale period. (Naming it before the create
# instead would make a loser's release mark the winner's generation released.)
_hwlock_defer_signals() {
	_HWLOCK_SAVED_TRAPS="$(trap -p INT TERM HUP)"
	_HWLOCK_PENDING=""
	trap '_HWLOCK_PENDING=INT' INT
	trap '_HWLOCK_PENDING=TERM' TERM
	trap '_HWLOCK_PENDING=HUP' HUP
}

_hwlock_resume_signals() {
	trap - INT TERM HUP
	eval "$_HWLOCK_SAVED_TRAPS"
	[ -z "$_HWLOCK_PENDING" ] || kill -s "$_HWLOCK_PENDING" "$BASHPID"
}

# _hwlock_take <gen> — create generation <gen>.
#   0  won: HWLOCK_NAME is ours, and its heartbeat is running
#   2  an Incus error: HWLOCK_ERROR says what
#   4  lost the create, or won a stale one and deferred: HWLOCK_GENS is fresh,
#      for hwlock_acquire to look again
_hwlock_take() {
	# The process holding the lock: read here, since inside $(...) BASHPID is
	# the substitution's own subshell.
	local gen="$1" owner="$BASHPID" name body host since rc
	if [ "$gen" -gt "$HWLOCK_GEN_MAX" ]; then
		HWLOCK_ERROR="generation $gen would pass $HWLOCK_GEN_MAX; delete the $HWLOCK_BASE-* profiles by hand once no run holds the lock"
		return 2
	fi
	name="$HWLOCK_BASE-$gen"
	host="${HOSTNAME:-$(uname -n)}"
	since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	body="$(jq -nc \
		--arg name "$name" \
		--arg who "${USER:-$(id -un 2>/dev/null || echo "?")}" \
		--arg host "$host" \
		--arg where "$PWD" \
		--arg pid "$owner" \
		--arg since "$since" \
		--arg sha "${VALIDATE_HARDWARE_LOCK_SHA:-$(git -C "$_HWLOCK_DIR" rev-parse HEAD 2>/dev/null || echo "?")}" \
		--arg command "$HWLOCK_COMMAND" \
		'{name: $name,
		  description: "validate hardware lock (#665): holder in user.*",
		  config: {"user.who": $who, "user.host": $host, "user.where": $where,
		           "user.pid": $pid, "user.since": $since, "user.sha": $sha,
		           "user.command": $command, "user.heartbeat": "0"}}')"
	_hwlock_barrier
	_hwlock_defer_signals
	_hwlock_create "$gen" "$name" "$body" "$owner" "$host" "$since"
	rc=$?
	_hwlock_resume_signals
	return "$rc"
}

# _hwlock_create <gen> <name> <body> <owner> <host> <since> — _hwlock_take's
# create, and recording what came of it; runs with signals held back.
_hwlock_create() {
	local gen="$1" name="$2" body="$3" owner="$4" host="$5" since="$6" err mine g top
	local rereads=0 orphan
	if ! err="$(_hwlock_query -X POST "$(_hwlock_url "")" --data "$body" 2>&1 >/dev/null)"; then
		# §25: the loser's error is a raw 500, so confirm by re-reading. A
		# create cut off (a timeout, or a Ctrl-C reaching incus) may still
		# land: if <gen> is there with this process's holder fields, it is
		# this process's win. It may land after the first re-read, so with
		# nothing at or above <gen> visible, re-read a few times before giving
		# up; a create landing later still would be a generation nobody owns.
		orphan="if it lands later, nobody owns it: free it with incus profile set $HWLOCK_REMOTE:$name user.released=true --project $HWLOCK_PROJECT"
		while :; do
			if ! _hwlock_list; then
				HWLOCK_ERROR="creating $name failed (${err:-no error text}), then $HWLOCK_ERROR; $orphan"
				return 2
			fi
			mine="$(jq -c --argjson n "$gen" --arg pid "$owner" --arg host "$host" --arg since "$since" '
				.[] | select(.gen == $n and .config["user.pid"] == $pid
				  and .config["user.host"] == $host and .config["user.since"] == $since)' <<<"$HWLOCK_GENS")"
			if [ -n "$mine" ]; then
				echo "hardware lock: creating $name reported a failure (${err:-no error text}), but it is there with this process as holder" >&2
				break
			elif g="$(_hwlock_top gen)" && [ -n "$g" ] && [ "$g" -ge "$gen" ]; then
				echo "hardware lock: lost the race for $name" >&2
				return 4
			elif [ "$rereads" -ge 3 ]; then
				HWLOCK_ERROR="creating $name failed and no holder of it is visible after $rereads re-reads (${err:-no error text}); $orphan"
				return 2
			fi
			rereads=$((rereads + 1))
			sleep 2
		done
	fi
	HWLOCK_NAME="$name"
	# Won the create. Re-list: a higher generation means this create was stale
	# (pruning had already removed the name it took), so defer to that one.
	# Without the re-list there's no telling, so don't claim the lock.
	if ! _hwlock_list; then
		err="$HWLOCK_ERROR"
		hwlock_release
		HWLOCK_ERROR="created $name, but could not re-list to check for a newer generation, so marked it released: $err"
		return 2
	fi
	g="$(_hwlock_top gen)"
	if [ -n "$g" ] && [ "$g" -gt "$gen" ]; then
		top="$(_hwlock_top)"
		_hwlock_delete "$name"
		HWLOCK_NAME=""
		echo "hardware lock: created $name but $(jq -r .name <<<"$top") is newer; deleted mine" >&2
		return 4
	fi
	# Detached (a subshell's background job), so run.sh's `wait` never waits on
	# it, and with its output closed, so it holds no caller's pipe open.
	HWLOCK_HB_PID="$( (_hwlock_heartbeat "$owner" "$(_hwlock_start "$owner")" "$name" </dev/null >/dev/null 2>&1 &
		echo "$!"))"
	HWLOCK_HB_START="$(_hwlock_start "$HWLOCK_HB_PID")"
	for g in $(jq -r --argjson n "$gen" '.[] | select(.gen < $n - 1) | .name' <<<"$HWLOCK_GENS"); do
		_hwlock_delete "$g"
	done
	echo "hardware lock: acquired $name on $HWLOCK_REMOTE" >&2
	return 0
}

# hwlock_acquire — take the lock, or find out who has it.
#   0  acquired: HWLOCK_NAME is ours, and its heartbeat is running
#   1  held by someone else: HWLOCK_HOLDER says who
#   2  bad settings or an Incus error: HWLOCK_ERROR says what
#   3  the remote can't be reached: HWLOCK_ERROR says so
# An unreleased highest generation is watched until its heartbeat moves (held)
# or has stood still for the stale period (dead: take over). A lost or deferred
# create looks again, so a released highest generation is still taken, and a
# live one is still watched before refusing. Only the first listing can come
# back 3; once one has succeeded, losing the remote is a 2, since the holder
# it showed may be live.
hwlock_acquire() {
	local poll name hb seen="" seen_hb="" seen_at=0 gen take tries=0 rc
	hwlock_check_config || {
		HWLOCK_ERROR="bad lock settings"
		return 2
	}
	poll=$((HWLOCK_HEARTBEAT_S / 5))
	[ "$poll" -ge 1 ] || poll=1
	# A timeout or a reset may be a busy server (another run booting VMs on
	# it, say), not a missing one: retry those before calling it unreachable.
	for tries in 1 2 3; do
		_hwlock_list
		rc=$?
		[ "$rc" -eq 3 ] && [ "$_HWLOCK_TRANSIENT" = 1 ] && [ "$tries" -lt 3 ] || break
		echo "hardware lock: $HWLOCK_ERROR; retrying" >&2
		sleep 2
	done
	[ "$rc" -eq 0 ] || return "$rc"
	tries=0
	while :; do
		gen="$(_hwlock_top gen)"
		take=""
		name="$(_hwlock_top name)"
		hb="$(_hwlock_top heartbeat)"
		if [ -z "$gen" ] || [ "$(_hwlock_top released)" = true ]; then
			take=$((${gen:-0} + 1))
		elif [ "$name" != "$seen" ]; then
			seen="$name" seen_hb="$hb" seen_at="$SECONDS"
			echo "hardware lock: $name is unreleased; watching its heartbeat (up to ${HWLOCK_STALE_S}s)" >&2
		elif [ "$hb" != "$seen_hb" ]; then
			HWLOCK_HOLDER="$(_hwlock_describe "$(_hwlock_top)")"
			return 1
		elif [ $((SECONDS - seen_at)) -ge "$HWLOCK_STALE_S" ]; then
			echo "hardware lock: $name's heartbeat stood still for ${HWLOCK_STALE_S}s; taking over" >&2
			take=$((gen + 1))
		fi
		if [ -n "$take" ]; then
			_hwlock_take "$take"
			rc=$?
			[ "$rc" -eq 4 ] || return "$rc"
			# Lost or deferred: HWLOCK_GENS is fresh, so look again now.
			tries=$((tries + 1))
			if [ "$tries" -ge 5 ]; then
				HWLOCK_HOLDER="$(_hwlock_describe "$(_hwlock_top)") (after $tries lost creates)"
				return 1
			fi
			continue
		fi
		sleep "$poll"
		if ! _hwlock_list; then
			HWLOCK_ERROR="lost the lock's listing after it was read, so refusing rather than run beside a holder that may be live: $HWLOCK_ERROR"
			return 2
		fi
	done
}

# hwlock_release — mark our generation released and stop its heartbeat. Safe to
# call when nothing is held, and more than once.
hwlock_release() {
	local n
	[ -n "$HWLOCK_NAME" ] || return 0
	if [ -n "$HWLOCK_HB_PID" ]; then
		# Wait for the heartbeat to end: Incus merges a PATCH into the config
		# it read earlier, so one still in flight could land after release's
		# and drop user.released. It ends within one call of TERM.
		kill "$HWLOCK_HB_PID" 2>/dev/null
		n=$(((HWLOCK_CALL_TIMEOUT_S + 2) * 10))
		while [ "$n" -gt 0 ] && _hwlock_running "$HWLOCK_HB_PID" "$HWLOCK_HB_START"; do
			sleep 0.1
			n=$((n - 1))
		done
	fi
	if _hwlock_patch "$HWLOCK_NAME" '{"config":{"user.released":"true"}}'; then
		echo "hardware lock: released $HWLOCK_NAME" >&2
	else
		echo "hardware lock: could not mark $HWLOCK_NAME released; it frees once its heartbeat has stood still for ${HWLOCK_STALE_S}s" >&2
	fi
	HWLOCK_NAME="" HWLOCK_HB_PID="" HWLOCK_HB_START=""
}

# hwlock_check_held <name> — for a caller that says it holds the lock
# (VALIDATE_HARDWARE_LOCK_HELD): 0 when <name> is the lock's current,
# unreleased generation; 1 when not (HWLOCK_ERROR says why); 2 on bad
# settings or an Incus error; 3 when the remote can't be reached.
hwlock_check_held() {
	local want="$1" name
	hwlock_check_config || return 2
	_hwlock_list || return
	name="$(_hwlock_top name)"
	if [ -z "$name" ]; then
		HWLOCK_ERROR="it names $want, but no $HWLOCK_BASE-<n> generation exists"
		return 1
	fi
	if [ "$name" != "$want" ]; then
		HWLOCK_ERROR="it names $want, but the lock is $(_hwlock_describe "$(_hwlock_top)")"
		return 1
	fi
	if [ "$(_hwlock_top released)" = true ]; then
		HWLOCK_ERROR="it names $want, which is released"
		return 1
	fi
	return 0
}

# hwlock_status — print the lock's state.
hwlock_status() {
	local rc
	hwlock_check_config || return 2
	_hwlock_list || {
		rc=$?
		echo "$HWLOCK_ERROR" >&2
		return "$rc"
	}
	if [ -z "$(_hwlock_top gen)" ]; then
		echo "free: no $HWLOCK_BASE-<n> profile on $HWLOCK_REMOTE"
	elif [ "$(_hwlock_top released)" = true ]; then
		echo "free: $(_hwlock_top name) is released"
	else
		echo "unreleased (held unless its heartbeat stands still for ${HWLOCK_STALE_S}s): $(_hwlock_describe "$(_hwlock_top)"), heartbeat $(_hwlock_top heartbeat)"
	fi
}

_hwlock_main() {
	local rc
	case "${1:-}" in
	hold)
		shift
		[ "${1:-}" != -- ] || shift
		if [ $# -eq 0 ]; then
			echo "usage: $(basename "$0") hold [--] <cmd> [args...]" >&2
			exit 2
		fi
		HWLOCK_COMMAND="$*"
		trap hwlock_release EXIT
		trap 'exit 130' INT
		trap 'exit 143' TERM
		trap 'exit 129' HUP
		hwlock_acquire
		rc=$?
		case "$rc" in
		0) ;;
		1)
			echo "hardware lock: held: $HWLOCK_HOLDER" >&2
			exit 2
			;;
		*)
			echo "ERROR: hardware lock: $HWLOCK_ERROR" >&2
			exit 2
			;;
		esac
		export VALIDATE_HARDWARE_LOCK_HELD="$HWLOCK_NAME"
		"$@"
		exit $?
		;;
	status)
		hwlock_status
		exit $?
		;;
	*)
		sed -n '2,15p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
		exit 2
		;;
	esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	set -uo pipefail
	_hwlock_main "$@"
fi
