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
# "homelab-host" remote / "default" project / "home-lan" network set up
# by .devcontainer/scripts/2-setup-dev-network.sh (see devcontainer-reaches-host-incus.sh).
#
# Requires a real, bootable base IncusOS raw image — unlike
# cli-renders-seed-and-builds-image.sh's placeholder, a VM actually has to boot and install
# from this one. Point INCUSOS_BASE_IMAGE at a local copy; the relevant
# section is skipped with a clear message if it's unset.
#
# Getting the .img's bytes onto homelab-host: a plain `disk source=<path>`
# device is resolved on the SERVER's filesystem, not the client's — since
# REMOTE is a remote like homelab-host, the .img built locally by
# build-image is never visible there directly. The only way found to move
# arbitrary local bytes into a remote Incus's storage is to stream them
# through the API: create a block-type custom volume, attach it to a
# disposable "writer" VM, and `dd` the file in via `incus exec`. (Custom
# volume `import --type=iso` only works for genuine ISO9660 images with a
# boot catalog, not raw USB-style .img files.)
#
# node0's static IP lives on the home-lan Incus bridge, which is only
# routable from the Incus host and other instances on that bridge — not
# from the devcontainer itself (it reaches Incus over the
# host.docker.internal:8443 API, not the bridge's network). So the actual
# cert-authenticated request runs from a small probe container launched on
# home-lan for that purpose, via `incus exec`.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-incus.sh
. "$ROOT_DIR/scripts/validate/lib-incus.sh"

VALIDATE_PROVES="a node boots from a seeded .img, installs IncusOS, and trusts the bootstrap cert (#5)"
VALIDATE_GROUP="incus-vm"
VALIDATE_NEEDS="incus go pinned-base-images [flasher-tool] [INCUSOS_BASE_IMAGE]"
VALIDATE_DURATION="~9m"

validate_parse_args "$@"
WORK_DIR="$(mktemp -d)"
# Overridable so this can run somewhere other than the devcontainer — notably
# on the Incus host itself, where Incus is a local unix socket and no remote
# named "homelab-host" exists (see #115's CI design).
#
# PROJECT defaults to "default". This used to target a "homelab-dev" project,
# which got stuck with features.networks=true and could therefore see no
# networks at all, failing every run's prerequisites (#96). #132 repointed the
# suite at "default", where home-lan lives; #131 deleted homelab-dev outright.
REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
PROJECT="${VALIDATE_INCUS_PROJECT:-default}"
NETWORK="${VALIDATE_INCUS_NETWORK:-home-lan}"
POOL="default"
STATIC_IP="192.168.1.201"
MAC="aa:bb:cc:dd:ee:ff"
VM_NAME="validate-nodeboot-$$"
WRITER_NAME="validate-nodeboot-writer-$$"
PROBE_NAME="validate-nodeboot-probe-$$"
SEED_VOL="validate-nodeboot-seeded-img-$$"

BOOTSTRAP_BIN="$WORK_DIR/bootstrap"

pass=0
fail=0

