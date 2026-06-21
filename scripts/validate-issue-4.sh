#! /bin/bash
# Validates GH issue #4 ("Shell out to flasher-tool to produce a .img")
# "Done when" criteria: running the bootstrap CLI against a seed produces a
# .img file ready to dd onto a USB stick.
#
# Intended to run INSIDE the devcontainer. Requires flasher-tool on $PATH
# (go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool). Uses a
# small placeholder file in place of a real IncusOS base image — flasher-tool's
# --seed path only WriteAt's the seed tar at a fixed byte offset, so it
# doesn't need a real bootable image to validate that the injection itself
# works end-to-end. The resulting file is a sparse stand-in, not a real
# bootable .img.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

BOOTSTRAP_BIN="$WORK_DIR/bootstrap"

pass=0
fail=0

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

echo "== 1. Prerequisites =="
check "flasher-tool installed (go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool)" command -v flasher-tool
check "bootstrap CLI builds" go -C "$ROOT_DIR" build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap

if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo
  echo "$((pass)) passed, $((fail + 1)) failed (bootstrap CLI didn't build, skipping remaining checks)"
  exit 1
fi

echo
echo "== 2. render-seed produces a seed bundle =="
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$WORK_DIR/cert"

cat >"$WORK_DIR/fleet.yaml" <<'EOF'
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: aa:bb:cc:dd:ee:ff
network: home-lan
static_ip: 192.168.1.201
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
EOF

check "render-seed exits 0" "$BOOTSTRAP_BIN" render-seed \
  --file "$WORK_DIR/fleet.yaml" \
  --cert "$WORK_DIR/cert/client.crt" \
  --output-dir "$WORK_DIR/seed"
check "install.yaml rendered" test -f "$WORK_DIR/seed/install.yaml"
check "network.yaml rendered" test -f "$WORK_DIR/seed/network.yaml"
check "applications.yaml rendered" test -f "$WORK_DIR/seed/applications.yaml"
check "incus.yaml rendered" test -f "$WORK_DIR/seed/incus.yaml"

echo
echo "== 3. build-image injects the seed into a .img =="
if ! command -v flasher-tool >/dev/null 2>&1; then
  echo "FAIL: build-image exits 0 (skipped: flasher-tool not installed)"
  echo "FAIL: output .img exists and grew to cover the injected seed (skipped: flasher-tool not installed)"
  fail=$((fail + 2))
else
  base_image="$WORK_DIR/base.img"
  output_image="$WORK_DIR/out.img"
  : >"$base_image" # empty placeholder standing in for a real base IncusOS image

  check "build-image exits 0" "$BOOTSTRAP_BIN" build-image \
    --seed-dir "$WORK_DIR/seed" \
    --image "$base_image" \
    --output "$output_image"

  # flasher-tool's --seed path writes the seed tar at a fixed offset past
  # the end of an empty placeholder file, so a successful run must grow the
  # output file beyond that offset.
  seed_offset=2148532224
  output_size=$(stat -c '%s' "$output_image" 2>/dev/null || echo 0)
  check "output .img exists and grew to cover the injected seed" test "$output_size" -gt "$seed_offset"
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
