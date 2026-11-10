#! /bin/bash
# Validates GH issue #242 (docs/Decisions.md §29, "Where install media is
# built"): with the web app's BASE_IMAGE_PATH unset, so its image route
# answers 503, the operator CLI still builds both client-side kinds of
# install media from the seed it downloads over its own tunnel peer, against
# the real docker compose stack:
#   - `bootstrap api image --base-image` (a local build, through
#     hostos.Backend.RenderImage and flasher-tool): the image is the base
#     image's size, carries the instance's seeded MAC, is mode 600, and the
#     seed files it was built from are gone from TMPDIR afterwards;
#   - `bootstrap api seed-media` (hostos.Backend.RenderSeedMedia): an ISO 9660
#     volume labelled SEED_DATA, mode 600, and the CLI says to remove it after
#     install.
#
# This proves the media is built from the web app's seed; booting it is
# node-boots-from-separate-seed-media.sh's job for seed media, and the local
# build's image is made by the same RenderImage path build-image takes, which
# node-tunnel-survives-nat-and-provisions.sh boots.
#
# The local build needs the pinned IncusOS base image (`make incusos-base`) and
# flasher-tool; without either its checks skip [base-image] / [flasher-tool].
# The seed media needs neither.
#
# Intended to run INSIDE the devcontainer, from the repo root.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="with BASE_IMAGE_PATH unset, the CLI builds a seeded image locally and writes SEED_DATA seed media from a downloaded seed (#242)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go blkid [flasher-tool] [INCUSOS_BASE_IMAGE]"
VALIDATE_DURATION="~2m"
VALIDATE_SUPPORTED_OS="incusos"

validate_parse_args "$@"
require_compose
require_cmd jq go blkid
check_prereqs
default_incusos_base_image
cd "$ROOT_DIR" || exit 1

WORK_DIR="$(mktemp -d)"
HARNESS="$WORK_DIR/validate-tunnel-harness"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
CERT_DIR="$WORK_DIR/cert"
OPERATOR_KEY="$WORK_DIR/operator.key"
OVERRIDE="$WORK_DIR/compose-override.yml"
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"
# The CLI's temporary files land here, so the run can check none outlive it.
CLI_TMP="$WORK_DIR/tmp"
mkdir -p "$CLI_TMP"
export CERT_DIR

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

# cli <verb> [args...] — one `bootstrap api` run as the operator peer.
# stdout goes to $WORK_DIR/cli.out, stderr to $WORK_DIR/cli.err; the exit
# status is the CLI's.
cli() {
  TMPDIR="$CLI_TMP" "$BOOTSTRAP_BIN" api "$@" --private-key-file "$OPERATOR_KEY" --local-addr "$OPERATOR_ADDR" \
    --server-public-key "$APP_PUB" --endpoint "$WG_ENDPOINT" \
    >"$WORK_DIR/cli.out" 2>"$WORK_DIR/cli.err"
}

# expect_ok <desc> <verb> [args...] — the CLI exits 0.
expect_ok() {
  local desc="$1"
  shift
  if cli "$@"; then
    record_pass "$desc"
  else
    record_fail "$desc" "exit $?: $(tail -n 3 "$WORK_DIR/cli.err" | tr '\n' '|')"
  fi
}

