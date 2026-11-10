#! /bin/bash
# Validates GH issue #196 (docs/Decisions.md §28): the operator CLI,
# `bootstrap api <verb>`, drives every route of the web app's API through its
# own embedded WireGuard peer, against the real docker compose stack in the
# default, tunnel-only posture (#195).
#
# The built bootstrap binary runs on the host and joins the web app's tunnel
# itself (an in-process userspace peer: no OS WireGuard interface, no root),
# as the operator peer listed in WIREGUARD_OPERATOR_PEERS. It checks that:
#   - status, networks, instances and sync reflect the fixture repo
#     (dev/git-fixture): the synced commit is the repo's HEAD, and a pushed
#     fleet change shows up in sync's diff, status and instances;
#   - seed saves the instance's seed to a 0600 file;
#   - image streams the seeded image to a file, the base image's size, with
#     the seed in it (when the pinned IncusOS base image is cached; otherwise
#     the route's 503 comes back as a non-zero exit, and the streaming checks
#     skip [base-image]);
#   - deploy-agent reaches its handler: an unknown instance is a 404, and a
#     fleet with no agent App is a 422, each a non-zero exit with the server's
#     message (a real deploy needs a booted node:
#     node-tunnel-survives-nat-and-provisions.sh);
#   - a failed sync exits non-zero with the server's message;
#   - a node's key, which the seed hands out, is refused with 403, and a key
#     the web app doesn't know gets no answer.
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

VALIDATE_PROVES="the operator CLI (bootstrap api) drives every API route through its own tunnel peer, and non-operator keys are refused (#196)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go curl [INCUSOS_BASE_IMAGE]"
VALIDATE_DURATION="~2m"
VALIDATE_SUPPORTED_OS="incusos"

validate_parse_args "$@"
require_compose
require_cmd jq go curl
check_prereqs
default_incusos_base_image
cd "$ROOT_DIR" || exit 1

WORK_DIR="$(mktemp -d)"
HARNESS="$WORK_DIR/validate-tunnel-harness"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
CERT_DIR="$WORK_DIR/cert"
OPERATOR_KEY="$WORK_DIR/operator.key"
STRANGER_KEY="$WORK_DIR/stranger.key"
NODE_KEY="$WORK_DIR/node.key"
OVERRIDE="$WORK_DIR/compose-override.yml"
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"
export CERT_DIR
# The web app's WireGuard identity, which it needs with WIREGUARD_ENDPOINT set
# (#197), and the store key api init generates.
WEB_DEPLOY_DIR="$WORK_DIR/web"
export WEB_DEPLOY_DIR
STORE_KEY="$WORK_DIR/store.key"

# The fixture's only instance and its MAC (dev/git-fixture/fleet.yaml).
INSTANCE=devnode0
INSTANCE_MAC="aa:bb:cc:dd:ee:00"
OPERATOR_ADDR=10.100.0.240
WG_ENDPOINT=127.0.0.1:51820
HEALTH=http://localhost:8081

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  compose_down
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# cli_as <key-file> <local-addr> <verb> [args...] — one `bootstrap api` run.
# stdout goes to $WORK_DIR/cli.out, stderr to $WORK_DIR/cli.err; the exit
# status is the CLI's.
cli_as() {
  local key="$1" addr="$2"
  shift 2
  "$BOOTSTRAP_BIN" api "$@" --private-key-file "$key" --local-addr "$addr" \
    --server-public-key "$APP_PUB" --endpoint "$WG_ENDPOINT" \
    >"$WORK_DIR/cli.out" 2>"$WORK_DIR/cli.err"
}

# cli <verb> [args...] — cli_as the operator peer.
cli() { cli_as "$OPERATOR_KEY" "$OPERATOR_ADDR" "$@"; }

# expect_ok <desc> <verb> [args...] — the CLI exits 0; its stdout is in
# $CLI_OUT for the caller's own checks.
expect_ok() {
  local desc="$1"
  shift
  if cli "$@"; then
    record_pass "$desc"
  else
    record_fail "$desc" "exit $?: $(tail -n 1 "$WORK_DIR/cli.err")"
  fi
  CLI_OUT=$(cat "$WORK_DIR/cli.out")
}

