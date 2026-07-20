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
# Runs against the "homelab-host" remote's DEFAULT project. An earlier
# "homelab-dev" project got stuck with features.networks=true while this
# script was being built and could see no networks at all (#96); rather than
# repair it, #132 repointed the suite here and #131 deleted it. This script
# creates and tears down its
# own network/instances entirely within the default project, alongside
# node-boots-and-trusts-bootstrap-cert.sh's "home-lan" network, which it reuses read-only.
#
# Requires a real, bootable base IncusOS raw image — like #5, point
# INCUSOS_BASE_IMAGE at a local copy; the node-boot-dependent sections are
# skipped with a clear message if it's unset.
#
# KNOWN RED, deliberately: the last two checks (Incus reachable over the
# tunnel, and create-instance over it) fail against #157 — a node's seeded wg0
# gets a /32 address and no overlay route, so it completes a WireGuard
# handshake but cannot carry IP traffic. That is a real product bug this
# script is correctly reporting, not rot; their failure text prints node0's
# own view of the peer, which is what identifies it. Expect 39 passed,
# 2 failed until #157 lands, then 41/0. See docs/Decisions.md §23.
#
# ---------------------------------------------------------------------------
# Topology (see docs/Roadmap.md #91's plan for the full design rationale):
#
#   node0 ---- home-lan ---- [gateway] ---- wan-sim ---- webapp
#                              NAT/MASQUERADE +
#                              tuned-down conntrack
#                              UDP timeout
#
# Both subnets are read off the bridges at runtime rather than written here —
# home-lan's from `incus network get`, wan-sim's from $WAN_CIDR.
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
#   everything else. Section 4 asserts a UDP datagram actually crosses it and
#   lands with a NAT-translated conntrack entry, per run — that used to be a
#   one-time manual check at authoring time, which is what made #137 so hard
#   to diagnose (see section 2's comment).
# - node0 reaches the internet through home-lan's own ipv4.nat, and IncusOS
#   auto-updates itself on first boot. That is deliberate, not an oversight:
#   confining node0's egress with a network ACL was tried under #137 and
#   breaks the node outright — IncusOS fetches the Incus application from
#   images.linuxcontainers.org during first boot, so with egress blocked
#   Incus never starts and port 8443 never opens. The defence against a
#   moving upstream is therefore to make the assertions version-agnostic
#   (see the endpoint fallback in section 5), not to freeze the version.
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
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-incus.sh
. "$ROOT_DIR/scripts/validate/lib-incus.sh"

VALIDATE_PROVES="a node's WireGuard tunnel survives a NAT hop and carries provisioning (#91)"
VALIDATE_GROUP="incus-vm"
VALIDATE_NEEDS="incus go git python3 jq pinned-base-images [flasher-tool] [INCUSOS_BASE_IMAGE]"
VALIDATE_DURATION="~13m"

validate_parse_args "$@"
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
MAC="aa:bb:cc:dd:ee:02"
WAN_CIDR="10.200.0.0/24"
WAN_GATEWAY_ADDR="10.200.0.1"

# The LAN side is derived from the bridge itself, just after the prereq gate
# below — see the "LAN addressing" block there.
LAN_ADDR=""
LAN_GATEWAY_ADDR=""
LAN_CIDR=""
STATIC_IP=""

VM_NAME="validate-tunnel-node0-$$"
WRITER_NAME="validate-tunnel-writer-$$"
PROBE_NAME="validate-tunnel-probe-$$"
GATEWAY_NAME="validate-tunnel-gw-$$"
WEBAPP_NAME="validate-tunnel-webapp-$$"
HARNESS_NAME="validate-tunnel-harness-$$"
NAT_SENDER_NAME="validate-tunnel-natsend-$$"
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
  incus delete --force --project "$PROJECT" "$REMOTE:$NAT_SENDER_NAME" >/dev/null 2>&1
  incus storage volume delete "$REMOTE:$POOL" "$SEED_VOL" --project "$PROJECT" >/dev/null 2>&1
  incus network delete "$REMOTE:$WAN_NETWORK" --project "$PROJECT" >/dev/null 2>&1
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

