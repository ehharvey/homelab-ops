#! /bin/bash
# Validates GH issue #36: POST /instances/{name}/seed renders the same four
# seed files (install.yaml, network.yaml, applications.yaml, incus.yaml) the
# bootstrap CLI's render-seed produces for an equivalent fleet/cert input.
#
# Also exercises #36's cert-sourcing design (folded in from #37, now closed):
# the web app never generates or stores a cert — it reads one operator-
# supplied cert's public half from CLIENT_CERT_PATH and embeds it verbatim.
# This script generates that cert with the same `bootstrap gen-cert` an
# operator would run, mounts it into the web container via a compose
# override (CLIENT_CERT_PATH env var + a read-only volume — scoped to this
# script, not a permanent docker-compose.yml change, same pattern
# validate-config-sync-poll.sh uses for CONFIG_SYNC_INTERVAL), and asserts
# the route's incus.yaml embeds that exact cert's DER bytes.
#
# The route, CertSource interface, and CLIENT_CERT_PATH wiring this script
# exercises may not exist yet — until #36 lands, expect this to fail with a
# connection/404 error against POST /instances/.../seed. That's the desired
# failure mode until the fix is applied (same convention as
# validate-issue-41.sh).
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires
# docker compose, jq, go, and openssl.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

pass=0
fail=0

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$ROOT_DIR/bin/bootstrap"
CERT_DIR="$WORK_DIR/cert"
export CERT_DIR
OVERRIDE="$(mktemp /tmp/compose-cert-override.XXXXXX.yml)"

# WIREGUARD_ENDPOINT is required alongside the cert: #107 added a second gate to
# the seed route ("wireguard not configured"), and this script never set it, so
# every assertion below has been failing since. It is a *config* gate, not a
# hardware one — internal/wireguard runs the tunnel over netstack.CreateNetTUN +
# conn.NewDefaultBind() (userspace, no NET_ADMIN, an unprivileged
# net.ListenUDP), and resolveInstanceSeed mints the node credential offline
# without dialing anything. So a loopback endpoint satisfies it; the port is
# already published by docker-compose.yml. See #129, which fixed the same defect
# in validate-issue-38.sh and -39.sh, and docs/Decisions.md §20.
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

compose() {
  docker compose -f "$ROOT_DIR/docker-compose.yml" -f "$OVERRIDE" "$@"
}

cleanup() {
  compose down >/dev/null 2>&1
  rm -rf "$WORK_DIR"
  rm -f "$OVERRIDE"
}
trap cleanup EXIT

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

echo "== 1. Prerequisites: build bootstrap binary, generate the operator's cert =="
check "build bootstrap binary" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap binary not built or not executable: $BOOTSTRAP_BIN" >&2
  exit 2
fi
# This is the exact step an operator runs once for the deployment's single
# break-glass cert (per docs/Architecture.md's "Cert sourcing"); the web app
# never runs this itself.
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "validate-issue-36"

# The DER bytes the route's incus.yaml must embed (base64-encoded), so the
# comparison below proves the route reads back the literal file gen-cert
# wrote, not a regenerated/different cert.
want_der_b64="$(openssl x509 -in "$CERT_DIR/client.crt" -outform DER 2>/dev/null | base64 | tr -d '\n')"
check "computed expected DER (sanity check on openssl/cert)" test -n "$want_der_b64"

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
echo "== 3. Sync the fixture fleet (dev-lan / devnode0) into the store =="
sync_resp=$(curl -s -X POST "$base_url/sync")
check_json "POST /sync returns a commit SHA" "$sync_resp" '.commit | length > 0'

echo
echo "== 4. POST /instances/devnode0/seed renders all four documents =="
seed_resp=$(curl -s -X POST "$base_url/instances/devnode0/seed")
check_json "response has install_yaml" "$seed_resp" '.install_yaml | length > 0'
check_json "response has network_yaml" "$seed_resp" '.network_yaml | length > 0'
check_json "response has applications_yaml" "$seed_resp" '.applications_yaml | length > 0'
check_json "response has incus_yaml" "$seed_resp" '.incus_yaml | length > 0'

network_yaml=$(echo "$seed_resp" | jq -r '.network_yaml')
check "network.yaml carries the synced instance's MAC" \
  bash -c "echo '$network_yaml' | grep -qi 'aa:bb:cc:dd:ee:00'"
check "network.yaml carries the IPAM/static_ip-assigned address" \
  bash -c "echo '$network_yaml' | grep -q '10.0.0.210'"

incus_yaml=$(echo "$seed_resp" | jq -r '.incus_yaml')
got_der_b64=$(echo "$incus_yaml" | grep -oE '[A-Za-z0-9+/]{100,}={0,2}' | head -1)
check "incus.yaml embeds a base64 certificate blob" test -n "$got_der_b64"
if [ "$got_der_b64" = "$want_der_b64" ]; then
  echo "PASS: incus.yaml's embedded cert matches the operator-supplied cert exactly"
  pass=$((pass + 1))
else
  echo "FAIL: incus.yaml's embedded cert does not match CLIENT_CERT_PATH's cert"
  fail=$((fail + 1))
fi

echo
echo "== 5. Unknown instance 404s rather than 500ing =="
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$base_url/instances/does-not-exist/seed")
check "POST /instances/does-not-exist/seed returns 404" bash -c "[ '$status' = '404' ]"

echo
echo "(Not covered here: CLIENT_CERT_PATH unset -> 503 'cert source not configured'."
echo " That's a unit-test concern (fakeCertSource=nil), not a real-stack one.)"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
