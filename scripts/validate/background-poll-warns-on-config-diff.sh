#! /bin/bash
# Validates the background config-sync poller surfaces diff warnings.
#
# Regression guard for a code-review fix: cmd/web's pollSync used to call
# syncer.Sync + store.Replace directly, silently skipping the
# diff-against-last-synced-state warning that POST /sync produces (issue #22).
# Both paths now go through server.SyncOnce, so a background poll logs the
# same added/changed/removed warnings a manual sync does.
#
# Unlike sync-warns-on-config-diff.sh, this script NEVER calls POST /sync: it sets
# CONFIG_SYNC_INTERVAL (via a compose override) and proves the warnings come
# from the poll loop alone. Drives the real `docker compose` stack (web + the
# dev/git-fixture config-repo).
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires
# docker compose.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="the background config-sync poller surfaces diff warnings without any POST /sync"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose curl"
VALIDATE_DURATION="~17s"

validate_parse_args "$@"
cd "$ROOT_DIR"

# A short poll interval keeps the test fast; the waits below tolerate clone
# and scheduling jitter well beyond one tick.
OVERRIDE="$(mktemp /tmp/compose-poll-override.XXXXXX.yml)"
# lib-compose.sh's compose() picks this up; scripts that need no scoped
# override simply leave it unset.
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    environment:
      - CONFIG_SYNC_INTERVAL=2s
EOF

cleanup() {
  compose_down
  rm -f "$OVERRIDE"
}
trap cleanup EXIT

echo "== 1. Bring up the real stack with background polling enabled =="
check "docker compose up --build succeeds" compose up --build -d

base_url="http://localhost:8080"
for _ in $(seq 1 20); do
  curl -s -o /dev/null "$base_url/healthz" && break
  sleep 0.5
done
check "web is reachable" curl -sf -o /dev/null "$base_url/healthz"

echo
echo "== 2. The first background poll establishes a baseline (no warnings yet) =="
# No POST /sync anywhere: this log line can only come from the poll loop.
wait_log "background poll logs the first sync as a baseline" \
  "configdiff: first sync, 1 networks / 1 instances baseline"

echo
echo "== 3. A pushed YAML change produces diff warnings on the next poll =="
docker compose -f "$ROOT_DIR/docker-compose.yml" -f "$OVERRIDE" exec -T config-repo sh -c '
  set -e
  rm -rf /tmp/validate-work
  git clone --no-hardlinks /srv/git/fleet.git /tmp/validate-work
  cd /tmp/validate-work
  git config user.email dev@homelab-ops.local
  git config user.name "background-poll-warns-on-config-diff"
  cat > fleet.yaml <<EOF
kind: Network
name: dev-lan
cidr: 10.0.1.0/24
gateway: 10.0.1.1
dhcp_excluded_range: 10.0.1.200-10.0.1.250
dns: [10.0.1.1]
---
kind: Network
name: extra-lan
cidr: 10.0.2.0/24
gateway: 10.0.2.1
dhcp_excluded_range: 10.0.2.200-10.0.2.250
dns: [10.0.2.1]
EOF
  git add fleet.yaml
  git commit -m "background-poll-warns-on-config-diff: change dev-lan, add extra-lan, drop devnode0" >/dev/null
  git push origin main >/dev/null 2>&1
' >/dev/null 2>&1
check "pushed a second commit to the fixture repo" \
  compose exec -T config-repo test -d /tmp/validate-work

# These warning lines are emitted only by SyncOnce's diff logging, reached
# here purely through the background poll loop.
wait_log "background poll warns about the added network" "+ network extra-lan added"
wait_log "background poll warns about the changed network" "~ network dev-lan changed"
wait_log "background poll warns about the removed instance" "- instance devnode0 removed"

echo
echo "== 4. The store reflects the polled change (no manual sync triggered) =="
status=$(curl -s "$base_url/status")
check "GET /status reports a sync happened via the poller alone" \
  bash -c "echo '$status' | grep -q '\"synced\":true'"

summary
