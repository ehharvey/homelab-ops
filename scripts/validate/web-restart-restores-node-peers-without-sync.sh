#! /bin/bash
# Validates GH issue #269 (docs/Decisions.md §11, §28): a restarted web app
# answers an already-provisioned node's WireGuard handshake straight away,
# with no config sync since the restart.
#
# The peer table lives only in the web app's in-process tunnel. Before #269
# it was filled only by a sync, so after a restart with CONFIG_SYNC_INTERVAL
# unset (as here) every flashed node's handshake went unanswered until an
# operator's POST /sync. Now startup re-registers the peers of every instance
# in the store.
#
# Steps, against the real docker compose stack with a persistent store:
#   1. sync once and render the fixture instance's seed (the dev compose
#      file's opt-in host listener serves both), taking the node's WireGuard
#      key and overlay address from it;
#   2. a peer holding that key gets an answer through the tunnel;
#   3. restart the web container (same container, so the store and the
#      tunnel identity survive) and, without syncing, the same peer gets an
#      answer again; GET /status's synced_at, which every sync's store
#      Replace rewrites, is unchanged since step 1.
#
# "An answer" is the tunnel API listener's 403 for a non-operator peer: it
# can only arrive once the WireGuard handshake completed, which needs the
# node's key in the peer table. The 403 itself is
# api-serves-operator-peers-over-tunnel-by-default.sh's subject.
#
# The tunnel side is driven by cmd/validate-tunnel-harness's http mode: the
# web app's tunnel lives in its own process's userspace network stack, so
# only another WireGuard peer can dial it, never curl.
#
# Intended to run INSIDE the devcontainer, from the repo root.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="a restarted web app answers a provisioned node's tunnel handshake before any sync (#269)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go curl"
VALIDATE_DURATION="~60s"

validate_parse_args "$@"
require_compose
require_cmd jq go curl
check_prereqs
cd "$ROOT_DIR"

WORK_DIR="$(mktemp -d)"
HARNESS="$WORK_DIR/validate-tunnel-harness"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
CERT_DIR="$WORK_DIR/cert"
NODE_KEY="$WORK_DIR/node.key"
OVERRIDE="$WORK_DIR/compose-override.yml"
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"
export CERT_DIR

# The fixture's only instance (dev/git-fixture/fleet.yaml).
INSTANCE=devnode0
WEBAPP_ADDR=10.100.0.1
WG_ENDPOINT=127.0.0.1:51820
HOST_API=http://localhost:8080

cleanup() {
  compose_down
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# synced_at — when the stored snapshot was last replaced, i.e. the last sync.
synced_at() {
  curl -s "$HOST_API/status" | jq -r '.synced_at // empty' 2>/dev/null
}

# app_pub — the web app's WireGuard public key, from the newest startup log.
app_pub() {
  compose logs web 2>/dev/null | grep -oE 'wireguard public key [A-Za-z0-9+/=]+' | tail -1 | awk '{print $4}'
}

# expect_node_answer <desc> — the node's key reaches the tunnel API listener.
expect_node_answer() {
  local desc="$1" resp code
  if ! resp=$("$HARNESS" -mode http -private-key-file "$NODE_KEY" -local-addr "$node_addr" \
    -peer-public-key "$APP_PUB" -peer-tunnel-ip "$WEBAPP_ADDR" -peer-endpoint "$WG_ENDPOINT" \
    -method GET -path /status -timeout 30s 2>"$WORK_DIR/harness.err"); then
    record_fail "$desc" "no answer through the tunnel: $(tail -1 "$WORK_DIR/harness.err")"
    return
  fi
  code=${resp%%$'\n'*}
  if [ "$code" = 403 ] && grep -qF 'not an operator peer' <<<"${resp#*$'\n'}"; then
    record_pass "$desc"
  else
    record_fail "$desc" "want the operator filter's 403, got $code: $(head -c 200 <<<"${resp#*$'\n'}")"
  fi
}

echo "== 1. Build the harness and the break-glass cert =="
check "build validate-tunnel-harness" go build -o "$HARNESS" ./cmd/validate-tunnel-harness
check "build bootstrap" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
if [ ! -x "$HARNESS" ] || [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: could not build the harness or bootstrap binary" >&2
  exit 2
fi
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "web-restart-restores-node-peers-without-sync"

echo
echo "== 2. Sync once, provision the node, reach the tunnel with its key =="
# STORE_PATH defaults to :memory:, which a restart would wipe along with the
# tunnel identity. A file in the container's own filesystem survives
# `compose restart`, which keeps the container.
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    volumes:
      - ${CERT_DIR}:/cert:ro
    environment:
      - CLIENT_CERT_PATH=/cert/client.crt
      - STORE_PATH=/tmp/store.db
      - WIREGUARD_ENDPOINT=127.0.0.1:51820
      - CONFIG_SYNC_INTERVAL=
EOF
check "docker compose up --build succeeds" compose up --build -d
wait_web_ready "$HOST_API" 40
check_log "startup with an empty store restores no instance peers" "wireguard: registered 0/0 stored instance peers"
APP_PUB=$(app_pub)
check "web app logged its WireGuard public key" test -n "$APP_PUB"

check_eq "POST /sync" 200 "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$HOST_API/sync")"
SYNCED_AT=$(synced_at)
check "GET /status reports the sync's synced_at" test -n "$SYNCED_AT"
seed=$(curl -s -X POST "$HOST_API/instances/$INSTANCE/seed")
node_priv=$(jq -r '.network_yaml // empty' <<<"$seed" 2>/dev/null | grep -oE 'private_key: *[A-Za-z0-9+/=]+' | head -1 | awk '{print $2}')
node_addr=$(curl -s "$HOST_API/instances" | jq -r --arg n "$INSTANCE" '.[] | select(.Name == $n) | .TunnelIP // empty' 2>/dev/null)
check "node WireGuard key and overlay address read from the seed" test -n "$node_priv" -a -n "$node_addr"
printf '%s' "$node_priv" >"$NODE_KEY"

expect_node_answer "before the restart: node peer ($node_addr) gets an answer through the tunnel"

echo
echo "== 3. Restart the web app; the node gets an answer with no sync since =="
check "docker compose restart web succeeds" compose restart web
wait_web_ready "$HOST_API" 40
wait_log "startup restores the stored instance's peer" "wireguard: registered 1/1 stored instance peers" 10
check_eq "the tunnel identity survived the restart" "$APP_PUB" "$(app_pub)"

expect_node_answer "after the restart, no sync: node peer ($node_addr) gets an answer through the tunnel"

# Every sync replaces the stored snapshot and so rewrites synced_at, even one
# that finds no diff and logs nothing. Unchanged after the tunnel check, it
# shows the answer above came from the restore, not from a sync since.
check_eq "no config sync since step 2 (synced_at unchanged)" "$SYNCED_AT" "$(synced_at)"

summary
