#! /bin/bash
# Validates GH issue #39: GET /instances/{name}/image regenerates and streams a
# bootable .img reflecting the instance's current seed/cert/IP, generated fresh
# on each request (no caching — docs/Decisions.md §3).
#
# Drives the real docker compose stack (web + dev/git-fixture git remote) and
# hits the route with curl, following seed-route-renders-static-ip.sh's pattern: the web
# app's deployment config (the break-glass cert AND the base IncusOS image) is
# supplied via a scoped compose override written here, NOT a permanent
# docker-compose.yml change. The web image built by `compose up --build` is the
# one that carries flasher-tool (see Dockerfile), so this also exercises
# flasher-tool running inside the distroless container end-to-end.
#
# The image checks need a real base IncusOS raw image: point INCUSOS_BASE_IMAGE
# at a local copy. Those checks skip with a clear message if it's unset/missing
# (same convention as node-boots-and-trusts-bootstrap-cert.sh). A real IncusOS image injects the
# seed in place at a fixed offset, so the output is the SAME size as the base —
# this asserts that equality plus the presence of the seeded MAC in the output
# bytes (absent from the base), and stops at "structurally a seeded disk image":
# a full boot-and-cert-trust check belongs to #40, not here.
#
# The route may not exist yet — until #39 lands, expect the image checks to fail
# with a 404/503 against GET /instances/.../image (same convention as
# seed-route-renders-static-ip.sh before #36 landed).
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires docker
# compose, curl, jq, go, and openssl.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="GET /instances/{name}/image streams a freshly seeded, correctly-sized .img (#39)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose curl jq go openssl [INCUSOS_BASE_IMAGE]"
VALIDATE_DURATION="~1m"

validate_parse_args "$@"
cd "$ROOT_DIR"

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$ROOT_DIR/bin/bootstrap"
CERT_DIR="$WORK_DIR/cert"
export CERT_DIR
OVERRIDE="$(mktemp /tmp/compose-image-override.XXXXXX.yml)"
# lib-compose.sh's compose() picks this up; scripts that need no scoped
# override simply leave it unset.
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"

# devnode0's MAC from the committed fixture (dev/git-fixture/fleet.yaml). Used
# as the positive marker that the seed really was injected into the image.
DEVNODE0_MAC="aa:bb:cc:dd:ee:00"

# Base override just mounts the cert (as #36 does). The base-image mount +
# BASE_IMAGE_PATH are appended below only when INCUSOS_BASE_IMAGE is usable, so
# the stack still comes up (and the route reports 503 "not configured") when
# it isn't.
#
# WIREGUARD_ENDPOINT is required for the same reason as the cert: #107 added a
# second gate to this route ("wireguard not configured"), and this script never
# set it, so the image assertions below have been failing since. It is a
# *config* gate, not a hardware one — internal/wireguard runs the tunnel over
# netstack.CreateNetTUN + conn.NewDefaultBind() (userspace, no NET_ADMIN, an
# unprivileged net.ListenUDP), and resolveInstanceSeed mints the node
# credential offline without dialing anything. So a loopback endpoint satisfies
# it; the port is already published by docker-compose.yml.
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

BASE_IMAGE_READY=0
if [ -z "${INCUSOS_BASE_IMAGE:-}" ]; then
  echo "NOTE: INCUSOS_BASE_IMAGE unset — image-generation checks will be skipped."
elif [ ! -f "${INCUSOS_BASE_IMAGE}" ]; then
  echo "NOTE: INCUSOS_BASE_IMAGE=${INCUSOS_BASE_IMAGE} not found — image-generation checks will be skipped."
else
  BASE_IMAGE_READY=1
  BASE_IMAGE_ABS="$(cd "$(dirname "$INCUSOS_BASE_IMAGE")" && pwd)/$(basename "$INCUSOS_BASE_IMAGE")"
  export BASE_IMAGE_ABS
  # Rewrite the override with the cert mount, the read-only base-image mount,
  # and both env vars together.
  cat >"$OVERRIDE" <<'EOF'
