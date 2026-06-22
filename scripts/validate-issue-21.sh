#! /bin/bash
# Validates GH issue #21 ("In-memory/local store for parsed objects")
# "Done when" criteria: the store retains last-synced state across sync
# runs and supports lookups by kind/name.
#
# Drives the real `docker compose` stack (web + the dev/git-fixture
# config-repo from #20) rather than just `go test`: builds the actual
# distroless web image, syncs against a real git remote, and pushes a
# second commit to that remote to prove the store *replaces* its snapshot
# on each sync rather than merging into it.
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires
# docker compose and jq.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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

cleanup() {
  docker compose down >/dev/null 2>&1
}
trap cleanup EXIT

echo "== 1. Bring up the real stack (web + dev git fixture) =="
check "docker compose up --build succeeds" docker compose up --build -d
check "web container has no shell (still distroless)" bash -c '! docker compose exec -T web sh -c "true" 2>/dev/null'

base_url="http://localhost:8080"
for _ in $(seq 1 20); do
  curl -s -o /dev/null "$base_url/healthz" && break
  sleep 0.5
done
check "web is reachable" curl -sf -o /dev/null "$base_url/healthz"

echo
echo "== 2. Store starts empty =="
status_before=$(curl -s "$base_url/status")
check_json "GET /status reports synced=false before any sync" "$status_before" '.synced == false'

echo
echo "== 3. First sync persists the fixture's Network/Instance objects =="
sync1=$(curl -s -X POST "$base_url/sync")
check_json "POST /sync returns a commit SHA" "$sync1" '.commit | length > 0'
check_json "POST /sync reports 1 network" "$sync1" '.networks == 1'
check_json "POST /sync reports 1 instance" "$sync1" '.instances == 1'

status_after=$(curl -s "$base_url/status")
check_json "GET /status reports synced=true after sync" "$status_after" '.synced == true'
check_json "GET /status commit matches the synced commit" "$status_after" ".commit == $(echo "$sync1" | jq '.commit')"

networks1=$(curl -s "$base_url/networks")
check_json "GET /networks returns dev-lan (queryable by name)" "$networks1" '[.[] | select(.Name == "dev-lan")] | length == 1'
instances1=$(curl -s "$base_url/instances")
check_json "GET /instances returns devnode0 (queryable by name)" "$instances1" '[.[] | select(.Name == "devnode0")] | length == 1'

echo
echo "== 4. A second sync replaces, rather than merges, the stored snapshot =="
docker compose exec -T config-repo sh -c '
  set -e
  rm -rf /tmp/validate-work
  git clone --no-hardlinks /srv/git/fleet.git /tmp/validate-work
  cd /tmp/validate-work
  git config user.email dev@homelab-ops.local
  git config user.name "validate-issue-21"
  cat > fleet.yaml <<EOF
kind: Network
name: other-lan
cidr: 10.1.0.0/24
gateway: 10.1.0.1
dhcp_excluded_range: 10.1.0.200-10.1.0.250
dns: [1.1.1.1]
EOF
  git add fleet.yaml
  git commit -m "validate-issue-21: replace fixture with other-lan" >/dev/null
  git push origin main >/dev/null 2>&1
' >/dev/null 2>&1
check "pushed a second commit to the fixture repo" docker compose exec -T config-repo test -d /tmp/validate-work

sync2=$(curl -s -X POST "$base_url/sync")
check_json "second POST /sync returns a new commit SHA" "$sync2" ".commit != $(echo "$sync1" | jq '.commit')"

networks2=$(curl -s "$base_url/networks")
check_json "GET /networks now returns only other-lan" "$networks2" '(. | length == 1) and (.[0].Name == "other-lan")'
instances2=$(curl -s "$base_url/instances")
check_json "GET /instances is empty (old devnode0 dropped, not unioned)" "$instances2" '. == null or length == 0'

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
