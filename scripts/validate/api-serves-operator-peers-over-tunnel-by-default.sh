#! /bin/bash
# Validates GH issue #195 (docs/Decisions.md §28, §29): the web app's API
# listener posture, against the real docker compose stack.
#
# Default posture (WIREGUARD_ENDPOINT + WIREGUARD_OPERATOR_PEERS set, no
# API_HOST_LISTEN_ADDR):
#   - no API route answers on the host network; the only TCP port is the
#     /healthz-only health listener;
#   - an operator peer, dialing through the WireGuard tunnel, reaches every
#     route;
#   - a node peer, using the WireGuard key the seed route hands that node, is
#     refused (403), and the same key can't pose as the operator by sending
#     from the operator's overlay address (WireGuard drops those packets).
#
# Opt-in posture (API_HOST_LISTEN_ADDR set, as the committed dev
# docker-compose.yml does): every route answers on the chosen address, and
# startup logs the exposure warning.
#
# The tunnel side is driven by cmd/validate-tunnel-harness's http mode: the
# web app's tunnel lives in its own process's userspace network stack, so
# only another WireGuard peer can dial it, never curl.
#
# The image route answers 503 "image generation not configured" in both
# phases: this script sets no BASE_IMAGE_PATH, because streaming a multi-GB
# image is image-route-streams-seeded-image.sh's job. Reaching that handler,
# rather than a 403 or no answer, is what proves the listener serves the route.
#
# Intended to run INSIDE the devcontainer, from the repo root.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="the API serves operator peers over the tunnel by default, refuses node peers, and is on the host only when opted in (#195)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go"
VALIDATE_DURATION="~60s"

validate_parse_args "$@"
require_docker_compose
require_cmd jq go curl
check_prereqs
cd "$ROOT_DIR"

WORK_DIR="$(mktemp -d)"
HARNESS="$WORK_DIR/validate-tunnel-harness"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
CERT_DIR="$WORK_DIR/cert"
OPERATOR_KEY="$WORK_DIR/operator.key"
NODE_KEY="$WORK_DIR/node.key"
OVERRIDE="$WORK_DIR/compose-override.yml"
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"
export CERT_DIR

# The fixture's only instance (dev/git-fixture/fleet.yaml).
INSTANCE=devnode0
OPERATOR_ADDR=10.100.0.240
WEBAPP_ADDR=10.100.0.1
WG_ENDPOINT=127.0.0.1:51820
HOST_API=http://localhost:8080
HEALTH=http://localhost:8081

