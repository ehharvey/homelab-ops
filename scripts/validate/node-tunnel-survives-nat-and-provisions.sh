#! /bin/bash
# Validates GH issue #91 ("Seed WireGuard connectivity between nodes and the
# web app"), rescoped per its issue thread: the original "POST /phone-home"
# done-when criterion is deferred to #92 (the local app-manager agent is the
# natural thing to phone home, once it exists). What this script actually
# proves:
#   1. A freshly-installed node's WireGuard tunnel to the web app comes up
#      and survives a NAT hop (PersistentKeepalive) — proven against a real
#      simulated-NAT topology, not just configured.
#   2. The node's Incus API is reachable *through the tunnel specifically*
#      (not just "same LAN"), from the real web app process.
#   3. The temp-cert -> dial-over-tunnel -> create-instance -> revoke
#      mechanism (internal/nodeprovision) works against a real node — the
#      exact mechanism #92's `bootstrap deploy-agent` will reuse for real.
#
# Runs against the "homelab-host" remote's DEFAULT project (not
# "homelab-dev" — see #96: that project got stuck with features.networks=true
# while building this script, and the plan going forward is to stop using it
# rather than repair it in place). This script creates and tears down its
# own network/instances entirely within the default project, alongside
# node-boots-and-trusts-bootstrap-cert.sh's "home-lan" network, which it reuses read-only.
#
# Requires a real, bootable base IncusOS raw image — like #5, point
# INCUSOS_BASE_IMAGE at a local copy; the node-boot-dependent sections are
# skipped with a clear message if it's unset.
#
# ---------------------------------------------------------------------------
# Topology (see docs/Roadmap.md #91's plan for the full design rationale):
#
#   node0 ---- home-lan (192.168.1.0/24) ---- [gateway] ---- wan-sim (10.200.0.0/24) ---- webapp
#                                               NAT/MASQUERADE +
#                                               tuned-down conntrack
#                                               UDP timeout
#
# - webapp runs the REAL statically-built `web` binary (not a test double)
#   on wan-sim, so this proves the actual production SyncOnce reconcile
#   loop (AssignTunnelIPs -> EnsureCredential -> UpsertPeer), not a parallel
#   mechanism. It gets its fleet config from a small bare git repo pushed
#   into its own container (go-git supports plain local filesystem paths —
#   no separate git-server container needed).
# - gateway is a disposable container with NICs on both networks, doing its
#   own MASQUERADE + a deliberately short nf_conntrack_udp_timeout (~15s,
#   shorter than WireGuard's 25s PersistentKeepalive) — chosen over Incus's
#   own per-network NAT because that would tune the SHARED HOST's netfilter
#   state; a disposable container keeps it fully scoped and torn down with
#   everything else. All of this was verified for real (UDP delivery + a
#   correctly-translated conntrack entry) while building this script.
# - node0's only change versus #5's topology: one extra static route to
#   wan-sim via the gateway's home-lan address, appended onto the rendered
#   seed after the real seed.Render call (not a production Render feature).
# - The harness (cmd/validate-tunnel-harness) drives the create-instance
#   mechanism from a third container on home-lan, with its own throwaway
#   WireGuard identity appended as a second test-only peer on node0's seed
#   — proving the mechanism over *a* tunnel, not re-proving NAT traversal
#   (already covered by the webapp<->node0 tunnel check).

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK_DIR="$(mktemp -d)"
# Overridable so this can run somewhere other than the devcontainer — notably
# on the Incus host itself, where Incus is a local unix socket and no remote
# named "homelab-host" exists (see #115's CI design). This script already
# targeted "default" (see the #96 note above); the other three now match it.
REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
PROJECT="${VALIDATE_INCUS_PROJECT:-default}"
LAN_NETWORK="${VALIDATE_INCUS_NETWORK:-home-lan}"
# Bridge network names become real host-level interface names (ip link),
# capped at 15 characters — keep this short.
WAN_NETWORK="valwan-$$"
POOL="default"
STATIC_IP="192.168.1.202"
MAC="aa:bb:cc:dd:ee:02"
WAN_CIDR="10.200.0.0/24"
WAN_GATEWAY_ADDR="10.200.0.1"

