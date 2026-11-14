#! /bin/bash
# Validates GH issue #242 (docs/Decisions.md §29, "Where install media is
# built", option 3): a node installs from the STOCK, unmodified IncusOS base
# image plus separate SEED_DATA seed media that the operator CLI wrote, while
# the web app has no BASE_IMAGE_PATH, and the installed node's WireGuard tunnel
# to the web app handshakes.
#
#   1. The real web app runs in an Alpine container on home-lan, with no
#      BASE_IMAGE_PATH (its image route answers 503, asserted), one operator
#      peer, and a fleet of one instance, node0.
#   2. `bootstrap api seed-media node0` runs as that operator peer, joining
#      the web app's tunnel, and writes the ISO 9660 SEED_DATA volume. It runs
#      inside the web app's container so the tunnel needs no route from the
#      devcontainer to home-lan; the ISO is then pulled out and imported on
#      the Incus host as an ISO volume.
#   3. node0 boots from the stock base image (`make incusos-base`) with the
#      ISO attached as its CD-ROM. Once IncusOS reports the install, both are
#      detached and node0 boots from its disk: left attached, the seed media
#      would stop that boot ("install media detected, but the system is
#      already installed").
#   4. node0's own network state shows a completed handshake with the web
#      app's WireGuard key, read over the LAN with the break-glass client cert
#      the seed told it to trust (as node-tunnel-survives-nat-and-provisions.sh
#      reads it).
#
# A handshake means node0 got the web app's key, endpoint and its own private
# key from the ISO: the stock image carries no seed for it.
#
# SKIPs (exit 3) without the homelab-host remote [incus-remote] or the cached
# pinned base image [base-image]. The other Incus setup (the home-lan network,
# the pinned Alpine images) is a hard prerequisite, as in the other incus-vm
# scripts.
#
# Don't run beside another incus-vm script: they share the home-lan bridge.
# node0 takes .203 on it (node-boots-and-trusts-bootstrap-cert.sh uses .201,
# node-tunnel-survives-nat-and-provisions.sh .202).
#
# Intended to run INSIDE the devcontainer, from the repo root.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-incus.sh
. "$ROOT_DIR/scripts/validate/lib-incus.sh"

VALIDATE_PROVES="a node installs from the stock IncusOS image plus CLI-written SEED_DATA media, with the web app's BASE_IMAGE_PATH unset, and its tunnel handshakes (#242)"
VALIDATE_GROUP="incus-vm"
VALIDATE_NEEDS="incus go git jq pinned-base-images [incus-remote] [INCUSOS_BASE_IMAGE]"
VALIDATE_DURATION="~12m"
VALIDATE_SUPPORTED_OS="incusos"

validate_parse_args "$@"
default_incusos_base_image

REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
PROJECT="${VALIDATE_INCUS_PROJECT:-default}"
NETWORK="${VALIDATE_INCUS_NETWORK:-home-lan}"
POOL="${VALIDATE_INCUS_POOL:-default}"
MAC="aa:bb:cc:dd:ee:42"
OPERATOR_ADDR=10.100.0.240
WG_PORT=51820

VM_NAME="validate-seedmedia-node0-$$"
WRITER_NAME="validate-seedmedia-writer-$$"
PROBE_NAME="validate-seedmedia-probe-$$"
WEBAPP_NAME="validate-seedmedia-webapp-$$"
BASE_VOL="validate-seedmedia-stock-img-$$"
ISO_VOL="validate-seedmedia-seed-iso-$$"

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
WEB_BIN="$WORK_DIR/web"
HARNESS_BIN="$WORK_DIR/validate-tunnel-harness"

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  cleanup_bg_pids
  incus delete --force --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$WRITER_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$PROBE_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$WEBAPP_NAME" >/dev/null 2>&1
  incus storage volume delete "$REMOTE:$POOL" "$BASE_VOL" --project "$PROJECT" >/dev/null 2>&1
  incus storage volume delete "$REMOTE:$POOL" "$ISO_VOL" --project "$PROJECT" >/dev/null 2>&1
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# webapp_exec <cmd...> — run a command in the web app's container.
webapp_exec() {
  incus exec --project "$PROJECT" "$REMOTE:$WEBAPP_NAME" -- "$@"
}