# expect_refused <desc> <stderr-substring...> — the last CLI run exited
# non-zero, and its stderr carries every substring (the server's message).
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

fixture_head() {
  compose exec -T config-repo git -C /srv/git/fleet.git rev-parse main 2>/dev/null | tr -d '\r'
}

echo "== 1. Build bootstrap, operator and stranger keypairs, the break-glass cert =="
check "build bootstrap" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
check "build validate-tunnel-harness (keygen only)" go build -o "$HARNESS" ./cmd/validate-tunnel-harness
if [ ! -x "$HARNESS" ] || [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: could not build the bootstrap binary or the harness" >&2
  exit 2
fi
OPERATOR_PUB=$("$HARNESS" -mode genkey -private-key-file "$OPERATOR_KEY")
check "operator keypair generated" test -n "$OPERATOR_PUB"
"$HARNESS" -mode genkey -private-key-file "$STRANGER_KEY" >/dev/null
check "stranger keypair generated" test -s "$STRANGER_KEY"
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "operator-cli-drives-api-over-tunnel"
export OPERATOR_PUB

echo
echo "== 2. Bring up the stack: tunnel only, one operator peer =="
# As api-serves-operator-peers-over-tunnel-by-default.sh: blanking
# API_HOST_LISTEN_ADDR restores the shipped default, so the tunnel is the
# only way in.
BASE_IMAGE_READY=0
BASE_IMAGE_MOUNT=""
BASE_IMAGE_ENV=""
if have_env_file INCUSOS_BASE_IMAGE; then
  BASE_IMAGE_READY=1
  BASE_IMAGE_ABS="$(cd "$(dirname "$INCUSOS_BASE_IMAGE")" && pwd)/$(basename "$INCUSOS_BASE_IMAGE")"
  export BASE_IMAGE_ABS
  # shellcheck disable=SC2016 # compose interpolates it, not bash
  BASE_IMAGE_MOUNT='      - ${BASE_IMAGE_ABS}:/data/incusos-base.img:ro'
  BASE_IMAGE_ENV='      - BASE_IMAGE_PATH=/data/incusos-base.img'
else
  echo "NOTE: $(base_image_skip_reason). The image-streaming checks will be skipped."
fi
cat >"$OVERRIDE" <<EOF
services:
  web:
    ports:
      - "127.0.0.1:8081:8081"
    volumes:
      - \${CERT_DIR}:/cert:ro
      - \${WEB_DEPLOY_DIR}:/etc/homelab-ops/web:ro
$BASE_IMAGE_MOUNT
    environment:
      - API_HOST_LISTEN_ADDR=
      - CLIENT_CERT_PATH=/cert/client.crt
      - WIREGUARD_ENDPOINT=127.0.0.1:51820
      - DEPLOYMENT_CONFIG_DIR=/etc/homelab-ops/web
      - WIREGUARD_OPERATOR_PEERS=$OPERATOR_ADDR=\${OPERATOR_PUB}
$BASE_IMAGE_ENV
EOF
check "web app WireGuard identity written" compose_web_identity "$WEB_DEPLOY_DIR"
check "docker compose up --build succeeds" compose up --build -d
wait_web_ready "$HEALTH" 40
wait_log "tunnel API listener is up" "tunnel API listener (1 operator peers)" 10

APP_PUB=$(compose logs web 2>/dev/null | grep -oE 'wireguard public key [A-Za-z0-9+/=]+' | tail -1 | awk '{print $4}')
check "web app logged its WireGuard public key" test -n "$APP_PUB"

echo
echo "== 3. The store is locked until api init (#197) =="
expect_ok "api lock-state exits 0 while locked" lock-state
check_json "api lock-state reads no_store before init" "$CLI_OUT" '.state == "no_store" and .store == "memory"'
cli status
expect_refused $? "api status before init exits non-zero with the server's 423" "423 Locked" "lock state no_store"
expect_ok "api init exits 0" init --key-file "$STORE_KEY"
check_json "api init leaves the store unlocked" "$CLI_OUT" '.state == "unlocked"'
check "api init wrote the generated store key" grep -q '^AGE-SECRET-KEY-1' "$STORE_KEY"

echo
echo "== 4. sync, status, networks, instances reflect the fixture repo =="
head1=$(fixture_head)
check "read the fixture repo's HEAD" test -n "$head1"

expect_ok "api sync exits 0" sync
check_json "api sync reports the fixture repo's HEAD" "$CLI_OUT" ".commit == \"$head1\""
check_json "api sync counts the fixture's 1 network and 1 instance" "$CLI_OUT" '.networks == 1 and .instances == 1'

expect_ok "api status exits 0" status
check_json "api status: synced at the fixture repo's HEAD" "$CLI_OUT" ".synced == true and .commit == \"$head1\""

expect_ok "api networks exits 0" networks
check_json "api networks lists dev-lan" "$CLI_OUT" '[.[].Name] == ["dev-lan"]'

expect_ok "api instances exits 0" instances
check_json "api instances lists $INSTANCE with a tunnel address" "$CLI_OUT" \
  "[.[] | select(.Name == \"$INSTANCE\" and (.TunnelIP // \"\") != \"\")] | length == 1"
node_addr=$(jq -r --arg n "$INSTANCE" '.[] | select(.Name == $n) | .TunnelIP // empty' <<<"$CLI_OUT" 2>/dev/null)

echo
echo "== 5. seed saves the instance's seed to a private file =="
seed_file="$WORK_DIR/$INSTANCE-seed.json"
expect_ok "api seed $INSTANCE --output <file> exits 0" seed "$INSTANCE" --output "$seed_file"
check_eq "the seed file is mode 600" 600 "$(stat -c %a "$seed_file" 2>/dev/null)"
seed_json=$(cat "$seed_file" 2>/dev/null)
check_json "the seed file holds all four seed documents" "$seed_json" \
  '[.install_yaml, .network_yaml, .applications_yaml, .incus_yaml] | all(type == "string" and length > 0)'
check "the seed's network.yaml carries $INSTANCE's MAC" grep -qF "$INSTANCE_MAC" <<<"$(jq -r '.network_yaml // empty' <<<"$seed_json" 2>/dev/null)"

echo
echo "== 6. image streams the seeded image to a file =="
img_file="$WORK_DIR/$INSTANCE.img"
if [ "$BASE_IMAGE_READY" -eq 1 ]; then
  expect_ok "api image $INSTANCE --output <file> exits 0" image "$INSTANCE" --output "$img_file"
  base_bytes=$(stat -c %s "$INCUSOS_BASE_IMAGE")
  check_eq "the image is the base image's size (in-place seed injection)" "$base_bytes" "$(stat -c %s "$img_file" 2>/dev/null)"
  check "the CLI reports the bytes it saved" grep -qF "($base_bytes bytes)" "$WORK_DIR/cli.err"
  check_eq "the image file is mode 600" 600 "$(stat -c %a "$img_file" 2>/dev/null)"
  check "the image carries $INSTANCE's seeded MAC" grep -aqF "$INSTANCE_MAC" "$img_file"
  rm -f "$img_file"
  # Only with a base image: without one the handler answers 503 before it
  # looks the instance up.
  cli image does-not-exist --output "$img_file"
  expect_refused $? "api image of an unknown instance exits non-zero with a 404" "404"
else
  cli image "$INSTANCE" --output "$img_file"
  expect_refused $? "api image without a base image exits non-zero with the server's 503 message" \
    "503" "image generation not configured"
  check "a refused image download leaves no file" test ! -e "$img_file"
  for _desc in \
    "api image $INSTANCE --output <file> exits 0" \
    "the image is the base image's size (in-place seed injection)" \
    "the image carries $INSTANCE's seeded MAC" \
    "api image of an unknown instance exits non-zero with a 404"; do
    skip_check "$_desc" base-image "$(base_image_skip_reason)"
  done
fi

echo
echo "== 7. deploy-agent reaches its handler =="
cli deploy-agent does-not-exist
expect_refused $? "api deploy-agent of an unknown instance exits non-zero with a 404" "POST /instances/does-not-exist/deploy-agent" "404"

echo
echo "== 8. A pushed fleet change shows up through the CLI =="
# A second instance, and no agent App: deploy-agent then has nothing to deploy.
FLEET2=$(
  cat <<'EOF'
kind: Network
name: dev-lan
cidr: 10.0.0.0/24
gateway: 10.0.0.1
dhcp_excluded_range: 10.0.0.200-10.0.0.250
dns: [10.0.0.1]
---
kind: Instance
name: devnode0
mac: aa:bb:cc:dd:ee:00
network: dev-lan
static_ip: 10.0.0.210
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
---
kind: Instance
name: devnode1
mac: aa:bb:cc:dd:ee:01
network: dev-lan
static_ip: 10.0.0.211
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
EOF
)
check "push a fleet with a second instance and no agent App" push_fleet "operator-cli" "$FLEET2"
head2=$(fixture_head)
check "the fixture repo moved to a new commit" test -n "$head2" -a "$head2" != "$head1"

expect_ok "api sync exits 0 after the push" sync
check_json "api sync reports the new commit, one instance added, the agent App removed" "$CLI_OUT" \
  ".commit == \"$head2\" and .instances == 2 and .diff.instances_added == 1 and .diff.apps_removed == 1"
expect_ok "api status exits 0 after the push" status
check_json "api status: synced at the new commit" "$CLI_OUT" ".synced == true and .commit == \"$head2\""
expect_ok "api instances exits 0 after the push" instances
check_json "api instances lists devnode0 and devnode1" "$CLI_OUT" '[.[].Name] | sort == ["devnode0", "devnode1"]'

cli deploy-agent "$INSTANCE"
expect_refused $? "api deploy-agent with no agent App exits non-zero with the server's 422 message" \
  "422" "no agent App declared"

echo
echo "== 9. A failed sync exits non-zero with the server's message =="
check "push a fleet that doesn't parse" push_fleet "operator-cli" "kind: [this is not a fleet"
cli sync
expect_refused $? "api sync of a broken fleet exits non-zero with the server's 502 message" "502" "sync failed"
cli status
check_json "api status still reports the last good commit" "$(cat "$WORK_DIR/cli.out")" ".commit == \"$head2\""

echo
echo "== 10. Keys that aren't an operator's are refused =="
node_priv=$(jq -r '.network_yaml // empty' <<<"$seed_json" 2>/dev/null | grep -oE 'private_key: *[A-Za-z0-9+/=]+' | head -1 | awk '{print $2}')
check "node WireGuard key and overlay address read from the seed and instances" test -n "$node_priv" -a -n "$node_addr"
printf '%s\n' "$node_priv" >"$NODE_KEY"
for verb in status instances "seed $INSTANCE -o -"; do
  # shellcheck disable=SC2086 # verb carries its arguments
  cli_as "$NODE_KEY" "$node_addr" $verb
  expect_refused $? "node peer ($node_addr): api ${verb%% *} exits non-zero with 403" "403" "not an operator peer"
done
check_log "the web log records the refusal" "refused GET \"/status\" from \"$node_addr:"

"$BOOTSTRAP_BIN" api status --private-key-file "$STRANGER_KEY" --local-addr 10.100.0.241 \
  --server-public-key "$APP_PUB" --endpoint "$WG_ENDPOINT" --connect-timeout 8s \
  >"$WORK_DIR/cli.out" 2>"$WORK_DIR/cli.err"
expect_refused $? "a key the web app doesn't know gets no answer" "no answer from 10.100.0.1:80 within 8s"

summary
