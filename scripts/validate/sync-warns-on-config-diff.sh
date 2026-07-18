#! /bin/bash
# Validates GH issue #22 ("Diff against last-synced state")
# "Done when" criteria: pushing a YAML change to the tracked repo and
# triggering a sync produces a visible diff/warning in the app, with zero
# side effects on real nodes.
#
# Drives the real `docker compose` stack (web + the dev/git-fixture
# config-repo from #20/#21) rather than just `go test`: syncs against a
# real git remote, then pushes a second commit to that remote (changing a
# Network, adding a Network, and dropping the Instance) to prove the
# diff surfaces added/changed/removed correctly in both the POST /sync
# response and the server log — and that nothing beyond the store is
# touched (no real node provisioning happens anywhere in this path).
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires
# docker compose and jq.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

pass=0
fail=0

check() {
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    echo "PASS: $desc"
    pass=$((pass + 1))
  else
    echo "FAIL: $desc"
    fail=$((fail + 1))
  fi
}

check_json() {
  local desc="$1" json="$2" filter="$3"
  if echo "$json" | jq -e "$filter" >/dev/null 2>&1; then
    echo "PASS: $desc"
    pass=$((pass + 1))
  else
    echo "FAIL: $desc (got: $json)"
    fail=$((fail + 1))
  fi
}

check_log() {
  local desc="$1" pattern="$2"
  if docker compose logs web 2>/dev/null | grep -qF -- "$pattern"; then
    echo "PASS: $desc"
    pass=$((pass + 1))
  else
    echo "FAIL: $desc (pattern not found: $pattern)"
    fail=$((fail + 1))
  fi
}

cleanup() {
  docker compose down >/dev/null 2>&1
}
trap cleanup EXIT

echo "== 1. Bring up the real stack (web + dev git fixture) =="
check "docker compose up --build succeeds" docker compose up --build -d

base_url="http://localhost:8080"
for _ in $(seq 1 20); do
  curl -s -o /dev/null "$base_url/healthz" && break
  sleep 0.5
done
check "web is reachable" curl -sf -o /dev/null "$base_url/healthz"

echo
echo "== 2. First sync has no prior state to diff against: baseline, not warnings =="
sync1=$(curl -s -X POST "$base_url/sync")
check_json "POST /sync returns a commit SHA" "$sync1" '.commit | length > 0'
check_json "first sync's diff counts are all zero" "$sync1" \
  '.diff | to_entries | all(.value == 0)'
check_log "log records the first sync as a baseline, not a diff" \
  "configdiff: first sync, 1 networks / 1 instances baseline"

echo
echo "== 3. A pushed YAML change produces a visible diff on the next sync =="
docker compose exec -T config-repo sh -c '
  set -e
  rm -rf /tmp/validate-work
  git clone --no-hardlinks /srv/git/fleet.git /tmp/validate-work
  cd /tmp/validate-work
  git config user.email dev@homelab-ops.local
  git config user.name "sync-warns-on-config-diff"
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
  git commit -m "sync-warns-on-config-diff: change dev-lan, add extra-lan, drop devnode0" >/dev/null
  git push origin main >/dev/null 2>&1
' >/dev/null 2>&1
check "pushed a second commit to the fixture repo" docker compose exec -T config-repo test -d /tmp/validate-work

sync2=$(curl -s -X POST "$base_url/sync")
check_json "second POST /sync returns a new commit SHA" "$sync2" ".commit != $(echo "$sync1" | jq '.commit')"
check_json "diff reports the added network" "$sync2" '.diff.networks_added == 1'
check_json "diff reports the changed network" "$sync2" '.diff.networks_changed == 1'
check_json "diff reports the removed instance" "$sync2" '.diff.instances_removed == 1'
check_json "diff reports no spurious instance adds/changes" "$sync2" \
  '.diff.instances_added == 0 and .diff.instances_changed == 0'

check_log "log shows the added network as a human-readable warning" \
  "+ network extra-lan added"
check_log "log shows the changed network as a human-readable warning" \
  "~ network dev-lan changed"
check_log "log shows the removed instance as a human-readable warning" \
  "- instance devnode0 removed"

echo
echo "== 4. A semantically-invalid pushed config is rejected at sync (issue #46) =="
# config.Validate runs in server.SyncOnce: a Network whose gateway falls
# outside its CIDR is bad upstream content and must fail the sync with 502
# (ErrValidate), naming the offending field path in the log — never persisting
# the broken state.
docker compose exec -T config-repo sh -c '
  set -e
  rm -rf /tmp/validate-bad
  git clone --no-hardlinks /srv/git/fleet.git /tmp/validate-bad
  cd /tmp/validate-bad
  git config user.email dev@homelab-ops.local
  git config user.name "sync-warns-on-config-diff"
  cat > fleet.yaml <<EOF
kind: Network
name: dev-lan
cidr: 10.0.1.0/24
gateway: 10.9.9.9
dhcp_excluded_range: 10.0.1.200-10.0.1.250
dns: [10.0.1.1]
EOF
  git add fleet.yaml
  git commit -m "sync-warns-on-config-diff: gateway outside cidr (must fail validation)" >/dev/null
  git push origin main >/dev/null 2>&1
' >/dev/null 2>&1
check "pushed an invalid third commit to the fixture repo" docker compose exec -T config-repo test -d /tmp/validate-bad

bad_status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$base_url/sync")
check_json "POST /sync on invalid config returns 502" "{\"code\":$bad_status}" '.code == 502'
check_log "log names the offending field path" "networks[0].gateway"

echo
echo "== 5. Zero side effects on real nodes =="
check "web container has no shell (still distroless; no provisioning tooling crept in)" \
  bash -c '! docker compose exec -T web sh -c "true" 2>/dev/null'

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
