#! /bin/bash
# Validates GH issue #397 (docs/Decisions.md §28, §29, §32): the web app's
# API is served over TLS and requires a listed operator client cert, on the
# tunnel listener and on a host listener alike, against the real docker
# compose stack.
#
# The stack runs with the tunnel on, the operator WireGuard peer listed, and
# the host listener on :8080 *without* the dev stack's plain-HTTP opt-in
# (API_HOST_PLAIN_HTTP blanked), so it serves TLS too. OPERATOR_CERTS_PATH
# lists two operator certs (two devices); CLIENT_CERT_PATH is the first. A
# third, self-signed cert is listed nowhere. It checks that:
#   - over the tunnel (`bootstrap api status`, as the listed operator peer):
#     no client cert and the unlisted cert each get the cert check's HTTP
#     403, and the CLI says which and what to do; each listed operator cert,
#     with the logged TLS key pinned, gets 200; a wrong pin is refused by the
#     CLI in the handshake, before any request;
#   - a non-operator WireGuard peer (a node's key, from its seed) presenting
#     the operator cert still gets the peer filter's 403;
#   - after a restart (same container, so the store and the tunnel identity
#     survive) the logged fingerprint is unchanged, and the pinned call
#     still gets 200;
#   - on the host listener, with curl --cert/--key and --pinnedpubkey: no
#     cert and the unlisted cert get 403, on an API route and an unknown one
#     alike (the check wraps every route), the operator cert gets 200, a
#     wrong pin is refused by curl, and plain HTTP gets no API answer;
#   - host-only TLS (tunnel off): the TLS key is derived from
#     secrets/wireguard.key, so the stack serves the operator cert and keeps
#     its fingerprint across a restart; without that file startup fails
#     naming it (operator decision on #397, 2026-11-10).
#
# cmd/validate-tunnel-harness is used only to generate keypairs.
#
# Intended to run INSIDE the devcontainer, from the repo root.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="the API, over the tunnel and on a TLS host listener, answers 403 to clients without a listed operator cert, serves one with it, keeps its pinned TLS key across a restart, and the CLI refuses a wrong pin (#397)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go curl"
# webtls.NotOperatorMessage: the body of the cert check's 403.
NOT_OPERATOR="forbidden: the API requires a listed operator client certificate"
VALIDATE_DURATION="~2m"
VALIDATE_SUPPORTED_OS="incusos"

validate_parse_args "$@"
require_compose
require_cmd jq go curl
check_prereqs
cd "$ROOT_DIR" || exit 1

WORK_DIR="$(mktemp -d)"
HARNESS="$WORK_DIR/validate-tunnel-harness"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
# Two operator devices' certs (both listed) and one listed nowhere.
CERT_DIR="$WORK_DIR/cert"
CERT2_DIR="$WORK_DIR/cert2"
STRANGER_DIR="$WORK_DIR/stranger"
OPERATOR_KEY="$WORK_DIR/operator.key"
NODE_KEY="$WORK_DIR/node.key"
OVERRIDE="$WORK_DIR/compose-override.yml"
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"
# Host-only TLS: a deployment config directory holding only the identity.
DEPLOY_DIR="$WORK_DIR/web"
export CERT_DIR DEPLOY_DIR

INSTANCE=devnode0
OPERATOR_ADDR=10.100.0.240
WG_ENDPOINT=127.0.0.1:51820
HOST_API=https://localhost:8080
HEALTH=http://localhost:8081
# A well-formed pin no key has.
HOST_CODE=""
CURL_RC=0
WRONG_PIN="sha256//$(printf 'not the web app key, 32 bytes!!!' | base64)"

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  compose_down
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# cli_as <key-file> <local-addr> <pin> <cert-dir|""> <verb> [args...] — one
# `bootstrap api` run. stdout goes to $WORK_DIR/cli.out, stderr to
# $WORK_DIR/cli.err; the exit status is the CLI's.
cli_as() {
  local key="$1" addr="$2" pin="$3" certdir="$4"
  shift 4
  local tls=(--server-fingerprint "$pin")
  if [ -n "$certdir" ]; then
    tls+=(--client-cert "$certdir/client.crt" --client-key "$certdir/client.key")
  fi
  "$BOOTSTRAP_BIN" api "$@" --private-key-file "$key" --local-addr "$addr" \
    --server-public-key "$APP_PUB" --endpoint "$WG_ENDPOINT" "${tls[@]}" \
    >"$WORK_DIR/cli.out" 2>"$WORK_DIR/cli.err"
}