echo "== 0. Hard prerequisites =="
# Preconditions, not assertions — this script's subject is the tunnel, and
# require_flasher_tool in particular is #136: build-image's dependency used to
# fail here as four cascading failures with no diagnostic.
require_cmd incus go git python3 jq
require_flasher_tool
require_incus_remote "$REMOTE"
require_incus_network "$REMOTE" "$PROJECT" "$LAN_NETWORK"
require_incus_image "$REMOTE" "$PROJECT" "$VALIDATE_ALPINE_CT"
require_incus_image "$REMOTE" "$PROJECT" "$VALIDATE_ALPINE_VM"
check_prereqs

# LAN addressing, derived from the bridge rather than hardcoded. #132 made the
# network *name* overridable but left its addressing written out literally in
# three places, so pointing this script at any bridge that isn't a
# 192.168.1.0/24 already rendered a fleet.yaml describing a network that didn't
# exist, and left the fleet.yaml and the rendered seed disagreeing about the
# network the node is actually on.
#
# Derived here rather than up with the other config so an unreachable remote
# still gets require_incus_remote's diagnostic instead of an empty variable.
# Assumes a /24, matching the assumption $WAN_GATEWAY_ADDR/24 already makes.
LAN_ADDR="$(incus network get "$REMOTE:$LAN_NETWORK" ipv4.address 2>/dev/null)"
if [ -z "$LAN_ADDR" ] || [ "$LAN_ADDR" = "none" ]; then
  echo "ERROR: network '$LAN_NETWORK' on $REMOTE has no ipv4.address; this script needs a managed IPv4 bridge" >&2
  exit 2
fi
LAN_GATEWAY_ADDR="${LAN_ADDR%/*}"
LAN_CIDR="${LAN_GATEWAY_ADDR%.*}.0/24"
STATIC_IP="${LAN_GATEWAY_ADDR%.*}.202"

echo
echo "== 1. Build the binaries =="
check "bootstrap CLI builds" go -C "$ROOT_DIR" build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
check "web app builds (CGO_ENABLED=0, matching the real Docker image)" \
  bash -c "cd '$ROOT_DIR' && CGO_ENABLED=0 go build -o '$WEB_BIN' ./cmd/web"
# CGO_ENABLED=0 for the same reason as the web app above, and it is not
# cosmetic: this binary is pushed into an Alpine container and executed there.
# A default cgo build requests /lib64/ld-linux-x86-64.so.2, which musl-based
# Alpine does not have, so `incus exec` reports the famously unhelpful
# "Error: Command not found" — for a file that exists and is executable.
# That is why both harness assertions had never passed (#137).
check "validate-tunnel-harness builds (CGO_ENABLED=0, it runs inside Alpine)" \
  bash -c "cd '$ROOT_DIR' && CGO_ENABLED=0 go build -o '$HARNESS_BIN' ./cmd/validate-tunnel-harness"

if [ ! -x "$BOOTSTRAP_BIN" ] || [ ! -x "$WEB_BIN" ] || [ ! -x "$HARNESS_BIN" ]; then
  echo "ERROR: a required binary didn't build; nothing downstream can be meaningful" >&2
  summary
fi

echo
echo "== 2. NAT-simulation topology: wan-sim network + gateway =="
# home-lan already exists (shared with node-boots-and-trusts-bootstrap-cert.sh); wan-sim is
# this run's own, deleted in cleanup. No ipv4.nat here — the gateway
# container does its own MASQUERADE (see this file's header for why).
check "network '$WAN_NETWORK' created" incus network create "$REMOTE:$WAN_NETWORK" \
  "ipv4.address=$WAN_GATEWAY_ADDR/24" "ipv4.nat=false" "ipv4.dhcp=true"

check "gateway container launched on $LAN_NETWORK" incus launch "$VALIDATE_ALPINE_CT" "$REMOTE:$GATEWAY_NAME" \
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
# bridges".
#
# These used to be one check that ran four setup commands and asserted they
# exited 0. That is the defect #137 was filed for: it could not distinguish a
# broken NAT simulation from a broken tunnel, because it never asserted NAT
# *translates* anything — both present as "handshake never observed". Now each
# setting is read back, and section 4 proves an actual packet traverses.
check "gateway iptables installed" \
  incus exec --project "$PROJECT" "$REMOTE:$GATEWAY_NAME" -- apk add --no-cache iptables
check_eq "gateway forwards IPv4" "1" \
  "$(incus exec --project "$PROJECT" "$REMOTE:$GATEWAY_NAME" -- \
    sh -c "sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1; sysctl -n net.ipv4.ip_forward" 2>/dev/null)"
