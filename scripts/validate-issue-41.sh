#! /bin/bash
# Validates GH issue #41: seed.Render should reject network addressing that
# doesn't add up. Three cases, each its own fleet.yaml + render-seed run:
#   1. Instance.static_ip not contained within the declared Network.CIDR.
#   2. Network.dhcp_excluded_range not contained within Network.CIDR.
#   3. Instance.static_ip inside the CIDR but outside dhcp_excluded_range
#      (per docs/Architecture.md, static IPs are meant to be drawn from the
#      excluded range so DHCP never hands one out from under a node).
#
# The script builds the bootstrap CLI binary, generates a client cert (via
# bootstrap gen-cert), then for each case writes a fleet.yaml and runs
# `bootstrap render-seed --file <fleet>` expecting it to return non-zero.
# If render-seed succeeds for any case, the validation fails.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$ROOT_DIR/bin/bootstrap"
CERT_DIR="$WORK_DIR/cert"

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

# expect_render_seed_rejects writes fleet_yaml to a temp file, runs render-seed
# against it, and requires BOTH a non-zero exit AND an error matching pattern.
#
# Matching the message is the whole point. The previous version asserted only a
# non-zero exit, which *any* failure satisfies — a renamed flag, a typo in the
# fixture — so all three checks below could report PASS while proving nothing
# about the rule under test. Demonstrated rather than assumed: renaming --file
# to --fleetfile makes render-seed exit non-zero with "unknown flag", which the
# old helper scored as three PASSes and this one reports as three FAILs naming
# the mismatch. Unlike validate-issue-39.sh's equivalent false pass, this one
# had no neighbouring assertion to catch it, so the script would have gone
# fully green. See #115.
expect_render_seed_rejects() {
  local desc="$1" pattern="$2" fleet_yaml="$3"
  local fleet_file out
  fleet_file="$(mktemp "$WORK_DIR/fleet.XXXXXX.yaml")"
  printf '%s\n' "$fleet_yaml" >"$fleet_file"

  if out=$("$BOOTSTRAP_BIN" render-seed --file "$fleet_file" --cert "$CERT_DIR/client.crt" --output-dir "$WORK_DIR/seed-$$-$RANDOM" 2>&1); then
    echo "FAIL: $desc (render-seed succeeded; expected it to reject the fleet)" >&2
    fail=$((fail + 1))
  elif ! grep -qE "$pattern" <<<"$out"; then
    echo "FAIL: $desc (rejected, but not for the reason under test)" >&2
    echo "      want stderr matching: $pattern" >&2
    echo "      got: $(head -1 <<<"$out")" >&2
    fail=$((fail + 1))
  else
    echo "PASS: $desc"
    pass=$((pass + 1))
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
echo "== 2. Generate cert =="
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR"

echo
echo "== 3. render-seed must reject static_ip outside the network's cidr =="
expect_render_seed_rejects "render-seed rejects static_ip outside cidr" \
  'instances\[0\]\.static_ip: .* is not inside cidr' "$(cat <<'EOF'
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
)"

echo
echo "== 4. render-seed must reject dhcp_excluded_range outside the network's cidr =="
expect_render_seed_rejects "render-seed rejects dhcp_excluded_range outside cidr" \
  'networks\[0\]\.dhcp_excluded_range: .*not contained in cidr' "$(cat <<'EOF'
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dhcp_excluded_range: 10.0.0.200-10.0.0.250
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: aa:bb:cc:dd:ee:ff
network: home-lan
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
EOF
)"

echo
echo "== 5. render-seed must reject static_ip inside cidr but outside dhcp_excluded_range =="
expect_render_seed_rejects "render-seed rejects static_ip outside dhcp_excluded_range" \
  'instances\[0\]\.static_ip: .* is outside dhcp_excluded_range' "$(cat <<'EOF'
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dhcp_excluded_range: 192.168.1.200-192.168.1.250
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: aa:bb:cc:dd:ee:ff
network: home-lan
static_ip: 192.168.1.50
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
EOF
)"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