# cli <pin> <cert-dir|""> <verb> [args...] — cli_as the operator peer.
cli() { cli_as "$OPERATOR_KEY" "$OPERATOR_ADDR" "$@"; }

# expect_ok <desc> — the last CLI run exited 0.
expect_ok() {
  local rc="$1" desc="$2"
  if [ "$rc" -eq 0 ]; then
    record_pass "$desc"
  else
    record_fail "$desc" "exit $rc: $(tail -n 1 "$WORK_DIR/cli.err")"
  fi
}

# expect_refused <rc> <desc> <stderr-substring...> — the last CLI run exited
# non-zero, and its stderr carries every substring.
expect_refused() {
  local rc="$1" desc="$2"
  shift 2
  local s err
  err=$(cat "$WORK_DIR/cli.err")
  if [ "$rc" -eq 0 ]; then
    record_fail "$desc" "exited 0: $(head -c 200 "$WORK_DIR/cli.out")"
    return
  fi
  for s in "$@"; do
    if ! grep -qF -- "$s" <<<"$err"; then
      record_fail "$desc" "exit $rc, but stderr lacks '$s': $(tail -n 1 <<<"$err")"
      return
    fi
  done
  record_pass "$desc"
}

# host_curl <pin> <cert-dir|""> <path> — one HTTPS GET on the host listener,
# pinned (-k skips only the CA chain: --pinnedpubkey is still checked).
# Sets HOST_CODE to the HTTP status ("000" for no answer) and CURL_RC to
# curl's exit code; the body is in $WORK_DIR/curl.out.
host_curl() {
  local pin="$1" certdir="$2" path="$3"
  local args=(-sk --max-time 10 --pinnedpubkey "$pin" -o "$WORK_DIR/curl.out" -w '%{http_code}')
  if [ -n "$certdir" ]; then
    args+=(--cert "$certdir/client.crt" --key "$certdir/client.key")
  fi
  HOST_CODE=$(curl "${args[@]}" "$HOST_API$path" 2>"$WORK_DIR/curl.err")
  CURL_RC=$?
}

# expect_host_403 <desc> — the last host_curl got the cert check's 403.
expect_host_403() {
  local body
  body=$(head -c 200 "$WORK_DIR/curl.out" 2>/dev/null)
  if [ "$HOST_CODE" = 403 ] && grep -qF -- "$NOT_OPERATOR" "$WORK_DIR/curl.out"; then
    record_pass "$1"
  else
    record_fail "$1" "HTTP $HOST_CODE, curl exit $CURL_RC: $body"
  fi
}

# web_exit_state — "<State> <ExitCode>" of the web container.
web_exit_state() {
  compose ps -a --format json web 2>/dev/null | jq -rs '.[0] | "\(.State) \(.ExitCode)"' 2>/dev/null
}

# wait_fingerprint_logged <n> [tries] — wait until the web app has logged its
# TLS key fingerprint n times (once per start).
wait_fingerprint_logged() {
  local n="$1" tries="${2:-30}" _
  for _ in $(seq 1 "$tries"); do
    [ "$(web_tls_fingerprint_count)" -ge "$n" ] && return 0
    sleep 1
  done
  return 1
}