cleanup() {
  compose_down
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# tunnel_req <key-file> <local-addr> <method> <path> [timeout] — one request
# through the tunnel. Prints "<status>\n<body>"; fails if nothing answers.
tunnel_req() {
  "$HARNESS" -mode http -private-key-file "$1" -local-addr "$2" \
    -peer-public-key "$APP_PUB" -peer-tunnel-ip "$WEBAPP_ADDR" -peer-endpoint "$WG_ENDPOINT" \
    -method "$3" -path "$4" -timeout "${5:-30s}" 2>"$WORK_DIR/harness.err"
}

# expect_route <desc> <want-status> <key-file> <local-addr> <method> <path> [body-substring]
expect_route() {
  local desc="$1" want="$2" key="$3" local_addr="$4" method="$5" path="$6" substr="${7:-}"
  local resp code body
  if ! resp=$(tunnel_req "$key" "$local_addr" "$method" "$path"); then
    record_fail "$desc" "no answer through the tunnel: $(tail -1 "$WORK_DIR/harness.err")"
    return
  fi
  code=${resp%%$'\n'*}
  body=${resp#*$'\n'}
  if [ "$code" != "$want" ]; then
    record_fail "$desc" "want $want, got $code: $(head -c 200 <<<"$body")"
  elif [ -n "$substr" ] && ! grep -qF -- "$substr" <<<"$body"; then
    record_fail "$desc" "status $code, but body lacks '$substr': $(head -c 200 <<<"$body")"
  else
    record_pass "$desc"
  fi
}

# expect_host <desc> <want-status> <method> <url> [body-substring]
expect_host() {
  local desc="$1" want="$2" method="$3" url="$4" substr="${5:-}"
  local body code
  body=$(curl -s -X "$method" -o - -w '\n%{http_code}' "$url")
  code=${body##*$'\n'}
  body=${body%$'\n'*}
  if [ "$code" != "$want" ]; then
    record_fail "$desc" "want $want, got $code: $(head -c 200 <<<"$body")"
  elif [ -n "$substr" ] && ! grep -qF -- "$substr" <<<"$body"; then
    record_fail "$desc" "status $code, but body lacks '$substr': $(head -c 200 <<<"$body")"
  else
    record_pass "$desc"
  fi
}

echo "== 1. Build the harness, an operator keypair and the break-glass cert =="
check "build validate-tunnel-harness" go build -o "$HARNESS" ./cmd/validate-tunnel-harness
check "build bootstrap" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
if [ ! -x "$HARNESS" ] || [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: could not build the harness or bootstrap binary" >&2
  exit 2
fi
OPERATOR_PUB=$("$HARNESS" -mode genkey -private-key-file "$OPERATOR_KEY")
check "operator keypair generated" test -n "$OPERATOR_PUB"
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "api-serves-operator-peers-over-tunnel-by-default"
export OPERATOR_PUB

echo
echo "== 2. Default posture: tunnel only, one operator peer, no host listener =="
# The committed docker-compose.yml opts in to the host listener for dev;
# blanking API_HOST_LISTEN_ADDR here restores the shipped default. Its port
# stays published, so the checks below dial a port nothing in the container
# listens on. The health listener's port is published so the script can
# tell when the app is up.
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    ports:
      - "127.0.0.1:8081:8081"
    volumes:
      - ${CERT_DIR}:/cert:ro
    environment:
      - API_HOST_LISTEN_ADDR=
      - CLIENT_CERT_PATH=/cert/client.crt
      - WIREGUARD_ENDPOINT=127.0.0.1:51820
      - WIREGUARD_OPERATOR_PEERS=10.100.0.240=${OPERATOR_PUB}
EOF
check "docker compose up --build succeeds" compose up --build -d
wait_web_ready "$HEALTH" 40
wait_log "tunnel API listener is up on $WEBAPP_ADDR:80" "tunnel API listener (1 operator peers) listening on \"$WEBAPP_ADDR:80\"" 10
if _grep_web_log "host API listener"; then
  record_fail "no host API listener in the default posture" "the web log shows one"
else
  record_pass "no host API listener in the default posture"
fi

# The operator's WireGuard config needs the web app's public key. In the
# default posture no API route can tell them, so the web app logs it at
# startup, which is where an operator reads it too.
APP_PUB=$(compose logs web 2>/dev/null | grep -oE 'wireguard public key [A-Za-z0-9+/=]+' | tail -1 | awk '{print $4}')
check "web app logged its WireGuard public key" test -n "$APP_PUB"

echo
echo "-- no API route answers on the host network --"
for route in "GET /healthz" "POST /sync" "GET /status" "GET /networks" "GET /instances" \
  "POST /instances/$INSTANCE/seed" "GET /instances/$INSTANCE/image"; do
  method=${route%% *}
  path=${route#* }
  code=$(curl -s -o /dev/null -w '%{http_code}' -X "$method" "$HOST_API$path")
  check_eq "host :8080 $method $path gets no HTTP answer" "000" "$code"
  if [ "$path" != /healthz ]; then
    code=$(curl -s -o /dev/null -w '%{http_code}' -X "$method" "$HEALTH$path")
    check_eq "health listener :8081 does not serve $method $path" "404" "$code"
  fi
done

echo
echo "-- an operator peer reaches every route through the tunnel --"
expect_route "operator: GET /healthz" 200 "$OPERATOR_KEY" "$OPERATOR_ADDR" GET /healthz
expect_route "operator: POST /sync" 200 "$OPERATOR_KEY" "$OPERATOR_ADDR" POST /sync '"commit"'
expect_route "operator: GET /status" 200 "$OPERATOR_KEY" "$OPERATOR_ADDR" GET /status '"synced":true'
expect_route "operator: GET /networks" 200 "$OPERATOR_KEY" "$OPERATOR_ADDR" GET /networks 'dev-lan'
expect_route "operator: GET /instances" 200 "$OPERATOR_KEY" "$OPERATOR_ADDR" GET /instances "$INSTANCE"
expect_route "operator: GET /instances/$INSTANCE/image reaches the image handler" 503 "$OPERATOR_KEY" "$OPERATOR_ADDR" GET "/instances/$INSTANCE/image" 'image generation not configured'

seed_resp=""
if resp=$(tunnel_req "$OPERATOR_KEY" "$OPERATOR_ADDR" POST "/instances/$INSTANCE/seed"); then
  seed_resp=${resp#*$'\n'}
  check_eq "operator: POST /instances/$INSTANCE/seed" 200 "${resp%%$'\n'*}"
else
  record_fail "operator: POST /instances/$INSTANCE/seed" "no answer through the tunnel"
fi
network_yaml=$(jq -r '.network_yaml // empty' <<<"$seed_resp" 2>/dev/null)
check "seed carries $INSTANCE's network.yaml" test -n "$network_yaml"

echo
echo "-- a node peer, holding the key its seed hands out, is refused --"
node_priv=$(grep -oE 'private_key: *[A-Za-z0-9+/=]+' <<<"$network_yaml" | head -1 | awk '{print $2}')
node_addr=""
if resp=$(tunnel_req "$OPERATOR_KEY" "$OPERATOR_ADDR" GET /instances); then
  node_addr=$(jq -r --arg n "$INSTANCE" '.[] | select(.Name == $n) | .TunnelIP // empty' <<<"${resp#*$'\n'}" 2>/dev/null)
fi
check "node WireGuard key and overlay address read from the seed" test -n "$node_priv" -a -n "$node_addr"
printf '%s' "$node_priv" >"$NODE_KEY"

for route in "GET /status" "GET /instances" "POST /instances/$INSTANCE/seed"; do
  expect_route "node peer ($node_addr): ${route} is refused" 403 "$NODE_KEY" "$node_addr" "${route%% *}" "${route#* }" 'not an operator peer'
done
check_log "the web log records the refusal" "refused GET \"/status\" from \"$node_addr:"

if tunnel_req "$NODE_KEY" "$OPERATOR_ADDR" GET /status 10s >/dev/null; then
  record_fail "node key posing as the operator address gets no answer" "the web app answered"
else
  record_pass "node key posing as the operator address gets no answer"
fi

compose_down

echo
echo "== 3. Opt-in posture: host listener on the chosen address =="
# Only what the seed route needs: the dev compose file's own
# API_HOST_LISTEN_ADDR=:8080, published on 127.0.0.1, is the opt-in.
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    volumes:
      - ${CERT_DIR}:/cert:ro
    environment:
      - CLIENT_CERT_PATH=/cert/client.crt
      - WIREGUARD_ENDPOINT=127.0.0.1:51820
EOF
check "docker compose up (host listener) succeeds" compose up -d
wait_web_ready "$HOST_API" 40
check_log "startup logs the host-listener exposure warning" 'WARNING: API_HOST_LISTEN_ADDR=":8080" exposes the whole API'
check_log "the warning names the leaked WireGuard private keys" "receive that node's WireGuard private key"

expect_host "host: GET /healthz" 200 GET "$HOST_API/healthz"
expect_host "host: POST /sync" 200 POST "$HOST_API/sync" '"commit"'
expect_host "host: GET /status" 200 GET "$HOST_API/status" '"synced":true'
expect_host "host: GET /networks" 200 GET "$HOST_API/networks" 'dev-lan'
expect_host "host: GET /instances" 200 GET "$HOST_API/instances" "$INSTANCE"
expect_host "host: POST /instances/$INSTANCE/seed" 200 POST "$HOST_API/instances/$INSTANCE/seed" 'network_yaml'
expect_host "host: GET /instances/$INSTANCE/image reaches the image handler" 503 GET "$HOST_API/instances/$INSTANCE/image" 'image generation not configured'

summary