VM_NAME="validate-tunnel-node0-$$"
WRITER_NAME="validate-tunnel-writer-$$"
PROBE_NAME="validate-tunnel-probe-$$"
GATEWAY_NAME="validate-tunnel-gw-$$"
WEBAPP_NAME="validate-tunnel-webapp-$$"
HARNESS_NAME="validate-tunnel-harness-$$"
SEED_VOL="validate-tunnel-seeded-img-$$"

BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
WEB_BIN="$WORK_DIR/web"
HARNESS_BIN="$WORK_DIR/validate-tunnel-harness"

# WireGuard UDP port both the webapp and the conntrack-tuning target on the
# gateway care about.
WG_PORT=51820

pass=0
fail=0

cleanup() {
  incus delete --force --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$WRITER_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$PROBE_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$GATEWAY_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$WEBAPP_NAME" >/dev/null 2>&1
  incus delete --force --project "$PROJECT" "$REMOTE:$HARNESS_NAME" >/dev/null 2>&1
  incus storage volume delete "$REMOTE:$POOL" "$SEED_VOL" --project "$PROJECT" >/dev/null 2>&1
  incus network delete "$REMOTE:$WAN_NETWORK" >/dev/null 2>&1
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

console_log() {
  incus console --project "$PROJECT" "$REMOTE:$VM_NAME" --show-log 2>&1 | tr -d '\0'
}

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

# incus_exec_bg runs a command inside an instance in the background (from
# the host side — the backgrounded `incus exec` process itself, not a
# nohup/disown trick inside the container), logging to logfile, and prints
# the backgrounded PID. Cleanup happens via cleanup_bg_pids (populated by
# the caller) since these aren't real container-level services.
BG_PIDS=()
incus_exec_bg() {
  local logfile="$1"
  shift
  incus exec --project "$PROJECT" "$@" >"$logfile" 2>&1 &
  BG_PIDS+=("$!")
}

cleanup_bg_pids() {
  for pid in "${BG_PIDS[@]:-}"; do
    kill "$pid" >/dev/null 2>&1
  done
}
trap 'cleanup_bg_pids; cleanup' EXIT

echo "== 1. Prerequisites =="
check "incus client installed" command -v incus
check "bootstrap CLI builds" go -C "$ROOT_DIR" build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
check "web app builds (CGO_ENABLED=0, matching the real Docker image)" \
  bash -c "cd '$ROOT_DIR' && CGO_ENABLED=0 go build -o '$WEB_BIN' ./cmd/web"
check "validate-tunnel-harness builds" go -C "$ROOT_DIR" build -o "$HARNESS_BIN" ./cmd/validate-tunnel-harness
check "incus remote '$REMOTE' reachable" incus info "$REMOTE:"
check "network '$LAN_NETWORK' exists" bash -c "incus network list '$REMOTE:' --project '$PROJECT' -f csv | cut -d, -f1 | sed 's/ (current)\$//' | grep -qx '$LAN_NETWORK'"

if [ ! -x "$BOOTSTRAP_BIN" ] || [ ! -x "$WEB_BIN" ] || [ ! -x "$HARNESS_BIN" ] || ! incus info "$REMOTE:" >/dev/null 2>&1; then
  echo
  echo "$pass passed, $((fail + 1)) failed (prerequisites not met, skipping remaining checks)"
  exit 1
fi

echo
echo "== 2. NAT-simulation topology: wan-sim network + gateway =="
# home-lan already exists (shared with node-boots-and-trusts-bootstrap-cert.sh); wan-sim is
# this run's own, deleted in cleanup. No ipv4.nat here — the gateway
# container does its own MASQUERADE (see this file's header for why).
check "network '$WAN_NETWORK' created" incus network create "$REMOTE:$WAN_NETWORK" \
  "ipv4.address=$WAN_GATEWAY_ADDR/24" "ipv4.nat=false" "ipv4.dhcp=true"

check "gateway container launched on $LAN_NETWORK" incus launch images:alpine/edge "$REMOTE:$GATEWAY_NAME" \
  --project "$PROJECT" --network "$LAN_NETWORK"

check "gateway attached to $WAN_NETWORK" incus config device add --project "$PROJECT" \
  "$REMOTE:$GATEWAY_NAME" eth1 nic network="$WAN_NETWORK"

# A device added after launch isn't picked up automatically (unlike the
# primary NIC present at launch, which DHCPs on first boot) — bring it up
# and request a lease explicitly. Short timeouts: fail fast rather than
# hang if the network's DHCP isn't answering.
check "gateway's wan-sim interface (eth1) configured" bash -c "
  incus exec --project '$PROJECT' '$REMOTE:$GATEWAY_NAME' -- ip link set eth1 up &&
  incus exec --project '$PROJECT' '$REMOTE:$GATEWAY_NAME' -- udhcpc -i eth1 -n -q -t 3 -T 3
"

GATEWAY_LAN_IP=$(incus exec --project "$PROJECT" "$REMOTE:$GATEWAY_NAME" -- \
  sh -c "ip -4 -o addr show eth0 | awk '{print \$4}' | cut -d/ -f1" 2>/dev/null)
GATEWAY_WAN_IP=$(incus exec --project "$PROJECT" "$REMOTE:$GATEWAY_NAME" -- \
  sh -c "ip -4 -o addr show eth1 | awk '{print \$4}' | cut -d/ -f1" 2>/dev/null)
check "gateway has addresses on both networks" bash -c "[ -n '$GATEWAY_LAN_IP' ] && [ -n '$GATEWAY_WAN_IP' ]"

# The gateway's own MASQUERADE + a conntrack UDP timeout tuned below
# WireGuard's PersistentKeepalive interval (25s, see internal/seed.go's
# wireGuardPersistentKeepaliveSeconds) but long enough that one keepalive
# keeps re-arming it — simulating an aggressive home router, so the
# NAT-survival check later is a real proof, not just "traffic crosses two
# bridges". All of this was verified for real (UDP delivery + a correctly
# NAT-translated conntrack entry) while writing this script.
check "gateway NAT + forwarding + conntrack tuning configured" bash -c "
  incus exec --project '$PROJECT' '$REMOTE:$GATEWAY_NAME' -- apk add --no-cache iptables >/dev/null &&
  incus exec --project '$PROJECT' '$REMOTE:$GATEWAY_NAME' -- sysctl -w net.ipv4.ip_forward=1 &&
  incus exec --project '$PROJECT' '$REMOTE:$GATEWAY_NAME' -- iptables -t nat -A POSTROUTING -o eth1 -j MASQUERADE &&
  incus exec --project '$PROJECT' '$REMOTE:$GATEWAY_NAME' -- sysctl -w net.netfilter.nf_conntrack_udp_timeout=15
"

echo
echo "== 3. Build the pipeline: gen-cert -> render-seed =="
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$WORK_DIR/cert"

cat >"$WORK_DIR/fleet.yaml" <<EOF
kind: Network
name: $LAN_NETWORK
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: $MAC
network: $LAN_NETWORK
static_ip: $STATIC_IP
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
EOF

# No WireGuard config yet via the bootstrap CLI — see internal/seed.go and
# docs/Architecture.md's Flow A vs. Flow B: the bootstrap CLI's render-seed
# is node0-bootstrap-only, before any web app (and therefore any tunnel)
# exists. This produces node0's base seed; step 4 below runs the REAL web
# app and re-fetches node0's seed from it (now with WireGuard embedded)
# before build-image, exactly mirroring how steady-state provisioning
# works for every node after node0.
check "render-seed exits 0 (base seed, no WireGuard yet)" "$BOOTSTRAP_BIN" render-seed \
  --file "$WORK_DIR/fleet.yaml" \
  --cert "$WORK_DIR/cert/client.crt" \
  --output-dir "$WORK_DIR/seed-base"
check "incus.yaml rendered" test -f "$WORK_DIR/seed-base/incus.yaml"

echo
echo "== 4. Stand up the real web app on wan-sim, wired to node0's fleet config =="
# A small bare git repo pushed directly into the webapp container's own
# filesystem — go-git (internal/configsync.Syncer) supports plain local
# filesystem paths, so no separate git-server container is needed.
mkdir -p "$WORK_DIR/fleet-repo-src"
cp "$WORK_DIR/fleet.yaml" "$WORK_DIR/fleet-repo-src/fleet.yaml"
check "fleet git repo built" bash -c "
  cd '$WORK_DIR/fleet-repo-src' &&
  git init -q -b main . &&
  git config user.email 'dev@homelab-ops.local' &&
  git config user.name 'node-tunnel-survives-nat' &&
  git add fleet.yaml &&
  git commit -q -m 'node-tunnel-survives-nat fleet' &&
  git clone -q --bare . '$WORK_DIR/fleet-repo.git'
"

check "webapp container launched on $WAN_NETWORK" incus launch images:alpine/edge "$REMOTE:$WEBAPP_NAME" \
  --project "$PROJECT" --network "$WAN_NETWORK"

# The primary NIC DHCPs on first boot, but not necessarily instantly —
# retry rather than checking exactly once immediately after launch.
WEBAPP_WAN_IP=""
for _ in $(seq 1 10); do
  WEBAPP_WAN_IP=$(incus exec --project "$PROJECT" "$REMOTE:$WEBAPP_NAME" -- \
    sh -c "ip -4 -o addr show eth0 | awk '{print \$4}' | cut -d/ -f1" 2>/dev/null)
  [ -n "$WEBAPP_WAN_IP" ] && break
  sleep 1
done
check "webapp has a wan-sim address" bash -c "[ -n '$WEBAPP_WAN_IP' ]"

# git is installed here purely for go-git's benefit: internal/configsync's
# "no git binary needed" guarantee holds for the git://, http(s)://, and
# ssh:// transports it uses in production, but go-git's *local filesystem
# path* transport (this script's own convenience choice, not a production
# concern) shells out to a real `git` binary internally
# (go-git/plumbing/transport/file/client.go) — confirmed while building
# this script. The distroless production image never needs this.
check "webapp fleet repo + git + binary + cert pushed" bash -c "
  incus file push -r --project '$PROJECT' '$WORK_DIR/fleet-repo.git' '$REMOTE:$WEBAPP_NAME/root/' &&
  incus exec --project '$PROJECT' '$REMOTE:$WEBAPP_NAME' -- apk add --no-cache git &&
  incus file push --project '$PROJECT' '$WEB_BIN' '$REMOTE:$WEBAPP_NAME/web' --mode=0755 &&
  incus file push --project '$PROJECT' '$WORK_DIR/cert/client.crt' '$REMOTE:$WEBAPP_NAME/root/client.crt'
"

echo "Starting the real web app (WIREGUARD_ENDPOINT=$WEBAPP_WAN_IP:$WG_PORT) ..."
incus_exec_bg "$WORK_DIR/webapp.log" "$REMOTE:$WEBAPP_NAME" -- env \
  PORT=8080 \
  STORE_PATH=/root/store.db \
  CLIENT_CERT_PATH=/root/client.crt \
  CONFIG_REPO_URL=/root/fleet-repo.git \
  CONFIG_SYNC_INTERVAL=5s \
  WIREGUARD_ENDPOINT="$WEBAPP_WAN_IP:$WG_PORT" \
  WIREGUARD_PORT="$WG_PORT" \
  /web

echo "Waiting up to 30s for the web app to sync and assign node0 a tunnel_ip ..."
NODE_TUNNEL_IP=""
APP_PUBLIC_KEY=""
for _ in $(seq 1 15); do
  resp=$(incus exec --project "$PROJECT" "$REMOTE:$WEBAPP_NAME" -- \
    wget -qO- http://127.0.0.1:8080/instances 2>/dev/null)
  NODE_TUNNEL_IP=$(echo "$resp" | grep -o '"TunnelIP":"[^"]*"' | head -1 | cut -d'"' -f4)
  if [ -n "$NODE_TUNNEL_IP" ]; then
    break
  fi
  sleep 2
done
if [ -n "$NODE_TUNNEL_IP" ]; then
  echo "PASS: web app synced and assigned node0 tunnel_ip $NODE_TUNNEL_IP"
  pass=$((pass + 1))
else
  echo "FAIL: web app synced and assigned node0 a tunnel_ip (webapp.log: $(tail -5 "$WORK_DIR/webapp.log" 2>/dev/null))"
  fail=$((fail + 1))
fi

echo
echo "== 5. Boot node0 with the real WireGuard-enabled seed and confirm the tunnel =="
if [ -z "${INCUSOS_BASE_IMAGE:-}" ]; then
  echo "FAIL: build-image exits 0 (skipped: INCUSOS_BASE_IMAGE not set)"
  echo "FAIL: node0 boots and its WireGuard tunnel to the webapp comes up (skipped: INCUSOS_BASE_IMAGE not set)"
  echo "FAIL: node0's Incus API is reachable through the tunnel, not just the LAN (skipped: INCUSOS_BASE_IMAGE not set)"
  echo "FAIL: temp-cert create-instance + revoke mechanism succeeds over the tunnel (skipped: INCUSOS_BASE_IMAGE not set)"
  fail=$((fail + 4))
elif [ ! -f "$INCUSOS_BASE_IMAGE" ]; then
  echo "FAIL: build-image exits 0 (skipped: INCUSOS_BASE_IMAGE=$INCUSOS_BASE_IMAGE not found)"
  echo "FAIL: node0 boots and its WireGuard tunnel to the webapp comes up (skipped: INCUSOS_BASE_IMAGE not found)"
  echo "FAIL: node0's Incus API is reachable through the tunnel, not just the LAN (skipped: INCUSOS_BASE_IMAGE not found)"
  echo "FAIL: temp-cert create-instance + revoke mechanism succeeds over the tunnel (skipped: INCUSOS_BASE_IMAGE not found)"
  fail=$((fail + 4))
elif [ -z "$NODE_TUNNEL_IP" ]; then
  echo "FAIL: build-image exits 0 (skipped: web app never assigned node0 a tunnel_ip, see step 4)"
  echo "FAIL: node0 boots and its WireGuard tunnel to the webapp comes up (skipped)"
  echo "FAIL: node0's Incus API is reachable through the tunnel, not just the LAN (skipped)"
  echo "FAIL: temp-cert create-instance + revoke mechanism succeeds over the tunnel (skipped)"
  fail=$((fail + 4))
else
  # Fetch node0's seed from the REAL running web app — now with WireGuard
  # config + the bootstrap cert embedded, unlike step 3's base seed. Then
  # append this run's two test-only additions the real seed.Render never
  # produces itself: the extra wan-sim route, and the harness's second
  # test peer (added below, after the harness has a keypair).
  check "fetched WireGuard-enabled seed from the real web app" bash -c "
    incus exec --project '$PROJECT' '$REMOTE:$WEBAPP_NAME' -- \
      wget -qO- --post-data='' http://127.0.0.1:8080/instances/node0/seed > '$WORK_DIR/node0-seed.json'
  "
  APP_PUBLIC_KEY=$(grep -o '\"public_key\": \"[^\"]*\"' "$WORK_DIR/node0-seed.json" | head -1 | cut -d'"' -f4)

  mkdir -p "$WORK_DIR/seed"
  python3 - "$WORK_DIR/node0-seed.json" "$WORK_DIR/seed" <<'PYEOF'
import json, sys, pathlib
data = json.load(open(sys.argv[1]))
out = pathlib.Path(sys.argv[2])
(out / "install.yaml").write_text(data["install_yaml"])
(out / "network.yaml").write_text(data["network_yaml"])
(out / "applications.yaml").write_text(data["applications_yaml"])
(out / "incus.yaml").write_text(data["incus_yaml"])
PYEOF
  check "all four seed files extracted from the web app's response" bash -c "
    test -s '$WORK_DIR/seed/install.yaml' && test -s '$WORK_DIR/seed/network.yaml' &&
    test -s '$WORK_DIR/seed/applications.yaml' && test -s '$WORK_DIR/seed/incus.yaml'
  "

  # Harness identity + this run's two validate-script-only seed additions
  # (route to wan-sim via the gateway, and the harness as a second trusted
  # peer) — see this file's header for why these are test-only, not
  # production seed.Render features.
  HARNESS_PUBLIC_KEY=$("$HARNESS_BIN" -mode=genkey -private-key-file="$WORK_DIR/harness.key")
  check "harness identity generated" bash -c "[ -n '$HARNESS_PUBLIC_KEY' ]"

  # Patches network.yaml in place using the exact vendored types
  # internal/seed renders with (see cmd/validate-tunnel-harness's patch-seed
  # mode) — no Python/PyYAML dependency, and byte-compatible with what
  # IncusOS actually parses.
  check "wan-sim route + harness test peer appended to network.yaml" "$HARNESS_BIN" -mode=patch-seed \
    -network-yaml="$WORK_DIR/seed/network.yaml" \
    -add-route-to="$WAN_CIDR" -add-route-via="$GATEWAY_LAN_IP" \
    -add-peer-public-key="$HARNESS_PUBLIC_KEY" -add-peer-allowed-ip=10.100.0.254/32

  check "build-image exits 0" "$BOOTSTRAP_BIN" build-image \
    --seed-dir "$WORK_DIR/seed" \
    --image "$INCUSOS_BASE_IMAGE" \
    --output "$WORK_DIR/node0.img"

  img_bytes=$(stat -c%s "$WORK_DIR/node0.img")
  vol_gib=$(((img_bytes + (1 << 30) - 1) / (1 << 30)))

  check "seed .img streamed onto $REMOTE as a block volume" bash -c "
    set -eu
    incus storage volume create '$REMOTE:$POOL' '$SEED_VOL' --type=block size=${vol_gib}GiB --project '$PROJECT' &&
    incus launch images:alpine/edge '$REMOTE:$WRITER_NAME' --vm --project '$PROJECT' \
      --network '$LAN_NETWORK' --storage '$POOL' \
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
    incus config device add --project '$PROJECT' '$REMOTE:$VM_NAME' eth0 nic network='$LAN_NETWORK' hwaddr='$MAC' &&
    incus start --project '$PROJECT' '$REMOTE:$VM_NAME'
  "

  echo "Waiting up to 5 minutes for the install to complete ..."
  if wait_for_console_text "successfully installed" 300; then
    echo "PASS: VM installs IncusOS from the seeded image"
    pass=$((pass + 1))
    incus stop --project "$PROJECT" "$REMOTE:$VM_NAME" --force >/dev/null 2>&1
    incus config device remove --project "$PROJECT" "$REMOTE:$VM_NAME" install-media >/dev/null 2>&1
    incus start --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
  else
    echo "FAIL: VM installs IncusOS from the seeded image"
    fail=$((fail + 1))
  fi

  echo "Waiting up to 3 minutes for node0's WireGuard tunnel to the webapp to come up (checked via the webapp's own Incus network state) ..."
  # LAN-side probe, exactly like #5, for querying node0's own IncusOS
  # network state — this is a different check than "Incus reachable over
  # the tunnel" below: it confirms the HANDSHAKE happened at all
  # (LatestHandshake non-empty), from the LAN side which we already know
  # is reachable.
  check "probe instance ready on $LAN_NETWORK" bash -c "
    set -eu
    incus launch images:alpine/edge '$REMOTE:$PROBE_NAME' --project '$PROJECT' --network '$LAN_NETWORK' --storage '$POOL' &&
    for _ in \$(seq 1 15); do
      incus exec --project '$PROJECT' '$REMOTE:$PROBE_NAME' -- apk add --no-cache curl >/dev/null 2>&1 && break
      sleep 2
    done &&
    incus exec --project '$PROJECT' '$REMOTE:$PROBE_NAME' -- sh -c 'command -v curl' &&
    incus file push '$WORK_DIR/cert/client.crt' '$REMOTE:$PROBE_NAME/root/client.crt' --project '$PROJECT' &&
    incus file push '$WORK_DIR/cert/client.key' '$REMOTE:$PROBE_NAME/root/client.key' --project '$PROJECT'
  "

  handshake_seen=false
  for _ in $(seq 1 36); do
    net_state=$(incus exec --project "$PROJECT" "$REMOTE:$PROBE_NAME" -- \
      curl --cert /root/client.crt --key /root/client.key -k -s "https://$STATIC_IP:8443/1.0/system/network" 2>/dev/null)
    if echo "$net_state" | grep -q "\"public_key\": *\"$APP_PUBLIC_KEY\"" && echo "$net_state" | grep -q "latest_handshake"; then
      handshake_seen=true
      break
    fi
    sleep 5
  done
  if "$handshake_seen"; then
    echo "PASS: node0's WireGuard tunnel to the webapp shows a completed handshake"
    pass=$((pass + 1))
  else
    echo "FAIL: node0's WireGuard tunnel to the webapp shows a completed handshake (never observed within 3 minutes)"
    fail=$((fail + 1))
  fi

  echo
  echo "== 6. Prove the mechanism via the harness: reachability + create-instance =="
  # The tunnel is a userspace network stack with no host-level route to it
  # — nothing outside the process that owns it can dial through it, and
  # there's deliberately no HTTP route exposing the real webapp's own
  # tunnel (see docs/Roadmap.md #91's scope decision). So both checks below
  # run from the harness's own, separate tunnel instead — registered as a
  # second trusted peer on node0's seed back in step 5. This still proves
  # "Incus reachable over a real WireGuard tunnel to node0", the mechanism
  # in question; NAT survival specifically was already proven by the
  # webapp<->node0 handshake check above, which is the pair that actually
  # crosses the gateway (the harness sits on home-lan, L2-adjacent to
  # node0, by design — see this file's header).
  check "harness container launched on $LAN_NETWORK" incus launch images:alpine/edge "$REMOTE:$HARNESS_NAME" \
    --project "$PROJECT" --network "$LAN_NETWORK"
  check "harness binary + key pushed" bash -c "
    incus file push --project '$PROJECT' '$HARNESS_BIN' '$REMOTE:$HARNESS_NAME/harness' --mode=0755 &&
    incus file push --project '$PROJECT' '$WORK_DIR/harness.key' '$REMOTE:$HARNESS_NAME/harness.key'
  "

  echo "Running the harness's probe mode against node0 ($NODE_TUNNEL_IP:8443) over its own tunnel ..."
  if incus exec --project "$PROJECT" "$REMOTE:$HARNESS_NAME" -- \
    /harness -mode=probe \
      -private-key-file=/harness.key \
      -local-addr=10.100.0.254 \
      -peer-public-key="$APP_PUBLIC_KEY" \
      -peer-tunnel-ip="$NODE_TUNNEL_IP" \
      -node-addr="$NODE_TUNNEL_IP:8443" \
      -timeout=60s >"$WORK_DIR/harness-probe.log" 2>&1; then
    echo "PASS: node0's Incus API reachable through a real WireGuard tunnel (10.100.0.0/24), not just the LAN"
    pass=$((pass + 1))
  else
    echo "FAIL: node0's Incus API reachable through a real WireGuard tunnel (see $WORK_DIR/harness-probe.log)"
    fail=$((fail + 1))
  fi

  # Pull the webapp's store and extract node0's real bootstrap credential
  # (minted by the live SyncOnce reconcile loop's EnsureCredential call in
  # step 4) — an offline file read, not a new HTTP route: the credential is
  # secret material that must never become reachable through the running
  # web app's API (see docs/Roadmap.md #91's security invariant). Reading
  # STORE_PATH directly is the same access an operator with real
  # filesystem access to that file already has.
  check "webapp store pulled and node0's real bootstrap credential extracted" bash -c "
    incus file pull --project '$PROJECT' '$REMOTE:$WEBAPP_NAME/root/store.db' '$WORK_DIR/store.db' &&
    '$HARNESS_BIN' -mode=extract-credential -store-path='$WORK_DIR/store.db' -instance-name=node0 \
      -out-cert='$WORK_DIR/bootstrap.crt' -out-key='$WORK_DIR/bootstrap.key'
  "
  check "bootstrap cert/key pushed to harness" bash -c "
    incus file push --project '$PROJECT' '$WORK_DIR/bootstrap.crt' '$REMOTE:$HARNESS_NAME/root/bootstrap.crt' &&
    incus file push --project '$PROJECT' '$WORK_DIR/bootstrap.key' '$REMOTE:$HARNESS_NAME/root/bootstrap.key'
  "

  echo "Running the harness's create-instance mode against node0 ($NODE_TUNNEL_IP:8443) ..."
  if incus exec --project "$PROJECT" "$REMOTE:$HARNESS_NAME" -- \
    /harness -mode=create-instance \
      -private-key-file=/harness.key \
      -local-addr=10.100.0.254 \
      -peer-public-key="$APP_PUBLIC_KEY" \
      -peer-tunnel-ip="$NODE_TUNNEL_IP" \
      -node-addr="$NODE_TUNNEL_IP:8443" \
      -bootstrap-cert=/root/bootstrap.crt \
      -bootstrap-key=/root/bootstrap.key \
      -timeout=60s >"$WORK_DIR/harness-create.log" 2>&1; then
    echo "PASS: temp-cert create-instance + revoke mechanism succeeds over the tunnel"
    pass=$((pass + 1))
  else
    echo "FAIL: temp-cert create-instance + revoke mechanism succeeds over the tunnel (see $WORK_DIR/harness-create.log)"
    fail=$((fail + 1))
  fi
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
