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

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="the bootstrap CLI renders a seed and injects it into a .img (#4)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="go [flasher-tool]"
VALIDATE_DURATION="~3s"

validate_parse_args "$@"

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

BOOTSTRAP_BIN="$WORK_DIR/bootstrap"

echo "== 0. Hard prerequisites =="
require_cmd go
check_prereqs

echo
echo "== 1. Build the bootstrap CLI =="
check "bootstrap CLI builds" go -C "$ROOT_DIR" build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap

if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap CLI didn't build; nothing downstream can be meaningful" >&2
  summary
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
# flasher-tool is an operator-installed binary the repo doesn't vendor, so its
# absence is a genuine skip, not a defect in anything under test. It used to be
# counted as two failures — see #136 for what that cost when the same gap in
# node-boots-and-trusts-bootstrap-cert.sh presented as five failures reading
# like a provisioning regression.
if ! have_cmd flasher-tool; then
  skip_check "build-image exits 0" flasher-tool "flasher-tool not installed"
  skip_check "output .img exists and grew to cover the injected seed" flasher-tool "flasher-tool not installed"
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
summary
