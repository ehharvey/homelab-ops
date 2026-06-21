#! /bin/bash
# Validates GH issue #5 ("Manually flash + install; confirm Incus is
# reachable and trusts the generated cert") "Done when" criteria, expanded
# per the issue's comment thread to automated Incus-VM-based testing:
#   1. gen-cert -> render-seed -> build-image produces a .img with Incus
#      preseeded to trust the bootstrap client cert (incus.yaml).
#   2. An Incus VM with an empty target disk, booted with that .img attached
#      as install media, installs IncusOS and brings up Incus.
#   3. The bootstrap cert authenticates against the installed node's Incus
#      API — this single check proves both "install succeeded" and "the
#      cert is trusted" at once.
#
# Intended to run INSIDE the devcontainer, against the existing
# "homelab-host" remote / "homelab-dev" project / "home-lan" network set up
# by .devcontainer/scripts/2-setup-dev-network.sh (see validate-issue-6.sh).
#
# Requires a real, bootable base IncusOS raw image — unlike
# validate-issue-4.sh's placeholder, a VM actually has to boot and install
# from this one. Point INCUSOS_BASE_IMAGE at a local copy; the relevant
# section is skipped with a clear message if it's unset.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="$(mktemp -d)"
REMOTE="homelab-host"
PROJECT="homelab-dev"
NETWORK="home-lan"
STATIC_IP="192.168.1.201"
VM_NAME="validate-issue-5-$$"

BOOTSTRAP_BIN="$WORK_DIR/bootstrap"

pass=0
fail=0

cleanup() {
  incus delete --force --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
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

echo "== 1. Prerequisites =="
check "incus client installed" command -v incus
check "bootstrap CLI builds" go -C "$ROOT_DIR" build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
check "incus remote '$REMOTE' reachable" incus info "$REMOTE:"
check "project '$PROJECT' exists" bash -c "incus project list '$REMOTE:' -f csv | cut -d, -f1 | sed 's/ (current)\$//' | grep -qx '$PROJECT'"
check "network '$NETWORK' exists" bash -c "incus network list '$REMOTE:' --project '$PROJECT' -f csv | cut -d, -f1 | sed 's/ (current)\$//' | grep -qx '$NETWORK'"

if [ ! -x "$BOOTSTRAP_BIN" ] || ! incus info "$REMOTE:" >/dev/null 2>&1; then
  echo
  echo "$pass passed, $((fail + 4)) failed (prerequisites not met, skipping remaining checks)"
  exit 1
fi

echo
echo "== 2. Build the pipeline: gen-cert -> render-seed -> build-image =="
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$WORK_DIR/cert"

cat >"$WORK_DIR/fleet.yaml" <<EOF
kind: Network
name: $NETWORK
cidr: 192.168.1.0/24
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: aa:bb:cc:dd:ee:ff
network: $NETWORK
static_ip: $STATIC_IP
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
check "incus.yaml rendered" test -f "$WORK_DIR/seed/incus.yaml"

echo
echo "== 3. Boot an Incus VM from the produced .img and confirm install + cert trust =="
if [ -z "${INCUSOS_BASE_IMAGE:-}" ]; then
  echo "FAIL: build-image exits 0 (skipped: INCUSOS_BASE_IMAGE not set)"
  echo "FAIL: VM created with empty disk + .img install media (skipped: INCUSOS_BASE_IMAGE not set)"
  echo "FAIL: node trusts the bootstrap cert and is reachable over Incus API (skipped: INCUSOS_BASE_IMAGE not set)"
  fail=$((fail + 3))
elif [ ! -f "$INCUSOS_BASE_IMAGE" ]; then
  echo "FAIL: build-image exits 0 (skipped: INCUSOS_BASE_IMAGE=$INCUSOS_BASE_IMAGE not found)"
  echo "FAIL: VM created with empty disk + .img install media (skipped: INCUSOS_BASE_IMAGE not found)"
  echo "FAIL: node trusts the bootstrap cert and is reachable over Incus API (skipped: INCUSOS_BASE_IMAGE not found)"
  fail=$((fail + 3))
else
  check "build-image exits 0" "$BOOTSTRAP_BIN" build-image \
    --seed-dir "$WORK_DIR/seed" \
    --image "$INCUSOS_BASE_IMAGE" \
    --output "$WORK_DIR/node0.img"

  check "VM created with empty disk + .img install media" bash -c "
    incus launch --empty --vm --project '$PROJECT' '$REMOTE:$VM_NAME' -c limits.cpu=2 -c limits.memory=2GiB &&
    incus config device add --project '$PROJECT' '$REMOTE:$VM_NAME' install-media disk source='$WORK_DIR/node0.img' boot.priority=10 &&
    incus config device add --project '$PROJECT' '$REMOTE:$VM_NAME' eth0 nic network='$NETWORK' &&
    incus start --project '$PROJECT' '$REMOTE:$VM_NAME'
  "

  echo "Waiting up to 5 minutes for node0 to install and bring up Incus on https://$STATIC_IP:8443 ..."
  reachable=false
  for _ in $(seq 1 60); do
    if curl --cert "$WORK_DIR/cert/client.crt" --key "$WORK_DIR/cert/client.key" -k -sf \
      "https://$STATIC_IP:8443/1.0" -o /dev/null; then
      reachable=true
      break
    fi
    sleep 5
  done

  # A single authenticated request proves both halves of the "done when"
  # criterion: install completed (Incus is up) and the cert is trusted
  # (the request succeeds instead of being rejected as untrusted).
  if "$reachable"; then
    echo "PASS: node trusts the bootstrap cert and is reachable over Incus API"
    pass=$((pass + 1))
  else
    echo "FAIL: node trusts the bootstrap cert and is reachable over Incus API"
    fail=$((fail + 1))
  fi
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