# Its own check because sysctl -w on a netfilter key can fail in a container
# depending on namespace ownership, and the old chained bash -c hid which of
# the four commands failed.
check_eq "gateway conntrack UDP timeout tuned below the 25s keepalive" "15" \
  "$(incus exec --project "$PROJECT" "$REMOTE:$GATEWAY_NAME" -- \
    sh -c "sysctl -w net.netfilter.nf_conntrack_udp_timeout=15 >/dev/null 2>&1; sysctl -n net.netfilter.nf_conntrack_udp_timeout" 2>/dev/null)"
incus exec --project "$PROJECT" "$REMOTE:$GATEWAY_NAME" -- \
  iptables -t nat -A POSTROUTING -o eth1 -j MASQUERADE >/dev/null 2>&1
check "gateway MASQUERADEs out of eth1" bash -c "
  incus exec --project '$PROJECT' '$REMOTE:$GATEWAY_NAME' -- iptables -t nat -S POSTROUTING |
    grep -qF -- '-A POSTROUTING -o eth1 -j MASQUERADE'
"

echo
echo "== 3. Build the pipeline: gen-cert -> render-seed =="
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$WORK_DIR/cert"

cat >"$WORK_DIR/fleet.yaml" <<EOF
kind: Network
name: $LAN_NETWORK
cidr: $LAN_CIDR
gateway: $LAN_GATEWAY_ADDR
dns: [$LAN_GATEWAY_ADDR]
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

check "webapp container launched on $WAN_NETWORK" incus launch "$VALIDATE_ALPINE_CT" "$REMOTE:$WEBAPP_NAME" \
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

# Prove the NAT simulation actually carries UDP across the gateway, before
# anything depends on it. Section 2 asserts the gateway is *configured*; this
# asserts it *works*, which is the distinction #137 was filed over — a
# silently-broken NAT simulation and a broken WireGuard tunnel both present as
# "handshake never observed", and telling them apart previously meant
# preserving the topology and inspecting it by hand.
#
# Runs here because the sender must sit behind the gateway on home-lan and the
# receiver on wan-sim, which is only true once the webapp container exists —
# and before /web starts, so nothing is competing for the port. A port other
# than $WG_PORT for the same reason.
#
# busybox nc on both ends deliberately: wan-sim is ipv4.nat=false and nothing
# MASQUERADEs for it, so the webapp container has no route off its bridge —
# verified, not assumed. Anything needing `apk add` would hang on mirror
# timeouts rather than fail cleanly.
NAT_PROBE_PORT=51821
NAT_NONCE="natprobe-$$"
check "NAT probe sender launched behind the gateway" bash -c "
  incus launch '$VALIDATE_ALPINE_CT' '$REMOTE:$NAT_SENDER_NAME' --project '$PROJECT' --network '$LAN_NETWORK' &&
  for _ in \$(seq 1 20); do
    incus exec --project '$PROJECT' '$REMOTE:$NAT_SENDER_NAME' -- \
      sh -c 'ip -4 -o addr show eth0 | grep -q inet' && break
    sleep 1
  done &&
  incus exec --project '$PROJECT' '$REMOTE:$NAT_SENDER_NAME' -- \
    ip route replace default via '$GATEWAY_LAN_IP'
"
# Listener first, in the background, then the datagram. UDP is fire-and-forget:
# if nc isn't bound yet the packet is silently dropped and the check fails for
# the wrong reason, hence the settle.
incus_exec_bg "$WORK_DIR/natprobe-listen.log" "$REMOTE:$WEBAPP_NAME" -- \
  sh -c "nc -u -l -p $NAT_PROBE_PORT >/tmp/natprobe.out"
sleep 2
incus exec --project "$PROJECT" "$REMOTE:$NAT_SENDER_NAME" -- \
  sh -c "echo $NAT_NONCE | nc -u -w2 $WEBAPP_WAN_IP $NAT_PROBE_PORT" >/dev/null 2>&1
sleep 2
check "a UDP datagram traverses the gateway to wan-sim" bash -c "
  incus exec --project '$PROJECT' '$REMOTE:$WEBAPP_NAME' -- \
    grep -qF '$NAT_NONCE' /tmp/natprobe.out
