#! /bin/bash
# Validates GH issue #197 (docs/Decisions.md §28): the web app's store is
# encrypted at rest to the operator-held key, an age X25519 identity, and the
# web app is locked after every start until that key is supplied, against the
# real docker compose stack in the default, tunnel-only posture.
#
# The built bootstrap binary is the operator, over its own tunnel peer
# (bootstrap api, #196), and the store file is on a host directory bind
# mounted at STORE_PATH's directory, so the script can read its bytes. The web
# container runs as this user, so the file it writes is this user's too.
#
#   1. Before init: status is refused (423), and the lock state reads
#      no_store.
#   2. init with a generated key, then sync: both succeed, and the store file
#      is age ciphertext (age's header, no sqlite header).
#   3. Restart: status is refused, and the lock state reads locked; init is
#      refused too.
#   4. unlock with a different key fails, and the store file's checksum is
#      unchanged; unlock with the right key, and status is 200 with the
#      synced fleet.
#   5. rotate; restart; unlock with the old key fails, with the new key
#      succeeds and the synced fleet is still there.
#   6. reset (--yes) on a restarted, locked app: the old file is kept aside,
#      byte for byte, and the store starts empty.
#
# cmd/validate-tunnel-harness is used only to generate WireGuard keypairs.
#
# Intended to run INSIDE the devcontainer, from the repo root.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="the web app's store is age ciphertext, locked after every restart until unlock supplies its key; a wrong key changes nothing, and rotate and reset keep what they promise (#197)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go sha256sum"
VALIDATE_DURATION="~90s"
VALIDATE_SUPPORTED_OS="any"

validate_parse_args "$@"
require_compose
require_cmd jq go sha256sum
check_prereqs
cd "$ROOT_DIR" || exit 1

WORK_DIR="$(mktemp -d)"
HARNESS="$WORK_DIR/validate-tunnel-harness"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
OPERATOR_KEY="$WORK_DIR/operator.key"
OVERRIDE="$WORK_DIR/compose-override.yml"
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"
# The deployment config directory (the tunnel's identity) and the store's
# directory, both mounted into the web container.
WEB_DEPLOY_DIR="$WORK_DIR/web"
STORE_DIR="$WORK_DIR/data"
STORE_FILE="$STORE_DIR/web.db"
VALIDATE_UID="$(id -u):$(id -g)"
export WEB_DEPLOY_DIR STORE_DIR VALIDATE_UID
# The store keys: the one init generates, a stranger's, and rotate's.
KEY="$WORK_DIR/store.key"
OTHER_KEY="$WORK_DIR/other.key"
NEW_KEY="$WORK_DIR/store-rotated.key"
RESET_KEY="$WORK_DIR/store-reset.key"

OPERATOR_ADDR=10.100.0.240
WG_ENDPOINT=127.0.0.1:51820
HEALTH=http://localhost:8081

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  compose_down
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# cli <verb> [args...] — one `bootstrap api` run as the operator, through
# the tunnel. stdout goes to $WORK_DIR/cli.out, stderr to $WORK_DIR/cli.err;
# the exit status is the CLI's.
cli() {
  "$BOOTSTRAP_BIN" api "$@" --private-key-file "$OPERATOR_KEY" --local-addr "$OPERATOR_ADDR" \
    --server-public-key "$APP_PUB" --endpoint "$WG_ENDPOINT" \
    >"$WORK_DIR/cli.out" 2>"$WORK_DIR/cli.err"
}

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

# expect_refused <desc> <stderr-substring> <verb> [args...] — the CLI exits
# non-zero, and its stderr carries the substring (the server's answer).
expect_refused() {
  local desc="$1" substr="$2"
  shift 2
  if cli "$@"; then
    record_fail "$desc" "exited 0: $(head -c 200 "$WORK_DIR/cli.out")"
  elif grep -qF -- "$substr" "$WORK_DIR/cli.err"; then
    record_pass "$desc"
  else
    record_fail "$desc" "stderr lacks '$substr': $(tail -n 1 "$WORK_DIR/cli.err")"
  fi
}

