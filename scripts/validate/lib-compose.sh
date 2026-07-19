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
compose() {
	if [ -n "${VALIDATE_COMPOSE_OVERRIDE:-}" ]; then
		docker compose -f "$ROOT_DIR/docker-compose.yml" -f "$VALIDATE_COMPOSE_OVERRIDE" "$@"
	else
		docker compose -f "$ROOT_DIR/docker-compose.yml" "$@"
	fi
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
