#! /bin/bash
# Validates GH issue #41: seed.Render should reject an Instance.static_ip that's
# not contained within the declared Network.CIDR. The current codebase does not
# perform this validation, so this script is expected to fail until the fix is
# implemented.
#
# The script builds the bootstrap CLI binary, generates a client cert (via
# bootstrap gen-cert), writes a small fleet.yaml where the instance's
# static_ip is outside the network's CIDR, and then runs
# `bootstrap render-seed --file <fleet>` expecting it to return non-zero.
# If render-seed succeeds (current behavior), the validation fails.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$ROOT_DIR/bin/bootstrap"

pass=0
fail=0

cleanup() {
  rm -rf "$WORK_DIR"
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

# Start

echo "== 1. Prerequisites: go tool and build bootstrap binary =="
check "go present" command -v go
check "build bootstrap binary" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap

if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap binary not built or not executable: $BOOTSTRAP_BIN" >&2
  exit 2
fi

echo
echo "== 2. Generate cert and create an out-of-range fleet.yaml =="
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$WORK_DIR/cert"

cat >"$WORK_DIR/fleet.yaml" <<EOF
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
static_ip: 10.0.0.5
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
EOF

# Run render-seed and EXPECT it to fail because static_ip is outside the CIDR.
# Since the current code does NOT validate this, the command will probably
# succeed and cause this validation script to fail — which is the desired
# outcome until the fix is applied.

echo
echo "== 3. Run render-seed and expect failure (static_ip outside CIDR) =="
if ! "$BOOTSTRAP_BIN" render-seed --file "$WORK_DIR/fleet.yaml" --cert "$WORK_DIR/cert/client.crt" --output-dir "$WORK_DIR/seed" >/dev/null 2>&1; then
  echo "PASS: render-seed returned non-zero as expected (validation present)"
  exit 0
else
  echo "FAIL: render-seed succeeded but should have rejected the out-of-range static_ip" >&2
  echo "(This means the repository currently lacks the IP-in-CIDR validation.)" >&2
  exit 1
fi