echo "== 1. Build bootstrap and the harness; an operator keypair; three certs =="
check "build bootstrap" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
check "build validate-tunnel-harness (keygen only)" go build -o "$HARNESS" ./cmd/validate-tunnel-harness
if [ ! -x "$HARNESS" ] || [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: could not build the bootstrap binary or the harness" >&2
  exit 2
fi
OPERATOR_PUB=$("$HARNESS" -mode genkey -private-key-file "$OPERATOR_KEY")
check "operator keypair generated" test -n "$OPERATOR_PUB"
check "gen-cert: the operator's first device" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "api-requires-operator-cert-1"
check "gen-cert: the operator's second device" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT2_DIR" --common-name "api-requires-operator-cert-2"
check "gen-cert: a cert listed nowhere" "$BOOTSTRAP_BIN" gen-cert --output-dir "$STRANGER_DIR" --common-name "api-requires-operator-cert-stranger"
# The operator cert list: both devices' public halves, one PEM file.
cat "$CERT_DIR/client.crt" "$CERT2_DIR/client.crt" >"$CERT_DIR/operator-certs.pem"
export OPERATOR_PUB

echo
echo "== 2. Bring up the stack: tunnel on, TLS host listener, two operator certs =="
# The committed docker-compose.yml's API_HOST_LISTEN_ADDR=:8080 stays, and
# blanking API_HOST_PLAIN_HTTP makes it TLS. STORE_PATH is a file in the
# container's own filesystem, which `compose restart` keeps, so the tunnel
# identity, and so the TLS key derived from it, survives the restart below.
cat >"$OVERRIDE" <<EOF
services:
  web:
    ports:
      - "127.0.0.1:8081:8081"
    volumes:
      - \${CERT_DIR}:/cert:ro
    environment:
      - API_HOST_PLAIN_HTTP=
      - STORE_PATH=/tmp/web.db
      - CLIENT_CERT_PATH=/cert/client.crt
      - OPERATOR_CERTS_PATH=/cert/operator-certs.pem
      - WIREGUARD_ENDPOINT=$WG_ENDPOINT
      - WIREGUARD_OPERATOR_PEERS=$OPERATOR_ADDR=\${OPERATOR_PUB}
EOF
check "docker compose up --build succeeds" compose up --build -d
wait_web_ready "$HEALTH" 40
wait_log "tunnel API listener is up" "tunnel API listener (1 operator peers)" 10
check_log "the host listener serves TLS, operator certs only" "host API listener (TLS, operator certs only, no source filter)"
check_log "the API requires one of the 2 certs from OPERATOR_CERTS_PATH" "one of 2 operator certs from OPERATOR_CERTS_PATH"
if _grep_web_log "exposes the whole API as plain HTTP"; then
  record_fail "no plain-HTTP warning without API_HOST_PLAIN_HTTP" "the web log has one"
else
  record_pass "no plain-HTTP warning without API_HOST_PLAIN_HTTP"
fi

APP_PUB=$(compose logs web 2>/dev/null | grep -oE 'wireguard public key [A-Za-z0-9+/=]+' | tail -1 | awk '{print $4}')
check "web app logged its WireGuard public key" test -n "$APP_PUB"
TLS_PIN=$(web_tls_fingerprint)
check "web app logged its TLS server key fingerprint" test -n "$TLS_PIN"

echo
echo "== 3. Over the tunnel: only a listed operator cert is served =="
cli "$TLS_PIN" "" status
expect_refused $? "no client cert: 403 from the cert check, with what to pass" "403" "$NOT_OPERATOR" "pass the operator cert with --client-cert and --client-key"
cli "$TLS_PIN" "$STRANGER_DIR" status
expect_refused $? "an unlisted self-signed cert: 403 from the cert check, naming the setting" "403" "$NOT_OPERATOR" "the web app doesn't list --client-cert" "OPERATOR_CERTS_PATH"
cli "$TLS_PIN" "$CERT_DIR" status
expect_ok $? "the first operator cert, pinned: api status exits 0"
check_json "api status answers with the server's JSON" "$(cat "$WORK_DIR/cli.out")" 'has("synced")'
cli "$TLS_PIN" "$CERT2_DIR" status
expect_ok $? "the second listed operator cert: api status exits 0"
cli "$WRONG_PIN" "$CERT_DIR" status
expect_refused $? "a wrong pin: refused by the CLI" "refusing the web app: its TLS key is $TLS_PIN, not the pinned $WRONG_PIN"

echo
echo "== 4. The peer gate still applies under the cert =="
cli "$TLS_PIN" "$CERT_DIR" sync
expect_ok $? "api sync exits 0"
cli "$TLS_PIN" "$CERT_DIR" seed "$INSTANCE" --output "$WORK_DIR/seed.json"
expect_ok $? "api seed $INSTANCE exits 0"
cli "$TLS_PIN" "$CERT_DIR" instances
node_addr=$(jq -r --arg n "$INSTANCE" '.[] | select(.Name == $n) | .TunnelIP // empty' "$WORK_DIR/cli.out" 2>/dev/null)
node_priv=$(jq -r '.network_yaml // empty' "$WORK_DIR/seed.json" 2>/dev/null | grep -oE 'private_key: *[A-Za-z0-9+/=]+' | head -1 | awk '{print $2}')
check "node WireGuard key and overlay address read from the seed and instances" test -n "$node_priv" -a -n "$node_addr"
printf '%s\n' "$node_priv" >"$NODE_KEY"
cli_as "$NODE_KEY" "$node_addr" "$TLS_PIN" "$CERT_DIR" status
expect_refused $? "a node peer ($node_addr) with the operator cert: 403" "403" "not an operator peer"

echo
echo "== 5. A restart keeps the TLS key =="
check "docker compose restart web succeeds" compose restart web
if wait_fingerprint_logged 2 40; then
  record_pass "the restarted web app logged its fingerprint again"
else
  record_fail "the restarted web app logged its fingerprint again" "only $(web_tls_fingerprint_count) fingerprint lines"
fi
wait_web_ready "$HEALTH" 40
check_eq "the logged fingerprint is unchanged after the restart" "$TLS_PIN" "$(web_tls_fingerprint)"
cli "$TLS_PIN" "$CERT_DIR" status
expect_ok $? "after the restart, the pinned api status exits 0"

echo
echo "== 6. The host listener: TLS, operator cert required =="
host_curl "$TLS_PIN" "" /status
expect_host_403 "host: no client cert gets 403 on GET /status"
host_curl "$TLS_PIN" "" /no-such-route
expect_host_403 "host: no client cert gets 403 on an unknown route too"
host_curl "$TLS_PIN" "$STRANGER_DIR" /status
expect_host_403 "host: the unlisted cert gets 403"
host_curl "$TLS_PIN" "$CERT_DIR" /status
check_eq "host: the operator cert, pinned, gets 200 on GET /status" 200 "$HOST_CODE"
check_json "host: GET /status answers with the server's JSON" "$(cat "$WORK_DIR/curl.out")" 'has("synced")'
host_curl "$WRONG_PIN" "$CERT_DIR" /status
code=$HOST_CODE
# 90: CURLE_SSL_PINNEDPUBKEYNOTMATCH.
check_eq "host: a wrong --pinnedpubkey is refused by curl (exit 90)" "000 90" "$code $CURL_RC"
code=$(curl -s --max-time 10 -o "$WORK_DIR/curl.out" -w '%{http_code}' "http://localhost:8080/status")
if [ "$code" != 200 ] && ! grep -qF '"synced"' "$WORK_DIR/curl.out" 2>/dev/null; then
  record_pass "host: plain HTTP gets no API answer (HTTP $code)"
else
  record_fail "host: plain HTTP gets no API answer" "HTTP $code: $(head -c 200 "$WORK_DIR/curl.out")"
fi

compose_down

echo
echo "== 7. Host-only TLS: the key comes from secrets/wireguard.key =="
# The tunnel is off, so there is no tunnel identity; the TLS key is derived
# from the operator-supplied file, which is then required.
mkdir -p "$DEPLOY_DIR/secrets"
chmod 0700 "$DEPLOY_DIR/secrets"
check "web app identity generated" "$HARNESS" -mode genkey -private-key-file "$DEPLOY_DIR/secrets/wireguard.key"
chmod 0600 "$DEPLOY_DIR/secrets/wireguard.key"
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    ports:
      - "127.0.0.1:8081:8081"
    volumes:
      - ${CERT_DIR}:/cert:ro
      - ${DEPLOY_DIR}:/etc/homelab-ops/web:ro
    environment:
      - DEPLOYMENT_CONFIG_DIR=/etc/homelab-ops/web
      - API_HOST_PLAIN_HTTP=
      - CLIENT_CERT_PATH=/cert/client.crt
EOF
check "docker compose up (host-only TLS) succeeds" compose up -d
wait_web_ready "$HEALTH" 40
host_pin=""
if wait_fingerprint_logged 1 20; then
  host_pin=$(web_tls_fingerprint)
fi
check "host-only TLS: the web app logged its TLS key fingerprint" test -n "$host_pin"
host_curl "$host_pin" "$CERT_DIR" /status
check_eq "host-only TLS: the operator cert (CLIENT_CERT_PATH, the fallback) gets 200" 200 "$HOST_CODE"
host_curl "$host_pin" "$CERT2_DIR" /status
expect_host_403 "host-only TLS: the second device's cert, not listed now, gets 403"
check "docker compose restart web succeeds" compose restart web
wait_fingerprint_logged 2 40
wait_web_ready "$HEALTH" 40
check_eq "host-only TLS: the fingerprint is unchanged after a restart" "$host_pin" "$(web_tls_fingerprint)"

rm "$DEPLOY_DIR/secrets/wireguard.key"
compose_down
state=""
if compose up -d >/dev/null 2>&1; then
  for _ in $(seq 1 30); do
    state=$(web_exit_state)
    case "$state" in exited*) break ;; esac
    sleep 1
  done
fi
case "$state" in
"exited 0" | "") record_fail "host-only TLS without secrets/wireguard.key: startup fails" "state: ${state:-unknown}" ;;
exited*) record_pass "host-only TLS without secrets/wireguard.key: startup fails (${state#exited })" ;;
*) record_fail "host-only TLS without secrets/wireguard.key: startup fails" "still running after 30s (state: $state)" ;;
esac
check_log "host-only TLS without the key: the log names secrets/wireguard.key" "secrets/wireguard.key: missing, and required while API_HOST_LISTEN_ADDR serves TLS"

summary
