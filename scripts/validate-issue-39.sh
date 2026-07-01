#! /bin/bash
# Validates GH issue #39: GET /instances/{name}/image regenerates and streams a
# bootable .img reflecting the instance's current seed/cert/IP, generated fresh
# on each request (no caching — docs/Decisions.md §3).
#
# Drives the real docker compose stack (web + dev/git-fixture git remote) and
# hits the route with curl, following validate-issue-36.sh's pattern: the web
# app's deployment config (the break-glass cert AND the base IncusOS image) is
# supplied via a scoped compose override written here, NOT a permanent
# docker-compose.yml change. The web image built by `compose up --build` is the
# one that carries flasher-tool (see Dockerfile), so this also exercises
# flasher-tool running inside the distroless container end-to-end.
#
# The image checks need a real base IncusOS raw image: point INCUSOS_BASE_IMAGE
# at a local copy. Those checks skip with a clear message if it's unset/missing
# (same convention as validate-issue-5.sh). A real IncusOS image injects the
# seed in place at a fixed offset, so the output is the SAME size as the base
# but differs byte-for-byte — this asserts the latter (seed injected) rather
# than size growth, and stops at "structurally a seeded disk image": a full
# boot-and-cert-trust check belongs to #40, not here.
#
# The route may not exist yet — until #39 lands, expect the image checks to fail
# with a 404/503 against GET /instances/.../image (same convention as
# validate-issue-36.sh before #36 landed).
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires docker
# compose, curl, jq, go, and openssl.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

pass=0
fail=0

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$ROOT_DIR/bin/bootstrap"
CERT_DIR="$WORK_DIR/cert"
export CERT_DIR
OVERRIDE="$(mktemp /tmp/compose-image-override.XXXXXX.yml)"

# Base override just mounts the cert (as #36 does). The base-image mount +
# BASE_IMAGE_PATH are appended below only when INCUSOS_BASE_IMAGE is usable, so
# the stack still comes up (and the route reports 503 "not configured") when
# it isn't.
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    volumes:
      - ${CERT_DIR}:/cert:ro
    environment:
      - CLIENT_CERT_PATH=/cert/client.crt
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
EOF
fi

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
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "validate-issue-39"

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
  echo "FAIL: image route returns 200 (skipped: INCUSOS_BASE_IMAGE not usable)"
  echo "FAIL: response is an attachment (skipped: INCUSOS_BASE_IMAGE not usable)"
  echo "FAIL: downloaded .img is a non-empty disk image (skipped: INCUSOS_BASE_IMAGE not usable)"
  echo "FAIL: downloaded .img differs from the base (seed injected) (skipped: INCUSOS_BASE_IMAGE not usable)"
  echo "FAIL: unknown instance 404s (skipped: INCUSOS_BASE_IMAGE not usable)"
  fail=$((fail + 5))
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

  # In-place seed injection keeps the size equal to the base but changes the
  # bytes; cmp differing proves flasher-tool actually wrote the seed rather than
  # streaming the base back untouched.
  if cmp -s "$INCUSOS_BASE_IMAGE" "$out_img"; then
    echo "FAIL: downloaded .img differs from the base (seed injected) — image is byte-identical to base ($out_bytes vs $base_bytes bytes)"
    fail=$((fail + 1))
  else
    echo "PASS: downloaded .img differs from the base (seed injected)"
    pass=$((pass + 1))
  fi

  echo
  echo "== 5. Unknown instance 404s rather than 500ing =="
  status=$(curl -s -o /dev/null -w '%{http_code}' "$base_url/instances/does-not-exist/image")
  check "GET /instances/does-not-exist/image returns 404" bash -c "[ '$status' = '404' ]"
fi

echo
echo "(Not covered here: booting the produced image in a real VM and confirming"
echo " cert trust — that's #40's broader end-to-end check, deliberately not"
echo " duplicated in this route-level script.)"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