"
# Delivery alone would also pass if the gateway merely *routed* between two
# bridges. What makes it NAT is the reply-direction destination being the
# gateway's own wan-sim address rather than the sender's home-lan one — i.e.
# the source really was rewritten on the way out.
check "the gateway NAT-translated it (conntrack reply dst is the gateway's wan-sim address)" bash -c "
  incus exec --project '$PROJECT' '$REMOTE:$GATEWAY_NAME' -- \
    grep -F 'dport=$NAT_PROBE_PORT' /proc/net/nf_conntrack |
    grep -qF 'dst=$GATEWAY_WAN_IP'
"
incus delete --force --project "$PROJECT" "$REMOTE:$NAT_SENDER_NAME" >/dev/null 2>&1

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
  record_pass "web app synced and assigned node0 tunnel_ip $NODE_TUNNEL_IP"
else
  record_fail "web app synced and assigned node0 a tunnel_ip (webapp.log: $(tail -5 "$WORK_DIR/webapp.log" 2>/dev/null))"
fi

echo
echo "== 5. Boot node0 with the real WireGuard-enabled seed and confirm the tunnel =="
# Every check the else-branch below records, in order and worded identically.
# They have to match: skip_check and record_pass/record_fail both feed the same
# tally, so a skip label that doesn't correspond to a real check makes the
# summary describe a run that never happened — and a check with no skip entry
# silently shrinks the total when the branch is skipped.
_vm_checks=(
  "fetched WireGuard-enabled seed from the real web app"
  "all four seed files extracted from the web app's response"
  "web app's WireGuard public key present in node0's seed"
  "harness identity generated"
  "wan-sim route + harness test peer appended to network.yaml"
  "build-image exits 0"
  "seed .img streamed onto $REMOTE as a block volume"
  "VM created with empty disk + seeded image as install media"
  "VM installs IncusOS from the seeded image"
  "probe instance ready on $LAN_NETWORK"
  "node0's WireGuard tunnel to the webapp shows a completed handshake"
  "node0's own wg0 public key and listening port read from the node"
  "harness container launched on $LAN_NETWORK"
  "harness binary + key pushed"
  "node0's Incus API reachable through a real WireGuard tunnel (10.100.0.0/24), not just the LAN"
  "webapp store pulled and node0's real bootstrap credential extracted"
  "bootstrap cert/key pushed to harness"
  "temp-cert create-instance + revoke mechanism succeeds over the tunnel"
)
if ! have_env_file INCUSOS_BASE_IMAGE; then
  # Operator-supplied image absent: says nothing about the tunnel.
  for _desc in "${_vm_checks[@]}"; do
    skip_check "$_desc" base-image \
      "INCUSOS_BASE_IMAGE ${INCUSOS_BASE_IMAGE:+=$INCUSOS_BASE_IMAGE }not usable"
  done
