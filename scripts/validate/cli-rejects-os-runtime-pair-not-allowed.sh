#! /bin/bash
# Validates GH issue #487: config.Validate checks each Instance's os and
# runtime against the allowed-pairs table, and requires every Instance in the
# fleet (its one cluster) to resolve to the same os and the same runtime.
# Proved on the real CLI: `bootstrap render-seed` runs config.Validate
# (cmd/bootstrap/cmd/render_seed.go); `parse-config` doesn't.
#
# Rejected, each grepping for its own error text:
#   1. os: incusos + runtime: kubernetes, with only the pair error.
#   2. a two-Instance fleet with mixed runtime (the fleet-wide error, not the
#      second Instance's pair error, which it also reports).
#   3. a two-Instance fleet with mixed os (the fleet-wide error, not the
#      second Instance's no-backend error, which it also reports).
# A two-Instance fleet would also fail render-seed's later exactly-one-Instance
# check, so a bare non-zero exit proves nothing: the message is the check.
#
# Accepted: os: incusos + runtime: incus, and an Instance with runtime omitted.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR" || exit 1
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="render-seed rejects an os/runtime pair the table doesn't allow, and a fleet mixing os or runtime (#487)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="go"
VALIDATE_DURATION="~2s"
# The accepted cases render an IncusOS seed, so this proves the check on the
# IncusOS backend only.
VALIDATE_SUPPORTED_OS="incusos"

validate_parse_args "$@"

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$ROOT_DIR/bin/bootstrap"
CERT_DIR="$WORK_DIR/cert"

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# render_seed writes fleet_yaml to a temp file and runs render-seed against it,
# leaving its combined output in RENDER_OUT and returning its exit status.
render_seed() {
  local fleet_yaml="$1" fleet_file
  fleet_file="$(mktemp "$WORK_DIR/fleet.XXXXXX.yaml")"
  printf '%s\n' "$fleet_yaml" >"$fleet_file"
  RENDER_OUT=$("$BOOTSTRAP_BIN" render-seed --file "$fleet_file" --cert "$CERT_DIR/client.crt" --output-dir "$WORK_DIR/seed-$$-$RANDOM" 2>&1)
}

# expect_rejects requires a non-zero exit AND an error matching pattern, and,
# when forbid is given, no output matching it. A bare non-zero exit is
# satisfied by any failure (a renamed flag, a fixture typo, the
# exactly-one-Instance check), so it isn't scored as a pass (#115).
expect_rejects() {
  local desc="$1" pattern="$2" fleet_yaml="$3" forbid="${4:-}"
  if render_seed "$fleet_yaml"; then
    record_fail "$desc" "render-seed succeeded; expected it to reject the fleet"
  elif ! grep -qE "$pattern" <<<"$RENDER_OUT"; then
    record_fail "$desc" "rejected, but not for the reason under test — want output matching: $pattern; got: $(head -1 <<<"$RENDER_OUT")"
  elif [ -n "$forbid" ] && grep -qE "$forbid" <<<"$RENDER_OUT"; then
    record_fail "$desc" "rejected for the reason under test, but also reported: $forbid; got: $(head -1 <<<"$RENDER_OUT")"
  else
    record_pass "$desc"
  fi
}

expect_accepts() {
  local desc="$1" fleet_yaml="$2"
  if render_seed "$fleet_yaml"; then
    record_pass "$desc"
  else
    record_fail "$desc" "render-seed rejected the fleet: $(head -1 <<<"$RENDER_OUT")"
  fi
}

NETWORK='kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dns: [192.168.1.1]'

# instance NAME EXTRA prints a kind: Instance document; EXTRA is extra YAML
# lines (os/runtime), or empty.
instance() {
  printf -- '---\nkind: Instance\nname: %s\nmac: aa:bb:cc:dd:ee:%s\nnetwork: home-lan\ndisk: single\nnic: single\nsecurity:\n  tpm: false\n  secure_boot: true\napplications: [incus]\n' "$1" "$3"
  if [ -n "$2" ]; then printf '%s\n' "$2"; fi
}

# Start

echo "== 0. Hard prerequisites =="
require_cmd go
check_prereqs

echo
echo "== 1. Build the bootstrap binary =="
check "build bootstrap binary" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap

if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap binary not built or not executable: $BOOTSTRAP_BIN" >&2
  exit 2
fi

echo
echo "== 2. Generate cert =="
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR"

echo
echo "== 3. render-seed must reject os: incusos + runtime: kubernetes with the pair error alone =="
expect_rejects "render-seed rejects incusos + kubernetes" \
  'instances\[0\]\.runtime: os "incusos" only runs incus, not "kubernetes"' \
  "$NETWORK
$(instance node0 $'os: incusos\nruntime: kubernetes' 01)" \
  'no backend yet'

echo
echo "== 4. render-seed must reject a fleet mixing runtime =="
expect_rejects "render-seed rejects a fleet mixing runtime" \
  "instances\\[1\\]\\.runtime: \"kubernetes\" differs from instances\\[0\\]'s \"incus\"" \
  "$NETWORK
$(instance node0 'runtime: incus' 01)
$(instance node1 'runtime: kubernetes' 02)"

echo
echo "== 5. render-seed must reject a fleet mixing os =="
expect_rejects "render-seed rejects a fleet mixing os" \
  "instances\\[1\\]\\.os: \"debian\" differs from instances\\[0\\]'s \"incusos\"" \
  "$NETWORK
$(instance node0 '' 01)
$(instance node1 'os: debian' 02)"

echo
echo "== 6. render-seed must accept os: incusos + runtime: incus =="
expect_accepts "render-seed accepts incusos + incus" \
  "$NETWORK
$(instance node0 $'os: incusos\nruntime: incus' 01)"

echo
echo "== 7. render-seed must accept an Instance with runtime omitted =="
expect_accepts "render-seed accepts runtime omitted" \
  "$NETWORK
$(instance node0 '' 01)"

summary
