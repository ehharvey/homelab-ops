#! /bin/bash
# Validates GH issue #40 and Phase 2's Roadmap "Done when": the app can take a
# new Instance entry from the synced repo and produce a working installer
# end-to-end, without the bootstrap CLI.
#
# This is the sprint-closing script and a hybrid of the two existing
# validation families:
#   - Web-app family (validate-issue-38.sh/-39.sh): drive the running app via
#     docker compose + curl to *produce* the .img — no bootstrap CLI in the
#     provisioning path.
#   - Bootstrap/Phase-0 family (validate-issue-5.sh): stream that .img onto
#     the real homelab-host Incus remote, boot a VM, and prove cert trust
#     over HTTPS.
#
# Unlike validate-issue-39.sh (which deliberately stops at "structurally a
# seeded disk image" and defers the full boot-and-cert-trust check here),
# this always boots a real Incus VM off the web-app-produced image — no
# skip-as-fail: it hard-fails if homelab-host or INCUSOS_BASE_IMAGE are
# unavailable, since this is the strict proof of "working installer" for the
# whole phase, not a route-level regression guard.
#
# Requires: docker compose, curl, jq, go, openssl, incus. Needs the real
# "homelab-host" Incus remote / "homelab-dev" project / "home-lan" network
# set up by .devcontainer/scripts/2-setup-dev-network.sh, and a real bootable
# INCUSOS_BASE_IMAGE (same one validate-issue-5.sh needs). The image route
# copies the multi-GB base image into the container's /tmp per request, so
# the devcontainer host needs enough writable disk.
#
# Don't run concurrently with validate-issue-5.sh: both drive real VMs on the
# same live home-lan bridge / homelab-host remote, and validate-issue-5.sh
# hardcodes STATIC_IP=192.168.1.201 — the same address IPAM will hand node1
# here.
#
# Intended to run INSIDE the devcontainer, from the repo root.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

pass=0
fail=0

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$ROOT_DIR/bin/bootstrap"
CERT_DIR="$WORK_DIR/cert"
export CERT_DIR
OVERRIDE="$(mktemp /tmp/compose-sprint-3-override.XXXXXX.yml)"

REMOTE="homelab-host"
PROJECT="homelab-dev"
NETWORK="home-lan"
POOL="default"
MAC="aa:bb:cc:dd:ee:40"
VM_NAME="validate-sprint-3-$$"
WRITER_NAME="validate-sprint-3-writer-$$"
PROBE_NAME="validate-sprint-3-probe-$$"
SEED_VOL="validate-sprint-3-seeded-img-$$"

compose() {
  docker compose -f "$ROOT_DIR/docker-compose.yml" -f "$OVERRIDE" "$@"
}

