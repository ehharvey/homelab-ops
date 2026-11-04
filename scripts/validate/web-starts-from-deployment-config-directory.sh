#! /bin/bash
# Validates GH issue #241 (docs/Deployment.md, docs/Decisions.md §29): the web
# app starts from a deployment config directory, against the real docker
# compose stack.
#
# The web service's committed environment is replaced, not merged: the only
# settings it gets come from a copy of examples/webapp/, plus
# DEPLOYMENT_CONFIG_DIR and one override. The copy is filled in the way an
# operator would: a generated operator peer in operator-peers, the break-glass
# cert's public half as client.crt, a generated WireGuard identity in
# secrets/wireguard.key (0700/0600; #197 will load it, nothing does yet), and
# CONFIG_REPO_URL pointed at the dev fixture repo.
#
#   1. It starts: the tunnel comes up, the operator peer from operator-peers
#      reaches the API over it, the sync from web.env's repo runs, and the
#      seed route renders with client.crt.
#   2. An env var overrides web.env: WIREGUARD_ENDPOINT is web.env's
#      webapp.example.net placeholder, and the environment's 127.0.0.1 wins.
#   3. It fails fast: the web app exits non-zero, naming the problem, when a
#      required setting is removed (WIREGUARD_ENDPOINT, required while
#      operator peers are listed), when CONFIG_REPO_URL carries credentials,
#      and when secrets/wireguard.key is readable beyond its owner.
#
# The tunnel side is driven by cmd/validate-tunnel-harness's http mode, as in
# api-serves-operator-peers-over-tunnel-by-default.sh: only another WireGuard
# peer can dial the web app's in-process tunnel.
#
# Intended to run INSIDE the devcontainer, from the repo root.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="the web app starts from a copy of examples/webapp/ alone, env vars override web.env, and a removed required setting stops startup naming it (#241)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go curl"
VALIDATE_DURATION="~60s"
VALIDATE_SUPPORTED_OS="incusos"

validate_parse_args "$@"
require_compose
require_cmd jq go curl
check_prereqs
cd "$ROOT_DIR" || exit 1

WORK_DIR="$(mktemp -d)"
HARNESS="$WORK_DIR/validate-tunnel-harness"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
CERT_DIR="$WORK_DIR/cert"
OPERATOR_KEY="$WORK_DIR/operator.key"
OVERRIDE="$WORK_DIR/compose-override.yml"
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"
# The deployment config directory, mounted read-only where an appliance
# would have it.
DEPLOY_DIR="$WORK_DIR/web"
export DEPLOY_DIR

# The fixture's only instance (dev/git-fixture/fleet.yaml).
INSTANCE=devnode0
OPERATOR_ADDR=10.100.0.240
WEBAPP_ADDR=10.100.0.1
WG_ENDPOINT=127.0.0.1:51820
HEALTH=http://localhost:8081

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  compose_down
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# write_override [extra env lines...] — the web service with its committed
# environment replaced (!override) by DEPLOYMENT_CONFIG_DIR and the given
# lines alone. CONFIG_REPO_PRIVATE=true is there because the dev fixture's
# fleet is git-addressed, which #199 refuses from a repo not declared
# private; until #199 lands nothing reads it, and an env var cmd/web doesn't
# know is ignored (only web.env's keys are checked).
write_override() {
  {
    cat <<'EOF'
services:
  web:
    ports:
      - "127.0.0.1:8081:8081"
    volumes:
      - ${DEPLOY_DIR}:/etc/homelab-ops/web:ro
    environment: !override
      - DEPLOYMENT_CONFIG_DIR=/etc/homelab-ops/web
      - CONFIG_REPO_PRIVATE=true
EOF
    local line
    for line in "$@"; do
      printf '      - %s\n' "$line"
    done
  } >"$OVERRIDE"
}

# set_web_env <key> <value> — replace KEY's line in the copy's web.env, as an
# operator filling in their values would. An empty value deletes the line.
set_web_env() {
  local key="$1" value="$2" tmp="$WORK_DIR/web.env.tmp"
  grep -v "^$key=" "$DEPLOY_DIR/web.env" >"$tmp"
  [ -n "$value" ] && printf '%s=%s\n' "$key" "$value" >>"$tmp"
  cat "$tmp" >"$DEPLOY_DIR/web.env"
}

# tunnel_req <method> <path> — one request as the operator, through the
# tunnel. Prints "<status>\n<body>"; fails if nothing answers.
tunnel_req() {
  "$HARNESS" -mode http -private-key-file "$OPERATOR_KEY" -local-addr "$OPERATOR_ADDR" \
    -peer-public-key "$APP_PUB" -peer-tunnel-ip "$WEBAPP_ADDR" -peer-endpoint "$WG_ENDPOINT" \
    -method "$1" -path "$2" -timeout 30s 2>"$WORK_DIR/harness.err"
}

# expect_route <desc> <want-status> <method> <path> [body-substring]
expect_route() {
  local desc="$1" want="$2" method="$3" path="$4" substr="${5:-}"
  local resp code body
  if ! resp=$(tunnel_req "$method" "$path"); then
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

# web_exit_state — "<State> <ExitCode>" of the web container.
web_exit_state() {
  compose ps -a --format json web 2>/dev/null | jq -rs '.[0] | "\(.State) \(.ExitCode)"' 2>/dev/null
}

# expect_startup_failure <desc> <message> — start the stack as configured,
# and require the web app to exit non-zero with <message> in its log.
expect_startup_failure() {
  local desc="$1" message="$2" state="" _
  compose_down
  if ! compose up -d >/dev/null 2>&1; then
    record_fail "$desc" "docker compose up failed"
    return
  fi
  for _ in $(seq 1 30); do
    state=$(web_exit_state)
    case "$state" in exited*) break ;; esac
    sleep 1
  done
  case "$state" in
  "exited 0") record_fail "$desc: exits non-zero" "it exited 0" ;;
  exited*) record_pass "$desc: exits non-zero (${state#exited })" ;;
  *) record_fail "$desc: exits non-zero" "still running after 30s (state: ${state:-unknown})" ;;
  esac
  check_log "$desc: the log names it" "$message"
}