# wait_lease <instance> — until its eth0 has an IPv4 address (up to 20s).
# shellcheck disable=SC2329 # run through check
wait_lease() {
  local _
  for _ in $(seq 1 20); do
    incus exec --project "$PROJECT" "$REMOTE:$1" -- sh -c 'ip -4 -o addr show eth0 | grep -q inet' >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

echo "== 0. Prerequisites =="
require_cmd incus go git jq
check_prereqs
if ! incus info "$REMOTE:" >/dev/null 2>&1; then
  skip_check "a node installs from stock IncusOS plus CLI-written SEED_DATA media" incus-remote \
    "incus remote '$REMOTE' not reachable (set up by .devcontainer/scripts/2-setup-dev-network.sh, or set VALIDATE_INCUS_REMOTE)"
  summary
fi
if ! have_env_file INCUSOS_BASE_IMAGE; then
  skip_check "a node installs from stock IncusOS plus CLI-written SEED_DATA media" base-image "$(base_image_skip_reason)"
  summary
fi
require_incus_project "$REMOTE" "$PROJECT"
require_incus_network "$REMOTE" "$PROJECT" "$NETWORK"
require_incus_image "$REMOTE" "$PROJECT" "$VALIDATE_ALPINE_CT"
require_incus_image "$REMOTE" "$PROJECT" "$VALIDATE_ALPINE_VM"
check_prereqs

# The LAN's addressing, read off the bridge (as node-tunnel-survives-nat-and-
# provisions.sh does). Assumes a /24.
LAN_ADDR="$(incus network get "$REMOTE:$NETWORK" ipv4.address --project "$PROJECT" 2>/dev/null)"
if [ -z "$LAN_ADDR" ] || [ "$LAN_ADDR" = "none" ]; then
  echo "ERROR: network '$NETWORK' on $REMOTE has no ipv4.address; this script needs a managed IPv4 bridge" >&2
  exit 2
fi
LAN_GATEWAY="${LAN_ADDR%/*}"
LAN_CIDR="${LAN_GATEWAY%.*}.0/24"
STATIC_IP="${LAN_GATEWAY%.*}.203"
record_pass "all prerequisites met ($NETWORK is $LAN_CIDR; node0 takes $STATIC_IP)"

echo
echo "== 1. Build the binaries, the operator key and the break-glass cert =="
# CGO_ENABLED=0: web and bootstrap run inside the musl-based Alpine container.
check "web app builds (CGO_ENABLED=0)" bash -c "cd '$ROOT_DIR' && CGO_ENABLED=0 go build -o '$WEB_BIN' ./cmd/web"
check "bootstrap CLI builds (CGO_ENABLED=0)" bash -c "cd '$ROOT_DIR' && CGO_ENABLED=0 go build -o '$BOOTSTRAP_BIN' ./cmd/bootstrap"
check "validate-tunnel-harness builds (keygen only)" go -C "$ROOT_DIR" build -o "$HARNESS_BIN" ./cmd/validate-tunnel-harness
if [ ! -x "$BOOTSTRAP_BIN" ] || [ ! -x "$WEB_BIN" ] || [ ! -x "$HARNESS_BIN" ]; then
  echo "ERROR: a required binary didn't build; nothing downstream can be meaningful" >&2
  summary
fi
OPERATOR_PUB=$("$HARNESS_BIN" -mode genkey -private-key-file "$WORK_DIR/operator.key")
check "operator keypair generated" test -n "$OPERATOR_PUB"
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$WORK_DIR/cert" --common-name "node-boots-from-separate-seed-media"

cat >"$WORK_DIR/fleet.yaml" <<EOF
kind: Network
name: $NETWORK
cidr: $LAN_CIDR
gateway: $LAN_GATEWAY
dns: [$LAN_GATEWAY]
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
mkdir -p "$WORK_DIR/fleet-src"
cp "$WORK_DIR/fleet.yaml" "$WORK_DIR/fleet-src/fleet.yaml"
check "fleet git repo built" bash -c "
  cd '$WORK_DIR/fleet-src' &&
  git init -q -b main . &&
  git -c user.email=dev@homelab-ops.local -c user.name=seed-media add fleet.yaml &&
  git -c user.email=dev@homelab-ops.local -c user.name=seed-media commit -q -m 'seed-media fleet' &&
  git clone -q --bare . '$WORK_DIR/fleet.git'
"

echo
echo "== 2. The real web app on $NETWORK, with no BASE_IMAGE_PATH =="
check "web app container launched on $NETWORK" incus launch "$VALIDATE_ALPINE_CT" "$REMOTE:$WEBAPP_NAME" \
  --project "$PROJECT" --network "$NETWORK" --storage "$POOL"
# go-git's local-path transport shells out to git (see
# node-tunnel-survives-nat-and-provisions.sh); production uses git:// or https.
check "web app container has a lease on $NETWORK" wait_lease "$WEBAPP_NAME"
check "web app container has git" webapp_exec apk add --no-cache git
WEBAPP_IP=$(webapp_exec sh -c "ip -4 -o addr show eth0 | awk '{print \$4}' | cut -d/ -f1" 2>/dev/null)
check "web app container has a $NETWORK address" test -n "$WEBAPP_IP"
check "fleet repo, binaries, cert and operator key pushed" bash -c "
  incus file push -r --project '$PROJECT' '$WORK_DIR/fleet.git' '$REMOTE:$WEBAPP_NAME/root/' &&
  incus file push --project '$PROJECT' '$WEB_BIN' '$REMOTE:$WEBAPP_NAME/web' --mode=0755 &&
  incus file push --project '$PROJECT' '$BOOTSTRAP_BIN' '$REMOTE:$WEBAPP_NAME/bootstrap' --mode=0755 &&
  incus file push --project '$PROJECT' '$WORK_DIR/cert/client.crt' '$REMOTE:$WEBAPP_NAME/root/client.crt' &&
  incus file push --project '$PROJECT' '$WORK_DIR/operator.key' '$REMOTE:$WEBAPP_NAME/root/operator.key' --mode=0600
"

echo "Starting the web app (WIREGUARD_ENDPOINT=$WEBAPP_IP:$WG_PORT, no BASE_IMAGE_PATH) ..."
incus_exec_bg "$WORK_DIR/webapp.log" "$REMOTE:$WEBAPP_NAME" -- env \
  API_HOST_LISTEN_ADDR=127.0.0.1:8080 \
  STORE_PATH=/root/store.db \
  CLIENT_CERT_PATH=/root/client.crt \
  CONFIG_REPO_URL=/root/fleet.git \
  CONFIG_REPO_PRIVATE=true \
  CONFIG_SYNC_INTERVAL=5s \
  WIREGUARD_ENDPOINT="$WEBAPP_IP:$WG_PORT" \
  WIREGUARD_PORT="$WG_PORT" \
  WIREGUARD_OPERATOR_PEERS="$OPERATOR_ADDR=$OPERATOR_PUB" \
  /web

APP_PUB=""
NODE_TUNNEL_IP=""
for _ in $(seq 1 15); do
  APP_PUB=$(grep -oE 'wireguard public key [A-Za-z0-9+/=]+' "$WORK_DIR/webapp.log" 2>/dev/null | tail -1 | awk '{print $4}')
  NODE_TUNNEL_IP=$(webapp_exec wget -qO- http://127.0.0.1:8080/instances 2>/dev/null |
    jq -r '.[] | select(.Name == "node0") | .TunnelIP // empty' 2>/dev/null)
  [ -n "$APP_PUB" ] && [ -n "$NODE_TUNNEL_IP" ] && break
  sleep 2
done
check "web app logged its WireGuard public key" test -n "$APP_PUB"
check "web app synced and assigned node0 a tunnel address" test -n "$NODE_TUNNEL_IP"

# cli <verb> [args...] — `bootstrap api` as the operator peer, inside the web
# app's container (the tunnel endpoint is local there). Its stdout and stderr
# land in $WORK_DIR/cli.out and cli.err.
cli() {
  webapp_exec /bootstrap api "$@" --private-key-file /root/operator.key --local-addr "$OPERATOR_ADDR" \
    --server-public-key "$APP_PUB" --endpoint "127.0.0.1:$WG_PORT" \
    >"$WORK_DIR/cli.out" 2>"$WORK_DIR/cli.err"
}

cli image node0 --output /root/served.img
rc=$?
if [ "$rc" -ne 0 ] && grep -qF "BASE_IMAGE_PATH" "$WORK_DIR/cli.err"; then
  record_pass "the web app's image route is off (503, no BASE_IMAGE_PATH)"
else
  record_fail "the web app's image route is off (503, no BASE_IMAGE_PATH)" "exit $rc: $(tail -n 1 "$WORK_DIR/cli.err")"
fi

echo
echo "== 3. The CLI writes node0's SEED_DATA seed media over the tunnel =="
if cli seed-media node0 --output /root/seed.iso; then
  record_pass "api seed-media node0 exits 0"
else
  record_fail "api seed-media node0 exits 0" "$(tail -n 3 "$WORK_DIR/cli.err" | tr '\n' '|')"
fi
check "the CLI says to remove the media after install" grep -qF "Remove the media once the install finishes" "$WORK_DIR/cli.err"
check_eq "the seed media is mode 600" 600 "$(webapp_exec stat -c %a /root/seed.iso 2>/dev/null)"
check "seed media pulled from the web app's container" \
  incus file pull --project "$PROJECT" "$REMOTE:$WEBAPP_NAME/root/seed.iso" "$WORK_DIR/seed.iso"
if have_cmd blkid; then
  probe=$(blkid -p -o export "$WORK_DIR/seed.iso" 2>&1)
  # shellcheck disable=SC2016 # $1 is the inner bash's argument, not pasted in
  check "the seed media is an iso9660 volume labelled SEED_DATA" \
    bash -c 'grep -qx TYPE=iso9660 <<<"$1" && grep -qx LABEL=SEED_DATA <<<"$1"' _ "$probe"
else
  # shellcheck disable=SC2016 # $1 is the inner bash's argument, not pasted in
  check "the seed media is an ISO 9660 volume labelled SEED_DATA" \
    bash -c 'dd if="$1" bs=1 skip=32808 count=9 2>/dev/null | grep -qx SEED_DATA' _ "$WORK_DIR/seed.iso"
fi
check "the seed media carries node0's web app key" grep -aqF "$APP_PUB" "$WORK_DIR/seed.iso"
check "seed media imported on $REMOTE as an ISO volume" \
  incus storage volume import "$REMOTE:$POOL" "$WORK_DIR/seed.iso" "$ISO_VOL" --type=iso --project "$PROJECT"

echo
echo "== 4. node0 boots the stock IncusOS image with the seed media attached =="
check "the stock base image carries no seed for node0" bash -c "! grep -aqF '$MAC' '$INCUSOS_BASE_IMAGE'"
img_bytes=$(stat -c%s "$INCUSOS_BASE_IMAGE")
vol_gib=$(((img_bytes + (1 << 30) - 1) / (1 << 30)))
# As node-tunnel-survives-nat-and-provisions.sh: a throwaway Alpine VM dd's
# the raw image onto a block volume, which node0 then boots from.
check "stock base image written onto $REMOTE as a block volume" bash -c "
  set -eu
  incus storage volume create '$REMOTE:$POOL' '$BASE_VOL' --type=block size=${vol_gib}GiB --project '$PROJECT' &&
  incus launch '$VALIDATE_ALPINE_VM' '$REMOTE:$WRITER_NAME' --vm --project '$PROJECT' \
    --network '$NETWORK' --storage '$POOL' \
    -c security.secureboot=false -c limits.cpu=1 -c limits.memory=512MiB &&
  incus config device add '$REMOTE:$WRITER_NAME' raw-disk disk pool='$POOL' source='$BASE_VOL' --project '$PROJECT' &&
  for _ in \$(seq 1 30); do
    incus exec --project '$PROJECT' '$REMOTE:$WRITER_NAME' -- true >/dev/null 2>&1 && break
    sleep 2
  done &&
  writer_dev=\$(incus exec --project '$PROJECT' '$REMOTE:$WRITER_NAME' -- \
    sh -c \"awk '\\\$4 ~ /^sd[a-z]+\\\$/ && \\\$4 != \\\"sda\\\" {print \\\$4}' /proc/partitions\" | head -1) &&
  [ -n \"\$writer_dev\" ] &&
  cat '$INCUSOS_BASE_IMAGE' | incus exec --project '$PROJECT' '$REMOTE:$WRITER_NAME' -- dd of=/dev/\$writer_dev bs=4M &&
  incus delete --force --project '$PROJECT' '$REMOTE:$WRITER_NAME'
"

check "node0 created: empty disk, stock image as install media, seed ISO as CD-ROM" bash -c "
  incus init --empty --vm --project '$PROJECT' '$REMOTE:$VM_NAME' \
    -c security.secureboot=false -c limits.cpu=2 -c limits.memory=2GiB \
    --storage '$POOL' -d root,size=50GiB &&
  incus config device add --project '$PROJECT' '$REMOTE:$VM_NAME' install-media disk pool='$POOL' source='$BASE_VOL' boot.priority=10 &&
  incus config device add --project '$PROJECT' '$REMOTE:$VM_NAME' seed-media disk pool='$POOL' source='$ISO_VOL' &&
  incus config device add --project '$PROJECT' '$REMOTE:$VM_NAME' eth0 nic network='$NETWORK' hwaddr='$MAC' &&
  incus start --project '$PROJECT' '$REMOTE:$VM_NAME'
"

echo "Waiting up to 8 minutes for the install to complete ..."
if wait_for_console_text "successfully installed" 480; then
  record_pass "node0 installs IncusOS from the stock image and the seed media"
else
  record_fail "node0 installs IncusOS from the stock image and the seed media" "console tail: $(console_log | tail -15 | tr '\n' '|')"
fi
check "install media and seed media detached, node0 restarted from its disk" bash -c "
  incus stop --project '$PROJECT' '$REMOTE:$VM_NAME' --force &&
  incus config device remove --project '$PROJECT' '$REMOTE:$VM_NAME' install-media &&
  incus config device remove --project '$PROJECT' '$REMOTE:$VM_NAME' seed-media &&
  incus start --project '$PROJECT' '$REMOTE:$VM_NAME'
"

echo
echo "== 5. node0's tunnel to the web app handshakes =="
check "probe container ready on $NETWORK" bash -c "
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

# node_net_state — node0's IncusOS network state over the LAN, with the
# break-glass cert its seed told it to trust. Both endpoint spellings, as in
# node-tunnel-survives-nat-and-provisions.sh (IncusOS moved it).
node_net_state() {
  local path body
  for path in /os/1.0/system/network /1.0/system/network; do
    body=$(incus exec --project "$PROJECT" "$REMOTE:$PROBE_NAME" -- \
      curl --cert /root/client.crt --key /root/client.key -k -s \
      "https://$STATIC_IP:8443$path" 2>/dev/null)
    if [ -n "$body" ] && ! grep -q '"error_code":404' <<<"$body"; then
      printf '%s' "$body"
      return 0
    fi
  done
  return 1
}

# The web app's peer, matched by its key, must report latest_handshake, which
# IncusOS omits until a handshake completes.
echo "Waiting up to 5 minutes for node0's handshake with the web app ..."
handshake_seen=false
last_peer=""
for _ in $(seq 1 60); do
  last_peer=$(node_net_state | jq -c --arg k "$APP_PUB" \
    '.metadata.state.interfaces.wg0.wireguard.peers[]? | select(.public_key == $k)' 2>/dev/null)
  if [ -n "$last_peer" ] && jq -e '(.latest_handshake // "") != ""' <<<"$last_peer" >/dev/null 2>&1; then
    handshake_seen=true
    break
  fi
  sleep 5
done
if "$handshake_seen"; then
  record_pass "node0's WireGuard tunnel to the web app shows a completed handshake"
else
  record_fail "node0's WireGuard tunnel to the web app shows a completed handshake" \
    "web app peer: ${last_peer:-<absent from wg0>}; console tail: $(console_log | tail -20 | tr '\n' '|')"
fi
check "the installed node0 didn't stop on leftover install media" \
  bash -c "! incus console --project '$PROJECT' '$REMOTE:$VM_NAME' --show-log 2>&1 | tr -d '\0' | grep -qF 'install media detected'"

summary
