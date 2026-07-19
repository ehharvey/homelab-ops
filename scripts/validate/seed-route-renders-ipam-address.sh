#! /bin/bash
# Validates GH issue #38: an Instance synced *without* static_ip still reaches
# seed.Render with a concrete, IPAM-assigned address (plus default route) by the
# time network.yaml is rendered via POST /instances/{name}/seed — and that the
# assigned address is stable across re-syncs (does not churn each poll).
#
# Unlike seed-route-renders-static-ip.sh (which asserts an *explicit* static_ip renders),
# this drives the static_ip-less path end-to-end through the running app: real
# config.Parse of a static_ip-less YAML -> POST /sync -> IPAM auto-assign ->
# SQLite -> seed route. That's coverage the Go integration test can't give (its
# fakeSyncer bypasses config.Parse, the git remote, and the HTTP app).
#
# NOTE: unlike seed-route-renders-static-ip.sh / -41.sh, there is no red->green fix here.
# #38's wiring is inherited from #35 (IPAM) and #36 (seed route); this script is
# a regression guard that would only fail if that pull-through were later broken.
#
# It deliberately does NOT touch the committed dev/git-fixture/fleet.yaml (shared
# by background-poll-warns-on-config-diff.sh / store-retains-synced-fleet.sh / -22.sh, which assert
# exact instance counts). Instead it pushes its own fleet into the config-repo
# git server at runtime, the same way background-poll-warns-on-config-diff.sh does.
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires docker
# compose, jq, and go.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="POST /instances/{name}/seed renders a stable IPAM-assigned address (#38)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go"
VALIDATE_DURATION="~15s"

validate_parse_args "$@"
cd "$ROOT_DIR"

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$ROOT_DIR/bin/bootstrap"
CERT_DIR="$WORK_DIR/cert"
export CERT_DIR
OVERRIDE="$(mktemp /tmp/compose-issue38-override.XXXXXX.yml)"
# lib-compose.sh's compose() picks this up; scripts that need no scoped
# override simply leave it unset.
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"

# The seed route 503s without a CertSource, so mount an operator cert at
# CLIENT_CERT_PATH exactly as seed-route-renders-static-ip.sh does.
#
# WIREGUARD_ENDPOINT is required for the same reason: #107 added a second gate
# to this route ("wireguard not configured"), and this script never set it, so
# every seed assertion below has been failing since. It is a *config* gate, not
# a hardware one — internal/wireguard runs the tunnel over netstack.CreateNetTUN
# + conn.NewDefaultBind() (userspace, no NET_ADMIN, an unprivileged
# net.ListenUDP), and resolveInstanceSeed mints the node credential offline
# without dialing anything. So a loopback endpoint satisfies it; the port is
# already published by docker-compose.yml.
#
# Note this makes tunnel startup FATAL rather than degraded (see cmd/web/main.go
# — a *set* endpoint expresses operator intent), so the stack will fail to come
# up if 51820 is already bound. That is the correct failure: loud, not silent.
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    volumes:
      - ${CERT_DIR}:/cert:ro
    environment:
      - CLIENT_CERT_PATH=/cert/client.crt
      - WIREGUARD_ENDPOINT=127.0.0.1:51820
EOF

cleanup() {
  compose down >/dev/null 2>&1
  rm -rf "$WORK_DIR"
  rm -f "$OVERRIDE"
}
trap cleanup EXIT

echo "== 1. Prerequisites: build bootstrap binary, generate the operator's cert =="
check "build bootstrap binary" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap binary not built or not executable: $BOOTSTRAP_BIN" >&2
  exit 2
fi
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "seed-route-renders-ipam-address"

echo
echo "== 2. Bring up the real stack with the cert mounted =="
check "docker compose up --build succeeds" compose up --build -d

base_url="http://localhost:8080"
for _ in $(seq 1 20); do
  curl -s -o /dev/null "$base_url/healthz" && break
  sleep 0.5
done
check "web is reachable" curl -sf -o /dev/null "$base_url/healthz"

echo
echo "== 3. Push a fleet whose only instance (devnode1) omits static_ip =="
# devnode1 has NO static_ip line: IPAM must auto-assign from dev-lan's
# dhcp_excluded_range (10.0.0.200-10.0.0.250) at sync time.
compose exec -T config-repo sh -c '
  set -e
  rm -rf /tmp/validate-work
  git clone --no-hardlinks /srv/git/fleet.git /tmp/validate-work
  cd /tmp/validate-work
  git config user.email dev@homelab-ops.local
  git config user.name "seed-route-renders-ipam-address"
  cat > fleet.yaml <<EOF
kind: Network
name: dev-lan
cidr: 10.0.0.0/24
gateway: 10.0.0.1
dhcp_excluded_range: 10.0.0.200-10.0.0.250
dns: [10.0.0.1]
---
kind: Instance
name: devnode1
mac: aa:bb:cc:dd:ee:01
network: dev-lan
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
EOF
  git add fleet.yaml
  git commit -m "seed-route-renders-ipam-address: static_ip-less devnode1" >/dev/null
  git push origin main >/dev/null 2>&1
' >/dev/null 2>&1
check "pushed a static_ip-less fleet to the fixture repo" \
  compose exec -T config-repo test -d /tmp/validate-work

echo
echo "== 4. Sync it and render devnode1's seed =="
sync_resp=$(curl -s -X POST "$base_url/sync")
check_json "POST /sync returns a commit SHA" "$sync_resp" '.commit | length > 0'
check_json "sync reports exactly 1 instance" "$sync_resp" '.instances == 1'

seed_resp=$(curl -s -X POST "$base_url/instances/devnode1/seed")
check_json "response has network_yaml" "$seed_resp" '.network_yaml | length > 0'

network_yaml=$(echo "$seed_resp" | jq -r '.network_yaml')
check "network.yaml carries devnode1's MAC" \
  bash -c "echo '$network_yaml' | grep -qi 'aa:bb:cc:dd:ee:01'"

# The only /24 address in network.yaml is the interface's assigned static IP.
ip1=$(echo "$network_yaml" | grep -oE '10\.0\.0\.[0-9]{1,3}/24' | head -1)
check "network.yaml carries an auto-assigned /24 address" test -n "$ip1"
# With a single instance, the pool's first usable address is deterministic:
# .200 (ascending from excludedStart, skipping network/broadcast/gateway, none
# of which is .200).
check "auto-assigned address is the first pool address (10.0.0.200/24)" \
  bash -c "[ '$ip1' = '10.0.0.200/24' ]"
check "network.yaml carries a default route (0.0.0.0/0)" \
  bash -c "echo '$network_yaml' | grep -q '0.0.0.0/0'"
check "network.yaml routes the default via the gateway (10.0.0.1)" \
  bash -c "echo '$network_yaml' | grep -q '10.0.0.1'"

echo
echo "== 5. Re-syncing the identical fleet keeps the same address (no churn) =="
sync_resp2=$(curl -s -X POST "$base_url/sync")
check_json "second POST /sync returns a commit SHA" "$sync_resp2" '.commit | length > 0'

seed_resp2=$(curl -s -X POST "$base_url/instances/devnode1/seed")
network_yaml2=$(echo "$seed_resp2" | jq -r '.network_yaml')
ip2=$(echo "$network_yaml2" | grep -oE '10\.0\.0\.[0-9]{1,3}/24' | head -1)
if [ -n "$ip2" ] && [ "$ip1" = "$ip2" ]; then
  record_pass "address is stable across re-sync ($ip1)"
else
  record_fail "address churned across re-sync (first $ip1, second $ip2)"
fi

summary
