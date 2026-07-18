#! /bin/bash
# Validates the local Grafana + Loki + Prometheus dev stack (issue #82).
#
# No real Alloy-in-Incus instance exists yet (that's #78/#80's job), so this
# proves the *destination* side works: Loki accepts a pushed log line and it
# comes back on query, Prometheus's --web.enable-remote-write-receiver
# accepts a real remote_write round trip (Prometheus scraping and
# remote_writing to itself), and Grafana is up with both wired as
# datasources — all with no Grafana Cloud credentials involved.
#
# Drives the real `docker compose` stack (loki + prometheus + grafana only;
# web/config-repo are unrelated to this check and skipped for speed).
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires
# docker compose, curl, and jq.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

pass=0
fail=0

compose() {
  docker compose -f "$ROOT_DIR/docker-compose.yml" "$@"
}

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
    echo "FAIL: $desc"
    fail=$((fail + 1))
  fi
}

# wait_http retries a plain HTTP 200 check against a readiness endpoint.
wait_http() {
  local desc="$1" url="$2" tries="${3:-30}"
  for _ in $(seq 1 "$tries"); do
    if curl -sf -o /dev/null "$url"; then
      echo "PASS: $desc"
      pass=$((pass + 1))
      return 0
    fi
    sleep 1
  done
  echo "FAIL: $desc (unreachable after ${tries}s: $url)"
  fail=$((fail + 1))
  return 1
}

# wait_json retries a GET against url until the response satisfies a jq
# filter, since both Loki's query path and Prometheus's remote_write path are
# asynchronous relative to the push/scrape that feeds them.
wait_json() {
  local desc="$1" url="$2" filter="$3" tries="${4:-30}"
  local body=""
  for _ in $(seq 1 "$tries"); do
    body=$(curl -s "$url")
    if echo "$body" | jq -e "$filter" >/dev/null 2>&1; then
      echo "PASS: $desc"
      pass=$((pass + 1))
      return 0
    fi
    sleep 1
  done
  echo "FAIL: $desc (condition not met after ${tries}s; last response: $body)"
  fail=$((fail + 1))
  return 1
}

cleanup() {
  compose down >/dev/null 2>&1
}
trap cleanup EXIT

echo "== 1. Bring up the local stack (loki, prometheus, grafana only) =="
check "docker compose up succeeds" compose up -d loki prometheus grafana

echo
echo "== 2. Poll readiness of each service =="
wait_http "Loki is ready" http://localhost:3100/ready
wait_http "Prometheus is ready" http://localhost:9090/-/ready
wait_http "Grafana is healthy" http://localhost:3000/api/health

echo
echo "== 3. Push a marker log line directly to Loki's push API and query it back =="
marker="homelab-ops-validate-82-$(date +%s)"
ts_ns=$(date +%s%N)
payload=$(jq -n --arg ts "$ts_ns" --arg line "$marker" \
  '{streams: [{stream: {job: "observability-stack"}, values: [[$ts, $line]]}]}')
check "pushed marker log line to Loki" \
  curl -sf -o /dev/null -X POST http://localhost:3100/loki/api/v1/push \
  -H "Content-Type: application/json" -d "$payload"

# Log-selector queries (as opposed to metric queries like count_over_time)
# are only served by query_range, not the instant /query endpoint.
query_encoded=$(jq -rn --arg q '{job="observability-stack"}' '$q|@uri')
filter='[.data.result[].values[][1]] | index("MARKER") != null'
filter="${filter/MARKER/$marker}"
wait_json "Loki query returns the pushed marker line" \
  "http://localhost:3100/loki/api/v1/query_range?query=${query_encoded}&limit=10" \
  "$filter"

echo
echo "== 4. Confirm Prometheus's remote_write receiver actually ingested data =="
# prometheus.yml (dev/observability-stack/prometheus/prometheus.yml) has
# Prometheus scrape itself and remote_write those samples back to its own
# --web.enable-remote-write-receiver endpoint, so a nonzero success counter
# here proves the receiver flag genuinely works, not just that scraping does.
# WAL replay + the queue manager's first batch flush take a while to warm
# up, so this needs a longer poll window than the other checks.
wait_json "Prometheus reports successful remote_write samples" \
  "http://localhost:9090/api/v1/query?query=prometheus_remote_storage_samples_total" \
  '.data.result[0].value[1] | tonumber > 0' \
  90

echo
echo "== 5. Confirm Grafana has both datasources provisioned =="
datasources=$(curl -s -u admin:admin http://localhost:3000/api/datasources)
check_json "Grafana lists Loki + Prometheus datasources" "$datasources" \
  '([.[].type] | sort) == ["loki", "prometheus"]'

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