# expect_lock_state <desc> <state> — the lock state, which a locked app still
# answers.
expect_lock_state() {
  expect_ok "$1: api lock-state exits 0" lock-state
  check_json "$1: the lock state reads $2" "$CLI_OUT" ".state == \"$2\" and .store == \"file\""
}

# restart_web <desc> — restart the web container and wait for it, and for the
# tunnel to have come back up with the same identity.
restart_web() {
  check "$1: docker compose restart web succeeds" compose restart web
  wait_web_ready "$HEALTH" 40
  wait_log "$1: the restarted web app is locked" "store is locked: the API serves only" 10
}

sha() { sha256sum "$1" 2>/dev/null | awk '{print $1}'; }

echo "== 1. Build bootstrap and the harness; the operator keypair and the tunnel's identity =="
check "build bootstrap" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
check "build validate-tunnel-harness (keygen only)" go build -o "$HARNESS" ./cmd/validate-tunnel-harness
if [ ! -x "$HARNESS" ] || [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: could not build the bootstrap binary or the harness" >&2
  exit 2
fi
OPERATOR_PUB=$("$HARNESS" -mode genkey -private-key-file "$OPERATOR_KEY")
check "operator keypair generated" test -n "$OPERATOR_PUB"
export OPERATOR_PUB
check "web app WireGuard identity written" compose_web_identity "$WEB_DEPLOY_DIR"
mkdir -p "$STORE_DIR"

# Tunnel only (API_HOST_LISTEN_ADDR blanked, as in
# api-serves-operator-peers-over-tunnel-by-default.sh), the store on the
# mounted directory, and no background sync, so every write is this
# script's.
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    user: "${VALIDATE_UID}"
    ports:
      - "127.0.0.1:8081:8081"
    volumes:
      - ${WEB_DEPLOY_DIR}:/etc/homelab-ops/web:ro
      - ${STORE_DIR}:/var/lib/homelab-ops
    environment:
      - API_HOST_LISTEN_ADDR=
      - DEPLOYMENT_CONFIG_DIR=/etc/homelab-ops/web
      - WIREGUARD_ENDPOINT=127.0.0.1:51820
      - WIREGUARD_OPERATOR_PEERS=10.100.0.240=${OPERATOR_PUB}
      - STORE_PATH=/var/lib/homelab-ops/web.db
      - CONFIG_SYNC_INTERVAL=
EOF

echo
echo "== 2. Before init: locked, with no store =="
check "docker compose up --build succeeds" compose up --build -d
wait_web_ready "$HEALTH" 40
wait_log "tunnel API listener is up" "tunnel API listener (1 operator peers)" 10
APP_PUB=$(compose logs web 2>/dev/null | grep -oE 'wireguard public key [A-Za-z0-9+/=]+' | tail -1 | awk '{print $4}')
check "web app logged its WireGuard public key" test -n "$APP_PUB"

expect_refused "api status before init is refused (423)" "423 Locked" status
expect_lock_state "before init" no_store
check "no store file before init" test ! -e "$STORE_FILE"

echo
echo "== 3. init with a generated key, then sync =="
expect_ok "api init exits 0" init --key-file "$KEY"
check_json "api init leaves the store unlocked" "$CLI_OUT" '.state == "unlocked"'
check "api init wrote the generated key (0600)" bash -c "[ \"\$(stat -c %a '$KEY')\" = 600 ] && grep -q '^AGE-SECRET-KEY-1' '$KEY'"
expect_ok "api sync exits 0" sync
check_json "api sync counts the fixture's network and instance" "$CLI_OUT" '.networks == 1 and .instances == 1'
expect_ok "api status exits 0" status
check_json "api status: synced" "$CLI_OUT" '.synced == true'
COMMIT=$(jq -r '.commit // empty' <<<"$CLI_OUT" 2>/dev/null)

check "the store file exists on the volume" test -s "$STORE_FILE"
check_eq "the store file begins with age's header" "age-encryption.org/v1" "$(head -n 1 "$STORE_FILE" 2>/dev/null)"
if grep -qa 'SQLite format 3' "$STORE_FILE" 2>/dev/null; then
  record_fail "the store file is not sqlite" "it carries sqlite's header"
else
  record_pass "the store file is not sqlite"
fi
if grep -qa 'devnode0' "$STORE_FILE" 2>/dev/null; then
  record_fail "the store file doesn't carry the fleet in plaintext" "it names devnode0"
else
  record_pass "the store file doesn't carry the fleet in plaintext"
fi

echo
echo "== 4. Restart: locked; a wrong key changes nothing; the right key resumes =="
restart_web "restart 1"
expect_refused "api status after the restart is refused (423)" "lock state locked" status
expect_lock_state "after the restart" locked
# init is refused once a store exists. It generates its key before asking,
# so the refused run leaves a valid key that isn't the store's: the stranger.
expect_refused "api init on a locked store is refused (409)" "409 Conflict" init --key-file "$OTHER_KEY"
check "the refused init left a different key" bash -c "[ -s '$OTHER_KEY' ] && ! cmp -s '$KEY' '$OTHER_KEY'"

before=$(sha "$STORE_FILE")
expect_refused "api unlock with a different key fails (403)" "403 Forbidden" unlock --key-file "$OTHER_KEY"
check_eq "the wrong key left the store file's checksum unchanged" "$before" "$(sha "$STORE_FILE")"
expect_lock_state "after a wrong key" locked

expect_ok "api unlock with the right key exits 0" unlock --key-file "$KEY"
expect_ok "api status after unlock exits 0" status
check_json "api status after unlock: the prior sync, at $COMMIT" "$CLI_OUT" ".synced == true and .commit == \"$COMMIT\""
expect_ok "api instances after unlock exits 0" instances
check_json "api instances after unlock: the synced fleet" "$CLI_OUT" '[.[].Name] == ["devnode0"]'
check_eq "unlock wrote nothing" "$before" "$(sha "$STORE_FILE")"

echo
echo "== 5. rotate; restart; only the new key unlocks =="
expect_ok "api rotate exits 0" rotate --key-file "$KEY" --new-key-file "$NEW_KEY"
check "rotate rewrote the store file" bash -c "[ \"\$(sha256sum '$STORE_FILE' | awk '{print \$1}')\" != '$before' ]"
restart_web "restart 2"
expect_refused "api unlock with the rotated-out key fails (403)" "403 Forbidden" unlock --key-file "$KEY"
expect_ok "api unlock with the new key exits 0" unlock --key-file "$NEW_KEY"
expect_ok "api status after rotate exits 0" status
check_json "api status after rotate: state kept, at $COMMIT" "$CLI_OUT" ".synced == true and .commit == \"$COMMIT\""

echo
echo "== 6. reset, for a lost key: the old file is kept aside, the store starts empty =="
expect_refused "api reset of an unlocked store is refused (409)" "409 Conflict" reset --yes --new-key-file "$RESET_KEY"
rm -f "$RESET_KEY"
restart_web "restart 3"
before=$(sha "$STORE_FILE")
cli reset --new-key-file "$RESET_KEY" </dev/null
check "api reset without --yes or a y declines" grep -qF "not confirmed" "$WORK_DIR/cli.err"
expect_lock_state "after a declined reset" locked
expect_ok "api reset --yes exits 0" reset --yes --new-key-file "$RESET_KEY"
aside=$(jq -r '.moved_aside // empty' <<<"$CLI_OUT" 2>/dev/null)
check "api reset reports where the old store went" test -n "$aside"
check_eq "the old store file is kept aside, byte for byte" "$before" "$(sha "$STORE_DIR/$(basename "$aside")")"
expect_ok "api status after reset exits 0" status
check_json "api status after reset: an empty store" "$CLI_OUT" '.synced == false'

summary