services:
  web:
    volumes:
      - ${CERT_DIR}:/cert:ro
      - ${BASE_IMAGE_ABS}:/data/incusos-base.img:ro
    environment:
      - CLIENT_CERT_PATH=/cert/client.crt
      - BASE_IMAGE_PATH=/data/incusos-base.img
      - WIREGUARD_ENDPOINT=127.0.0.1:51820
EOF
fi

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
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "image-route-streams-seeded-image"

echo
echo "== 2. Bring up the real stack (web image carries flasher-tool) =="
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
echo "== 4. GET /instances/devnode0/image streams a seeded .img =="
if [ "$BASE_IMAGE_READY" -ne 1 ]; then
  # A 3.2 GB base image is something the operator supplies, not something the
  # route can be wrong about — so its absence is a skip, tagged so CI can bless
  # it (`--allow-skip base-image`) while still failing on any *other* skip.
  # That distinction is what would have caught #107's gate the day it landed:
  # a route returning 503 produces failures here, not skips.
  for _desc in \
    "image route returns 200" \
    "response is an attachment" \
    "downloaded .img is a non-empty disk image" \
    "downloaded .img is exactly the base image's size" \
    "downloaded .img carries devnode0's seeded MAC" \
    "the base image does not carry that MAC" \
    "unknown instance 404s"; do
    skip_check "$_desc" base-image "INCUSOS_BASE_IMAGE not usable"
  done
else
  out_img="$WORK_DIR/devnode0.img"
  headers="$WORK_DIR/headers.txt"
  status=$(curl -s -D "$headers" -o "$out_img" -w '%{http_code}' "$base_url/instances/devnode0/image")

  check "image route returns 200" bash -c "[ '$status' = '200' ]"
  check "response is an attachment (Content-Disposition)" \
    bash -c "grep -qi 'content-disposition:.*attachment' '$headers'"
  check "downloaded .img is non-empty" bash -c "[ -s '$out_img' ]"

  # A real IncusOS image is at least tens of MiB; guard against a truncated
  # download or an error body written to the file.
  out_bytes=$(stat -c%s "$out_img")
  base_bytes=$(stat -c%s "$INCUSOS_BASE_IMAGE")
  check "downloaded .img is a plausibly-sized disk image (>= 1 MiB)" \
    bash -c "[ '$out_bytes' -ge $((1 << 20)) ]"

  # In-place seed injection keeps the size identical to the base, so equality is
  # the assertion to make here.
  #
  # This replaces a `cmp -s` check that asserted the download merely *differed*
  # from the base. That passed whenever the two files were not byte-identical —
  # which a 40-byte 503 error body trivially is not. It therefore reported PASS
  # for the exact failure mode it existed to catch, and did so for weeks while
  # the route was 503ing on #107's WireGuard gate. See #115.
  check "downloaded .img is exactly the base image's size (in-place injection)" \
    bash -c "[ '$out_bytes' -eq '$base_bytes' ]"

  # Positive proof the seed was actually written, rather than the base being
  # streamed back untouched: the seed is injected as an uncompressed tar of the
  # rendered YAML, so devnode0's MAC (dev/git-fixture/fleet.yaml) is greppable in
  # the image bytes. The negative control on the base image is what makes that
  # meaningful — without it, a match could just mean the base already contained
  # the string. Neither assertion can be satisfied by an error body.
  check "downloaded .img carries devnode0's seeded MAC" \
    grep -aq "$DEVNODE0_MAC" "$out_img"
  check "the base image does not carry that MAC (so the above proves injection)" \
    bash -c "! grep -aq '$DEVNODE0_MAC' '$INCUSOS_BASE_IMAGE'"

  echo
  echo "== 5. Unknown instance 404s rather than 500ing =="
  status=$(curl -s -o /dev/null -w '%{http_code}' "$base_url/instances/does-not-exist/image")
  check "GET /instances/does-not-exist/image returns 404" bash -c "[ '$status' = '404' ]"
fi

echo
echo "(Not covered here: booting the produced image in a real VM and confirming"
echo " cert trust — that's #40's broader end-to-end check, deliberately not"
echo " duplicated in this route-level script.)"

summary