cleanup() {
  incus delete --force --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$WRITER_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$PROBE_NAME" >/dev/null 2>&1
  incus storage volume delete "$REMOTE:$POOL" "$SEED_VOL" --project "$PROJECT" >/dev/null 2>&1
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

check() {
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    record_pass "$desc"
  else
    record_fail "$desc"
  fi
}

console_log() {
  incus console --project "$PROJECT" "$REMOTE:$VM_NAME" --show-log 2>&1 | tr -d '\0'
}

# wait_for_console_text polls the VM's console log for needle, up to
# timeout_s seconds.
wait_for_console_text() {
  local needle="$1" timeout_s="$2" waited=0
  while [ "$waited" -lt "$timeout_s" ]; do
    if console_log | grep -qF "$needle"; then
      return 0
    fi
    sleep 5
    waited=$((waited + 5))
  done
  return 1
}

echo "== 0. Hard prerequisites =="
# These are preconditions for the assertion, not the assertion itself — whether
# the devcontainer has a working Incus remote is devcontainer-reaches-host-incus.sh's
# job, so nothing is lost by gating rather than counting them here.
#
# require_flasher_tool exists because of #136: build-image shells out to it, and
# without this the CLI's own perfectly clear error was swallowed by `check`,
# producing five cascading failures that read like a provisioning regression.
require_cmd incus go
require_flasher_tool
require_incus_remote "$REMOTE"
require_incus_project "$REMOTE" "$PROJECT"
require_incus_network "$REMOTE" "$PROJECT" "$NETWORK"
require_incus_image "$REMOTE" "$PROJECT" "$VALIDATE_ALPINE_CT"
require_incus_image "$REMOTE" "$PROJECT" "$VALIDATE_ALPINE_VM"
check_prereqs

echo
echo "== 1. Build the bootstrap CLI =="
check "bootstrap CLI builds" go -C "$ROOT_DIR" build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap

if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap CLI didn't build; nothing downstream can be meaningful" >&2
  summary
fi

echo
echo "== 2. Build the pipeline: gen-cert -> render-seed -> build-image =="
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$WORK_DIR/cert"

cat >"$WORK_DIR/fleet.yaml" <<EOF
kind: Network
name: $NETWORK
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: $MAC
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
if ! have_env_file INCUSOS_BASE_IMAGE; then
  # The operator supplies this 3.2 GB image; its absence says nothing about
  # whether node provisioning works, so it's a skip rather than four failures.
  _why="INCUSOS_BASE_IMAGE ${INCUSOS_BASE_IMAGE:+=$INCUSOS_BASE_IMAGE }not usable"
  for _desc in \
    "build-image exits 0" \
    "seed .img streamed onto $REMOTE as a block volume" \
    "VM installs IncusOS from the seeded image" \
    "node trusts the bootstrap cert and is reachable over Incus API"; do
    skip_check "$_desc" base-image "$_why"
  done
else
  check "build-image exits 0" "$BOOTSTRAP_BIN" build-image \
    --seed-dir "$WORK_DIR/seed" \
    --image "$INCUSOS_BASE_IMAGE" \
    --output "$WORK_DIR/node0.img"

  img_bytes=$(stat -c%s "$WORK_DIR/node0.img")
  vol_gib=$(((img_bytes + (1 << 30) - 1) / (1 << 30)))

  # Stream node0.img onto the remote as a block-type custom volume, via a
  # disposable "writer" VM — see the file header comment for why a plain
  # `disk source=<local path>` device can't be used against a remote
  # target.
  check "seed .img streamed onto $REMOTE as a block volume" bash -c "
    set -eu
    incus storage volume create '$REMOTE:$POOL' '$SEED_VOL' --type=block size=${vol_gib}GiB --project '$PROJECT' &&
    incus launch '$VALIDATE_ALPINE_VM' '$REMOTE:$WRITER_NAME' --vm --project '$PROJECT' \
      --network '$NETWORK' --storage '$POOL' \
      -c security.secureboot=false -c limits.cpu=1 -c limits.memory=512MiB &&
    incus config device add '$REMOTE:$WRITER_NAME' raw-disk disk pool='$POOL' source='$SEED_VOL' --project '$PROJECT' &&
    for _ in \$(seq 1 30); do
      incus exec --project '$PROJECT' '$REMOTE:$WRITER_NAME' -- true >/dev/null 2>&1 && break
      sleep 2
    done &&
    writer_dev=\$(incus exec --project '$PROJECT' '$REMOTE:$WRITER_NAME' -- \
      sh -c \"awk '\\\$4 ~ /^sd[a-z]+\\\$/ && \\\$4 != \\\"sda\\\" {print \\\$4}' /proc/partitions\" | head -1) &&
    [ -n \"\$writer_dev\" ] &&
    cat '$WORK_DIR/node0.img' | incus exec --project '$PROJECT' '$REMOTE:$WRITER_NAME' -- dd of=/dev/\$writer_dev bs=4M &&
    incus delete --force --project '$PROJECT' '$REMOTE:$WRITER_NAME'
  "

  check "VM created with empty disk + seeded image as install media" bash -c "
    incus init --empty --vm --project '$PROJECT' '$REMOTE:$VM_NAME' \
      -c security.secureboot=false -c limits.cpu=2 -c limits.memory=2GiB \
      --storage '$POOL' -d root,size=50GiB &&
    incus config device add --project '$PROJECT' '$REMOTE:$VM_NAME' install-media disk pool='$POOL' source='$SEED_VOL' boot.priority=10 &&
    incus config device add --project '$PROJECT' '$REMOTE:$VM_NAME' eth0 nic network='$NETWORK' hwaddr='$MAC' &&
    incus start --project '$PROJECT' '$REMOTE:$VM_NAME'
  "

  echo "Waiting up to 5 minutes for the install to complete ..."
  if wait_for_console_text "successfully installed" 300; then
    record_pass "VM installs IncusOS from the seeded image"

    # IncusOS halts after installing and waits for the install media to be
    # removed before it'll boot the installed system — mirrors the manual
    # step real hardware needs (eject the USB stick), and is enforced by
    # IncusOS itself: it refuses to start at all if it can still see
    # install media alongside an already-installed disk.
    incus stop --project "$PROJECT" "$REMOTE:$VM_NAME" --force >/dev/null 2>&1
    incus config device remove --project "$PROJECT" "$REMOTE:$VM_NAME" install-media >/dev/null 2>&1
    incus start --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
  else
    record_fail "VM installs IncusOS from the seeded image"
  fi

  check "probe instance ready on $NETWORK" bash -c "
    set -eu
    incus launch '$VALIDATE_ALPINE_CT' '$REMOTE:$PROBE_NAME' --project '$PROJECT' --network '$NETWORK' --storage '$POOL' &&
    for _ in \$(seq 1 15); do
      incus exec --project '$PROJECT' '$REMOTE:$PROBE_NAME' -- apk add --no-cache curl >/dev/null 2>&1 && break
      sleep 2
    done &&
    incus exec --project '$PROJECT' '$REMOTE:$PROBE_NAME' -- sh -c 'command -v curl' &&
    incus file push '$WORK_DIR/cert/client.crt' '$REMOTE:$PROBE_NAME/root/client.crt' --project '$PROJECT' &&
    incus file push '$WORK_DIR/cert/client.key' '$REMOTE:$PROBE_NAME/root/client.key' --project '$PROJECT'
  "

  echo "Waiting up to 5 minutes for node0 to boot the installed system and bring up Incus on https://$STATIC_IP:8443 (checked from the probe instance) ..."
  reachable=false
  for _ in $(seq 1 60); do
    if incus exec --project "$PROJECT" "$REMOTE:$PROBE_NAME" -- \
      curl --cert /root/client.crt --key /root/client.key -k -sf "https://$STATIC_IP:8443/1.0" -o /dev/null >/dev/null 2>&1; then
      reachable=true
      break
    fi
    sleep 5
  done

  # A single authenticated request proves both halves of the "done when"
  # criterion: install completed (Incus is up) and the cert is trusted
  # (the request succeeds instead of being rejected as untrusted).
  if "$reachable"; then
    response=$(incus exec --project "$PROJECT" "$REMOTE:$PROBE_NAME" -- \
      curl --cert /root/client.crt --key /root/client.key -k -s "https://$STATIC_IP:8443/1.0" 2>/dev/null)
    if echo "$response" | grep -q '"auth":"trusted"'; then
      record_pass "node trusts the bootstrap cert and is reachable over Incus API"
    else
      record_fail "node trusts the bootstrap cert and is reachable over Incus API (reachable, but auth was not \"trusted\": $response)"
    fi
  else
    record_fail "node trusts the bootstrap cert and is reachable over Incus API (node never became reachable)"
  fi
fi

echo
summary