elif [ -z "$NODE_TUNNEL_IP" ]; then
  # Distinct from the case above: this isn't a missing prerequisite, it's a
  # cascade from step 4's already-recorded failure. Tagged separately so
  # --strict promotes it (the run is red regardless) and so the output doesn't
  # imply the operator forgot to supply something.
  for _desc in "${_vm_checks[@]}"; do
    skip_check "$_desc" upstream "web app never assigned node0 a tunnel_ip, see step 4"
  done
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

  # Read from the rendered YAML, not the JSON envelope. /instances/{name}/seed
  # returns compact JSON whose *values* are YAML documents, so the web app's
  # key appears as `public_key: <base64>` (YAML), never as `"public_key": "..."`
  # (JSON). The old JSON-shaped grep therefore matched nothing on every run
  # this script has ever had, leaving APP_PUBLIC_KEY empty — and nothing
  # noticed, because the handshake check below only ever grepped for the
  # *string* and an empty key made its pattern vacuous. That is the third
  # distinct reason #137's three assertions failed.
  #
  # Taken before patch-seed appends the harness's test peer, so the first
  # public_key in the file is unambiguously the web app's.
  APP_PUBLIC_KEY=$(awk '/^ *public_key:/ {print $2; exit}' "$WORK_DIR/seed/network.yaml")
  check "web app's WireGuard public key present in node0's seed" bash -c "[ -n '$APP_PUBLIC_KEY' ]"

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
    incus launch '$VALIDATE_ALPINE_VM' '$REMOTE:$WRITER_NAME' --vm --project '$PROJECT' \
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
    record_pass "VM installs IncusOS from the seeded image"
    incus stop --project "$PROJECT" "$REMOTE:$VM_NAME" --force >/dev/null 2>&1
    incus config device remove --project "$PROJECT" "$REMOTE:$VM_NAME" install-media >/dev/null 2>&1
    incus start --project "$PROJECT" "$REMOTE:$VM_NAME" >/dev/null 2>&1
  else
    record_fail "VM installs IncusOS from the seeded image"
  fi

  echo "Waiting up to 3 minutes for node0's WireGuard tunnel to the webapp to come up (checked via node0's own IncusOS network state) ..."
  # LAN-side probe, exactly like #5, for querying node0's own IncusOS
  # network state — this is a different check than "Incus reachable over
  # the tunnel" below: it confirms the HANDSHAKE happened at all, from the
  # LAN side which we already know is reachable.
  #
  # The probe only fetches; parsing happens host-side with jq. The old check
  # grepped the whole response body for two strings independently, which is a
  # #129-class false pass: the web app's public key appears in the *config*
  # stanza whether or not any traffic ever flowed, and `latest_handshake`
  # appears if ANY peer has handshaked — including the harness's own test peer.
  # Both could match with the webapp tunnel completely dead.
  check "probe instance ready on $LAN_NETWORK" bash -c "
    set -eu
    incus launch '$VALIDATE_ALPINE_CT' '$REMOTE:$PROBE_NAME' --project '$PROJECT' --network '$LAN_NETWORK' --storage '$POOL' &&
    for _ in \$(seq 1 15); do
      incus exec --project '$PROJECT' '$REMOTE:$PROBE_NAME' -- apk add --no-cache curl >/dev/null 2>&1 && break
      sleep 2
    done &&
    incus exec --project '$PROJECT' '$REMOTE:$PROBE_NAME' -- sh -c 'command -v curl' &&
    incus file push '$WORK_DIR/cert/client.crt' '$REMOTE:$PROBE_NAME/root/client.crt' --project '$PROJECT' &&
    incus file push '$WORK_DIR/cert/client.key' '$REMOTE:$PROBE_NAME/root/client.key' --project '$PROJECT'
  "

  # IncusOS moved this endpoint from /1.0/... to /os/1.0/... — 202606201302
  # serves the former, 202607060039 the latter. The script only ever asked for
  # the old one, so on a node that had auto-updated it got a 404 and reported
  # "handshake never observed", which is how #137 presented. Ask for both and
  # use whichever answers, so the assertion tracks the tunnel rather than
  # whichever IncusOS release the operator's base image happens to be.
  node_net_state() {
    local path
    for path in /os/1.0/system/network /1.0/system/network; do
      local body
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

  # The peer is matched by public key and then asked for its own
  # latest_handshake. IncusOS omits that field entirely until a handshake
  # actually completes — verified live against both peers on a running node,
  # where the webapp peer had it and the never-contacted harness peer did not.
  handshake_seen=false
  last_peer=""
  for _ in $(seq 1 36); do
    net_state=$(node_net_state)
    last_peer=$(jq -c --arg k "$APP_PUBLIC_KEY" \
      '.metadata.state.interfaces.wg0.wireguard.peers[]? | select(.public_key == $k)' \
      <<<"$net_state" 2>/dev/null)
    if [ -n "$last_peer" ] && jq -e '(.latest_handshake // "") != ""' <<<"$last_peer" >/dev/null 2>&1; then
      handshake_seen=true
      break
    fi
    sleep 5
  done
  if "$handshake_seen"; then
    record_pass "node0's WireGuard tunnel to the webapp shows a completed handshake"
  else
    # Report what was actually seen. #137 took a preserved live topology and
    # manual inspection to diagnose; the peer stanza and the tail of the
    # console are what that inspection turned out to need.
    record_fail "node0's WireGuard tunnel to the webapp shows a completed handshake" \
      "no handshake within 3 minutes; webapp peer stanza: ${last_peer:-<peer $APP_PUBLIC_KEY absent from wg0>}; console tail: $(console_log | tail -20 | tr '\n' '|')"
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
  # node0's *own* wg0 identity, read off the node rather than assumed. Both
  # values were previously missing, and between them are why these two checks
  # had never passed since #91 (see docs/Decisions.md §23):
  #
  #  - the public key: the harness was handed $APP_PUBLIC_KEY, which is the
  #    *web app's* key. The harness's peer is node0, whose wg0 key is a
  #    different one entirely, so it was trusting the wrong identity.
  #  - the listening port: node0's seed sets no `port` for wg0, so IncusOS
  #    binds a random one. Nothing can initiate to node0 without reading it.
  _wg=$(node_net_state | jq -r '.metadata.state.interfaces.wg0.wireguard // empty' 2>/dev/null)
  NODE_WG_PUBKEY=$(jq -r '.public_key // empty' <<<"$_wg" 2>/dev/null)
  NODE_WG_PORT=$(jq -r '.listening_port // empty' <<<"$_wg" 2>/dev/null)
  check "node0's own wg0 public key and listening port read from the node" bash -c \
    "[ -n '$NODE_WG_PUBKEY' ] && [ -n '$NODE_WG_PORT' ]"

  # The DHCP wait matters: the harness dials node0 the moment it starts, and
  # an interface without a lease yet has no route to home-lan, which surfaces
  # as `sendmmsg: network is unreachable` from inside WireGuard's own
  # handshake — a message that reads like a tunnel fault rather than a
  # container that isn't up yet. Every other container here already waits;
  # this one didn't, and nothing noticed while the binary couldn't execute.
  check "harness container launched on $LAN_NETWORK" bash -c "
    incus launch '$VALIDATE_ALPINE_CT' '$REMOTE:$HARNESS_NAME' --project '$PROJECT' --network '$LAN_NETWORK' &&
    for _ in \$(seq 1 20); do
      incus exec --project '$PROJECT' '$REMOTE:$HARNESS_NAME' -- \
        sh -c 'ip -4 -o addr show eth0 | grep -q inet' && break
      sleep 1
    done &&
    incus exec --project '$PROJECT' '$REMOTE:$HARNESS_NAME' -- \
      sh -c 'ip -4 -o addr show eth0 | grep -q inet'
  "
  check "harness binary + key pushed" bash -c "
    incus file push --project '$PROJECT' '$HARNESS_BIN' '$REMOTE:$HARNESS_NAME/harness' --mode=0755 &&
    incus file push --project '$PROJECT' '$WORK_DIR/harness.key' '$REMOTE:$HARNESS_NAME/harness.key'
  "

  echo "Running the harness's probe mode against node0 ($NODE_TUNNEL_IP:8443) over its own tunnel ..."
  if incus exec --project "$PROJECT" "$REMOTE:$HARNESS_NAME" -- \
    /harness -mode=probe \
      -private-key-file=/harness.key \
      -local-addr=10.100.0.254 \
      -peer-public-key="$NODE_WG_PUBKEY" \
      -peer-tunnel-ip="$NODE_TUNNEL_IP" \
      -peer-endpoint="$STATIC_IP:$NODE_WG_PORT" \
      -node-addr="$NODE_TUNNEL_IP:8443" \
      -timeout=60s >"$WORK_DIR/harness-probe.log" 2>&1; then
    record_pass "node0's Incus API reachable through a real WireGuard tunnel (10.100.0.0/24), not just the LAN"
  else
    # node0's view of the harness peer separates "the handshake never
    # happened" from "it handshaked but no IP traffic came back", which are
    # different faults with the same symptom at the HTTP layer.
    record_fail "node0's Incus API reachable through a real WireGuard tunnel (10.100.0.0/24), not just the LAN" \
      "$(tail -5 "$WORK_DIR/harness-probe.log" 2>/dev/null | tr '\n' '|') node0's view of the harness peer: $(node_net_state | jq -c --arg k "$HARNESS_PUBLIC_KEY" '.metadata.state.interfaces.wg0.wireguard.peers[]? | select(.public_key == $k)' 2>/dev/null)"
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
      -peer-public-key="$NODE_WG_PUBKEY" \
      -peer-tunnel-ip="$NODE_TUNNEL_IP" \
      -peer-endpoint="$STATIC_IP:$NODE_WG_PORT" \
      -node-addr="$NODE_TUNNEL_IP:8443" \
      -bootstrap-cert=/root/bootstrap.crt \
      -bootstrap-key=/root/bootstrap.key \
      -timeout=60s >"$WORK_DIR/harness-create.log" 2>&1; then
    record_pass "temp-cert create-instance + revoke mechanism succeeds over the tunnel"
  else
    record_fail "temp-cert create-instance + revoke mechanism succeeds over the tunnel" \
      "$(tail -5 "$WORK_DIR/harness-create.log" 2>/dev/null | tr '\n' '|')"
  fi
fi

echo
summary