echo "== 1. Build the harness and bootstrap; fill in a copy of examples/webapp/ =="
check "build validate-tunnel-harness" go build -o "$HARNESS" ./cmd/validate-tunnel-harness
check "build bootstrap" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
if [ ! -x "$HARNESS" ] || [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: could not build the harness or bootstrap binary" >&2
  exit 2
fi
cp -r examples/webapp "$DEPLOY_DIR"
OPERATOR_PUB=$("$HARNESS" -mode genkey -private-key-file "$OPERATOR_KEY")
check "operator keypair generated" test -n "$OPERATOR_PUB"
printf '%s=%s  # validate harness\n' "$OPERATOR_ADDR" "$OPERATOR_PUB" >>"$DEPLOY_DIR/operator-peers"
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "web-starts-from-deployment-config-directory"
cp "$CERT_DIR/client.crt" "$DEPLOY_DIR/client.crt"
mkdir -m 0700 "$DEPLOY_DIR/secrets"
check "web app identity generated" "$HARNESS" -mode genkey -private-key-file "$DEPLOY_DIR/secrets/wireguard.key"
chmod 0600 "$DEPLOY_DIR/secrets/wireguard.key"
set_web_env CONFIG_REPO_URL git://config-repo/fleet.git
check "web.env keeps its placeholder endpoint" grep -qx 'WIREGUARD_ENDPOINT=webapp.example.net:51820' "$DEPLOY_DIR/web.env"

echo
echo "== 2. Start from the directory, with WIREGUARD_ENDPOINT overridden by env =="
write_override "WIREGUARD_ENDPOINT=$WG_ENDPOINT"
web_env=$(compose config --format json 2>/dev/null | jq -r '.services.web.environment | keys | join(",")')
check_eq "the web service's environment is the directory and one override" \
  "CONFIG_REPO_PRIVATE,DEPLOYMENT_CONFIG_DIR,WIREGUARD_ENDPOINT" "$web_env"
check "docker compose up --build succeeds" compose up --build -d
wait_web_ready "$HEALTH" 40
check_log "the web app read the directory" 'deployment config directory "/etc/homelab-ops/web"'
wait_log "the tunnel API listener admits operator-peers' one peer" "tunnel API listener (1 operator peers) listening on \"$WEBAPP_ADDR:80\"" 10
check_log "the env's WIREGUARD_ENDPOINT overrides web.env's" "endpoint \"$WG_ENDPOINT\""
if _grep_web_log "webapp.example.net"; then
  record_fail "web.env's placeholder endpoint is not used" "the web log names webapp.example.net"
else
  record_pass "web.env's placeholder endpoint is not used"
fi
if _grep_web_log "host API listener"; then
  record_fail "no host API listener: the committed API_HOST_LISTEN_ADDR is gone" "the web log shows one"
else
  record_pass "no host API listener: the committed API_HOST_LISTEN_ADDR is gone"
fi
wait_log "web.env's CONFIG_REPO_URL and CONFIG_SYNC_INTERVAL drive the startup sync" "synced commit" 20

APP_PUB=$(compose logs web 2>/dev/null | grep -oE 'wireguard public key [A-Za-z0-9+/=]+' | tail -1 | awk '{print $4}')
check "web app logged its WireGuard public key" test -n "$APP_PUB"

expect_route "operator (from operator-peers): GET /status over the tunnel" 200 GET /status '"synced":true'
expect_route "operator: POST /instances/$INSTANCE/seed renders with client.crt" 200 POST "/instances/$INSTANCE/seed" 'network_yaml'

echo
echo "== 3. Fail fast: the web app exits non-zero, naming the problem =="
# From here on the environment holds DEPLOYMENT_CONFIG_DIR alone, so every
# setting is the directory's.
write_override

echo "-- a removed required setting --"
# operator-peers lists a peer, and they reach the API over the tunnel, so
# WIREGUARD_ENDPOINT is required.
set_web_env WIREGUARD_ENDPOINT ""
check "web.env no longer sets WIREGUARD_ENDPOINT" bash -c "! grep -q '^WIREGUARD_ENDPOINT=' '$DEPLOY_DIR/web.env'"
expect_startup_failure "WIREGUARD_ENDPOINT removed" "WIREGUARD_ENDPOINT: required when operator peers are listed"
set_web_env WIREGUARD_ENDPOINT webapp.example.net:51820

echo "-- credentials in CONFIG_REPO_URL --"
set_web_env CONFIG_REPO_URL "https://operator:s3cret-token@github.com/you/fleet.git"
expect_startup_failure "CONFIG_REPO_URL with a credential" "CONFIG_REPO_URL: carries a credential"
check_log "the refusal points at #218" "git credentials are #218's"
if _grep_web_log "s3cret-token"; then
  record_fail "the refusal doesn't echo the token" "the web log contains it"
else
  record_pass "the refusal doesn't echo the token"
fi
set_web_env CONFIG_REPO_URL git://config-repo/fleet.git

echo "-- a secret readable beyond its owner --"
chmod 0644 "$DEPLOY_DIR/secrets/wireguard.key"
expect_startup_failure "secrets/wireguard.key at 0644" "secrets/wireguard.key: mode 0644 is readable beyond its owner"
chmod 0600 "$DEPLOY_DIR/secrets/wireguard.key"

summary