echo "== 1. Build bootstrap, the operator keypair, the break-glass cert =="
check "build bootstrap" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
check "build validate-tunnel-harness (keygen only)" go build -o "$HARNESS" ./cmd/validate-tunnel-harness
if [ ! -x "$HARNESS" ] || [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: could not build the bootstrap binary or the harness" >&2
  exit 2
fi
OPERATOR_PUB=$("$HARNESS" -mode genkey -private-key-file "$OPERATOR_KEY")
check "operator keypair generated" test -n "$OPERATOR_PUB"
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "cli-builds-install-media-from-downloaded-seed"
export OPERATOR_PUB

echo
echo "== 2. Bring up the stack: tunnel only, one operator peer, no BASE_IMAGE_PATH =="
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
wait_log "tunnel API listener is up" "tunnel API listener (1 operator peers)" 10

APP_PUB=$(compose logs web 2>/dev/null | grep -oE 'wireguard public key [A-Za-z0-9+/=]+' | tail -1 | awk '{print $4}')
check "web app logged its WireGuard public key" test -n "$APP_PUB"

expect_ok "api sync exits 0" sync

cli image "$INSTANCE" --output "$WORK_DIR/served.img"
rc=$?
if [ "$rc" -ne 0 ] && grep -qF "503" "$WORK_DIR/cli.err" && grep -qF "BASE_IMAGE_PATH" "$WORK_DIR/cli.err"; then
  record_pass "the web app's image route answers 503: server-side builds are off"
else
  record_fail "the web app's image route answers 503: server-side builds are off" "exit $rc: $(tail -n 1 "$WORK_DIR/cli.err")"
fi

echo
echo "== 3. api seed-media writes SEED_DATA seed media from the downloaded seed =="
iso="$WORK_DIR/$INSTANCE-seed.iso"
expect_ok "api seed-media $INSTANCE --output <file> exits 0" seed-media "$INSTANCE" --output "$iso"
check_eq "the seed media is mode 600" 600 "$(stat -c %a "$iso" 2>/dev/null)"
check "the CLI says to remove the media after install" grep -qF "Remove the media once the install finishes" "$WORK_DIR/cli.err"
check "the seed media carries $INSTANCE's seeded MAC" grep -aqF "$INSTANCE_MAC" "$iso"
check "the seed media holds install.yaml at its root (ISO 9660 INSTALL.YAML;1)" grep -aqF "INSTALL.YAML;1" "$iso"
# udev files a volume under /dev/disk/by-label/ from what blkid probes, and
# that is where incus-osd looks for SEED_DATA.
probe=$(blkid -p -o export "$iso" 2>&1)
# shellcheck disable=SC2016 # $1 is the inner bash's argument, not pasted in
check "blkid sees an iso9660 filesystem labelled SEED_DATA" \
  bash -c 'grep -qx TYPE=iso9660 <<<"$1" && grep -qx LABEL=SEED_DATA <<<"$1"' _ "$probe"

echo
echo "== 4. api image --base-image builds the image here from the downloaded seed =="
_local_checks=(
  "api image $INSTANCE --base-image <base> --output <file> exits 0"
  "the local image is the base image's size (in-place seed injection)"
  "the local image file is mode 600"
  "the local image carries $INSTANCE's seeded MAC"
  "the base image is unmodified (no seeded MAC)"
  "no seed files are left in the CLI's TMPDIR"
)
if ! have_env_file INCUSOS_BASE_IMAGE; then
  for _desc in "${_local_checks[@]}"; do
    skip_check "$_desc" base-image "$(base_image_skip_reason)"
  done
elif ! have_cmd flasher-tool; then
  for _desc in "${_local_checks[@]}"; do
    skip_check "$_desc" flasher-tool "flasher-tool not found; install via 'go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool@latest'"
  done
else
  img="$WORK_DIR/$INSTANCE.img"
  expect_ok "${_local_checks[0]}" image "$INSTANCE" --base-image "$INCUSOS_BASE_IMAGE" --output "$img"
  check_eq "${_local_checks[1]}" "$(stat -c %s "$INCUSOS_BASE_IMAGE")" "$(stat -c %s "$img" 2>/dev/null)"
  check_eq "${_local_checks[2]}" 600 "$(stat -c %a "$img" 2>/dev/null)"
  check "${_local_checks[3]}" grep -aqF "$INSTANCE_MAC" "$img"
  check "${_local_checks[4]}" bash -c "! grep -aqF '$INSTANCE_MAC' '$INCUSOS_BASE_IMAGE'"
  check_eq "${_local_checks[5]}" "" "$(ls -A "$CLI_TMP")"
  rm -f "$img"
fi

summary