cleanup() {
  compose down >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$WRITER_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$PROBE_NAME" >/dev/null 2>&1
  incus storage volume delete "$REMOTE:$POOL" "$SEED_VOL" --project "$PROJECT" >/dev/null 2>&1
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

echo "== 0. Hard prerequisites (hard-fail, no skip-as-fail) =="
missing=0
for c in docker curl jq go openssl incus; do
  if ! command -v "$c" >/dev/null 2>&1; then
    echo "ERROR: required command not found: $c" >&2
    missing=1
  fi
done
if ! docker compose version >/dev/null 2>&1; then
  echo "ERROR: 'docker compose' not available" >&2
  missing=1
fi
if ! incus info "$REMOTE:" >/dev/null 2>&1; then
  echo "ERROR: incus remote '$REMOTE' not reachable" >&2
  missing=1
fi
if ! incus project list "$REMOTE:" -f csv | cut -d, -f1 | sed 's/ (current)$//' | grep -qx "$PROJECT"; then
  echo "ERROR: incus project '$PROJECT' not found on $REMOTE" >&2
  missing=1
fi
if ! incus network list "$REMOTE:" --project "$PROJECT" -f csv | cut -d, -f1 | sed 's/ (current)$//' | grep -qx "$NETWORK"; then
  echo "ERROR: incus network '$NETWORK' not found in project '$PROJECT' on $REMOTE" >&2
  missing=1
fi
if [ -z "${INCUSOS_BASE_IMAGE:-}" ]; then
  echo "ERROR: INCUSOS_BASE_IMAGE not set" >&2
  missing=1
elif [ ! -f "$INCUSOS_BASE_IMAGE" ]; then
  echo "ERROR: INCUSOS_BASE_IMAGE=$INCUSOS_BASE_IMAGE not found" >&2
  missing=1
fi
if [ "$missing" -ne 0 ]; then
  echo "ERROR: prerequisites not met — aborting before doing any work" >&2
  exit 2
fi
echo "PASS: all hard prerequisites met"
pass=$((pass + 1))

echo
echo "== 1. Operator cert (the only bootstrap CLI use — cert generation, not provisioning) =="
check "build bootstrap binary" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap binary not built or not executable: $BOOTSTRAP_BIN" >&2
  exit 2
fi
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "validate-sprint-3"

echo
echo "== 2. Bring up the web app configured to build real images =="
BASE_IMAGE_ABS="$(cd "$(dirname "$INCUSOS_BASE_IMAGE")" && pwd)/$(basename "$INCUSOS_BASE_IMAGE")"
export BASE_IMAGE_ABS
# WIREGUARD_ENDPOINT is required alongside the cert and base image: #107 gated
# both the seed and image routes on it ("wireguard not configured"), and this
# script never set it. It is a *config* gate, not a hardware one —
# internal/wireguard runs the tunnel over netstack.CreateNetTUN +
# conn.NewDefaultBind() (userspace, no NET_ADMIN, an unprivileged
# net.ListenUDP), and resolveInstanceSeed mints the node credential offline
# without dialing anything. So a loopback endpoint satisfies it; the port is
# already published by docker-compose.yml. See #129, which fixed the same defect
# in validate-issue-38.sh and -39.sh, and docs/Decisions.md §20.
#
# Note this makes tunnel startup FATAL rather than degraded (see cmd/web/main.go
# — a *set* endpoint expresses operator intent), so the stack will fail to come
# up if 51820 is already bound. That is the correct failure: loud, not silent.
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

check "docker compose up --build succeeds" compose up --build -d

base_url="http://localhost:8080"
for _ in $(seq 1 20); do
  curl -s -o /dev/null "$base_url/healthz" && break
  sleep 0.5
done
check "web is reachable" curl -sf -o /dev/null "$base_url/healthz"

echo
echo "== 3. Sync a home-lan fleet whose instances (node0/node1) omit static_ip =="
compose exec -T config-repo sh -c '
  set -e
  rm -rf /tmp/validate-work
  git clone --no-hardlinks /srv/git/fleet.git /tmp/validate-work
  cd /tmp/validate-work
  git config user.email dev@homelab-ops.local
  git config user.name "validate-sprint-3"
  cat > fleet.yaml <<EOF
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dhcp_excluded_range: 192.168.1.200-192.168.1.250
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: aa:bb:cc:dd:ee:40
network: home-lan
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
---
kind: Instance
name: node1
mac: aa:bb:cc:dd:ee:41
network: home-lan
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
EOF
  git add fleet.yaml
  git commit -m "validate-sprint-3: static_ip-less node0/node1 on home-lan" >/dev/null
  git push origin main >/dev/null 2>&1
' >/dev/null 2>&1
check "pushed a static_ip-less two-node fleet to the fixture repo" \
  compose exec -T config-repo test -d /tmp/validate-work

sync_resp=$(curl -s -X POST "$base_url/sync")
check_json "POST /sync returns a commit SHA" "$sync_resp" '.commit | length > 0'
check_json "sync reports exactly 2 instances" "$sync_resp" '.instances == 2'

echo
echo "== 4. Prove IPAM + seed + cert at the API layer =="
instances_resp=$(curl -s "$base_url/instances")
ip0=$(echo "$instances_resp" | jq -r '.[] | select(.Name=="node0") | .StaticIP')
ip1=$(echo "$instances_resp" | jq -r '.[] | select(.Name=="node1") | .StaticIP')

in_pool() {
  local ip="$1" last
  last="${ip##*.}"
  [ -n "$last" ] && [ "$last" -ge 200 ] 2>/dev/null && [ "$last" -le 250 ] 2>/dev/null
}
if in_pool "$ip0"; then
  echo "PASS: node0 has an IPAM-assigned StaticIP in .200-.250 ($ip0)"
  pass=$((pass + 1))
else
  echo "FAIL: node0 has an IPAM-assigned StaticIP in .200-.250 (got: $ip0)"
  fail=$((fail + 1))
fi
if in_pool "$ip1"; then
  echo "PASS: node1 has an IPAM-assigned StaticIP in .200-.250 ($ip1)"
  pass=$((pass + 1))
else
  echo "FAIL: node1 has an IPAM-assigned StaticIP in .200-.250 (got: $ip1)"
  fail=$((fail + 1))
fi
if [ -n "$ip0" ] && [ -n "$ip1" ] && [ "$ip0" != "$ip1" ]; then
  echo "PASS: node0 and node1 got distinct addresses ($ip0 vs $ip1)"
  pass=$((pass + 1))
else
  echo "FAIL: node0 and node1 did not get distinct addresses (got: $ip0 / $ip1)"
  fail=$((fail + 1))
fi

seed_resp=$(curl -s -X POST "$base_url/instances/node0/seed")
check_json "POST /instances/node0/seed returns network_yaml" "$seed_resp" '.network_yaml | length > 0'
network_yaml=$(echo "$seed_resp" | jq -r '.network_yaml')
check "network_yaml carries node0's IPAM-assigned address ($ip0/24)" \
  bash -c "echo '$network_yaml' | grep -q '$ip0/24'"
check "network_yaml carries a default route (0.0.0.0/0)" \
  bash -c "echo '$network_yaml' | grep -q '0.0.0.0/0'"
check "network_yaml routes the default via the gateway (192.168.1.1)" \
  bash -c "echo '$network_yaml' | grep -q '192.168.1.1'"

ASSIGNED_IP=$(echo "$network_yaml" | grep -oE '192\.168\.1\.[0-9]{1,3}/24' | head -1 | cut -d/ -f1)
check "extracted an ASSIGNED_IP from rendered network_yaml" test -n "$ASSIGNED_IP"

cert_der_b64=$(openssl x509 -in "$CERT_DIR/client.crt" -outform DER | base64 -w0)
incus_yaml=$(echo "$seed_resp" | jq -r '.incus_yaml')
check "node0's incus_yaml embeds the operator's break-glass cert" \
  bash -c "echo '$incus_yaml' | grep -qF '$cert_der_b64'"

seed_resp2=$(curl -s -X POST "$base_url/instances/node0/seed")
incus_yaml2=$(echo "$seed_resp2" | jq -r '.incus_yaml')
if [ "$incus_yaml" = "$incus_yaml2" ]; then
  echo "PASS: node0's incus_yaml is byte-identical across repeated seed calls (idempotent)"
  pass=$((pass + 1))
else
  echo "FAIL: node0's incus_yaml is byte-identical across repeated seed calls (idempotent)"
  fail=$((fail + 1))
fi

seed_resp_node1=$(curl -s -X POST "$base_url/instances/node1/seed")
incus_yaml_node1=$(echo "$seed_resp_node1" | jq -r '.incus_yaml')
check "node1's incus_yaml embeds the same break-glass cert as node0" \
  bash -c "echo '$incus_yaml_node1' | grep -qF '$cert_der_b64'"

echo
echo "== 5. Download the web-app-produced image (#39) =="
out_img="$WORK_DIR/node0.img"
headers="$WORK_DIR/headers.txt"
status=$(curl -s -D "$headers" -o "$out_img" -w '%{http_code}' "$base_url/instances/node0/image")

check "image route returns 200" bash -c "[ '$status' = '200' ]"
check "response Content-Type is application/octet-stream" \
  bash -c "grep -qi 'content-type:.*application/octet-stream' '$headers'"
check "response is an attachment named node0.img (Content-Disposition)" \
  bash -c "grep -qi 'content-disposition:.*attachment' '$headers' && grep -qi 'filename=\"node0.img\"' '$headers'"
check "downloaded .img is non-empty" bash -c "[ -s '$out_img' ]"

out_bytes=$(stat -c%s "$out_img")
base_bytes=$(stat -c%s "$INCUSOS_BASE_IMAGE")
check "downloaded .img is a plausibly-sized disk image (>= 1 MiB)" \
  bash -c "[ '$out_bytes' -ge $((1 << 20)) ]"

# In-place seed injection keeps the size identical to the base, so equality is
# the assertion to make here.
#
# This replaces a `cmp -s` check — byte-identical to the one in
# validate-issue-39.sh — that asserted the download merely *differed* from the
# base. That passed whenever the two files were not byte-identical, which a
# 40-byte 503 error body trivially is not, so it reported PASS for the exact
# failure mode it existed to catch. See #115.
check "downloaded .img is exactly the base image's size (in-place injection)" \
  bash -c "[ '$out_bytes' -eq '$base_bytes' ]"

# Positive proof the seed was written rather than the base streamed back: the
# seed is injected as an uncompressed tar of the rendered YAML, so node0's MAC
# is greppable in the image bytes. The negative control on the base is what
# makes that meaningful. Neither can be satisfied by an error body.
check "downloaded .img carries node0's seeded MAC" \
  grep -aq "$MAC" "$out_img"
check "the base image does not carry that MAC (so the above proves injection)" \
  bash -c "! grep -aq '$MAC' '$INCUSOS_BASE_IMAGE'"

echo
echo "== 6. Boot a real VM off that image and prove cert trust =="
img_bytes=$(stat -c%s "$out_img")
vol_gib=$(((img_bytes + (1 << 30) - 1) / (1 << 30)))

# Stream node0.img onto the remote as a block-type custom volume, via a
# disposable "writer" VM — see validate-issue-5.sh's header comment for why a
# plain `disk source=<local path>` device can't be used against a remote
# target.
check "seed .img streamed onto $REMOTE as a block volume" bash -c "
  set -eu
  incus storage volume create '$REMOTE:$POOL' '$SEED_VOL' --type=block size=${vol_gib}GiB --project '$PROJECT' &&
  incus launch images:alpine/edge '$REMOTE:$WRITER_NAME' --vm --project '$PROJECT' \
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
  cat '$out_img' | incus exec --project '$PROJECT' '$REMOTE:$WRITER_NAME' -- dd of=/dev/\$writer_dev bs=4M &&
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
  echo "PASS: VM installs IncusOS from the web-app-produced image"
  pass=$((pass + 1))

  # IncusOS halts after installing and waits for the install media to be
  # removed before it'll boot the installed system.
  incus stop --project "$PROJECT" "$REMOTE:$VM_NAME" --force >/dev/null 2>&1
  incus config device remove --project "$PROJECT" "$REMOTE:$VM_NAME" install-media >/dev/null 2>&1
  incus start --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
else
  echo "FAIL: VM installs IncusOS from the web-app-produced image"
  fail=$((fail + 1))
fi

check "probe instance ready on $NETWORK" bash -c "
  set -eu
  incus launch images:alpine/edge '$REMOTE:$PROBE_NAME' --project '$PROJECT' --network '$NETWORK' --storage '$POOL' &&
  for _ in \$(seq 1 15); do
    incus exec --project '$PROJECT' '$REMOTE:$PROBE_NAME' -- apk add --no-cache curl >/dev/null 2>&1 && break
    sleep 2
  done &&
  incus exec --project '$PROJECT' '$REMOTE:$PROBE_NAME' -- sh -c 'command -v curl' &&
  incus file push '$CERT_DIR/client.crt' '$REMOTE:$PROBE_NAME/root/client.crt' --project '$PROJECT' &&
  incus file push '$CERT_DIR/client.key' '$REMOTE:$PROBE_NAME/root/client.key' --project '$PROJECT'
"

echo "Waiting up to 5 minutes for node0 to boot the installed system and bring up Incus on https://$ASSIGNED_IP:8443 (checked from the probe instance) ..."
reachable=false
for _ in $(seq 1 60); do
  if incus exec --project "$PROJECT" "$REMOTE:$PROBE_NAME" -- \
    curl --cert /root/client.crt --key /root/client.key -k -sf "https://$ASSIGNED_IP:8443/1.0" -o /dev/null >/dev/null 2>&1; then
    reachable=true
    break
  fi
  sleep 5
done

# One authenticated request proves both install success and cert trust — for
# the IPAM-assigned IP and the web-app-embedded cert.
if "$reachable"; then
  response=$(incus exec --project "$PROJECT" "$REMOTE:$PROBE_NAME" -- \
    curl --cert /root/client.crt --key /root/client.key -k -s "https://$ASSIGNED_IP:8443/1.0" 2>/dev/null)
  if echo "$response" | grep -q '"auth":"trusted"'; then
    echo "PASS: node trusts the break-glass cert and is reachable over Incus API"
    pass=$((pass + 1))
  else
    echo "FAIL: node trusts the break-glass cert and is reachable over Incus API (reachable, but auth was not \"trusted\": $response)"
    fail=$((fail + 1))
  fi
else
  echo "FAIL: node trusts the break-glass cert and is reachable over Incus API (node never became reachable)"
  fail=$((fail + 1))
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
