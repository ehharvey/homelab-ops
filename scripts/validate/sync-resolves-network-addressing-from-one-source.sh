#! /bin/bash
# Validates GH issue #199 (docs/Decisions.md §28): each Network's addressing
# (cidr, gateway, dhcp_excluded_range, dns) comes from exactly one source, git
# (a private config repo) or the web app's state (the addressing file named by
# NETWORK_ADDRESSING_PATH), and the sync's resolve step joins the two before
# validation, IPAM and the seed route, against the real compose stack:
#
#   1. A fleet mixing both modes syncs, IPAM assigns on both networks, and an
#      instance on each renders a seed with its own network's addressing. The
#      state-addressed network's load is reported in the log, POST /sync's
#      warnings and GET /status's sync_warnings.
#   2. The same instance renders a byte-identical seed once its network's
#      identical addressing moves from state into git, and GET /status drops
#      the network's warning, since its addressing no longer comes from state.
#   3. Each of these is rejected, naming the network: addressed in both
#      sources, a state entry for a network git doesn't declare, and a
#      state-addressed network with no addressing at all, with 409, since the
#      fix may be in the web app's settings; partial addressing in git and a
#      git static_ip on a state-addressed network with 502, since the commit
#      is wrong on its own.
#   4. With CONFIG_REPO_PRIVATE unset (the shipped default), git addressing is
#      rejected at sync (409), and a public-repo fleet (names only) still syncs.
#
# The git-addressed regression (today's fleets syncing unchanged with
# CONFIG_REPO_PRIVATE=true) is seed-route-renders-static-ip.sh and
# seed-route-renders-ipam-address.sh.
#
# Like seed-route-renders-ipam-address.sh, it pushes its own fleets into the
# config-repo git server at runtime rather than editing dev/git-fixture.
#
# Intended to run INSIDE the devcontainer, from the repo root. Requires docker
# compose, jq, and go.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-compose.sh
. "$ROOT_DIR/scripts/validate/lib-compose.sh"

VALIDATE_PROVES="a sync takes each network's addressing from git or the addressing file, never both (#199)"
VALIDATE_GROUP="compose"
VALIDATE_NEEDS="docker-compose jq go"
VALIDATE_DURATION="~40s"
VALIDATE_SUPPORTED_OS="any"

validate_parse_args "$@"
require_compose
check_prereqs
cd "$ROOT_DIR" || exit 1

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
CERT_DIR="$WORK_DIR/cert"
ADDR_DIR="$WORK_DIR/addressing"
ADDR_FILE="$ADDR_DIR/networks.yaml"
mkdir -p "$ADDR_DIR"
# The override interpolates these. VALIDATE_REPO_PRIVATE starts as the dev
# stack's "true"; step 6 blanks it and recreates web to get the default.
export CERT_DIR ADDR_DIR
export VALIDATE_REPO_PRIVATE=true
OVERRIDE="$(mktemp /tmp/compose-addressing-override.XXXXXX.yml)"
VALIDATE_COMPOSE_OVERRIDE="$OVERRIDE"

# The seed route needs a CertSource and a WireGuard endpoint, as in
# seed-route-renders-ipam-address.sh (a loopback endpoint satisfies it). The
# addressing directory is mounted, not the file, so rewriting the file on the
# host shows up in the container on the next sync.
cat >"$OVERRIDE" <<'EOF'
services:
  web:
    volumes:
      - ${CERT_DIR}:/cert:ro
      - ${ADDR_DIR}:/addressing:ro
    environment:
      - CLIENT_CERT_PATH=/cert/client.crt
      - WIREGUARD_ENDPOINT=127.0.0.1:51820
      - NETWORK_ADDRESSING_PATH=/addressing/networks.yaml
      - CONFIG_REPO_PRIVATE=${VALIDATE_REPO_PRIVATE}
EOF

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  compose_down
  rm -rf "$WORK_DIR"
  rm -f "$OVERRIDE"
}
trap cleanup EXIT

base_url="http://localhost:8080"

# State addressing for state-lan, and the same block as git YAML.
STATE_LAN_ADDRESSING='cidr: 10.0.2.0/24
gateway: 10.0.2.1
dhcp_excluded_range: 10.0.2.200-10.0.2.250
dns: [10.0.2.1]'
GIT_LAN="kind: Network
name: git-lan
cidr: 10.0.1.0/24
gateway: 10.0.1.1
dhcp_excluded_range: 10.0.1.200-10.0.1.250
dns: [10.0.1.1]"

# instance <name> <mac> <network> [extra line] — an Instance document.
instance() {
  printf -- '---\nkind: Instance\nname: %s\nmac: %s\nnetwork: %s\ndisk: single\nnic: single\napplications: [incus]\n%s\n' \
    "$1" "$2" "$3" "${4:-}"
}

# fleet <state-lan body> [statenode extra line] — git-lan (git-addressed),
# state-lan with the given body after its name, and one instance on each.
fleet() {
  printf '%s\n---\nkind: Network\nname: state-lan\n%s\n' "$GIT_LAN" "$1"
  instance gitnode aa:bb:cc:dd:ee:01 git-lan
  instance statenode aa:bb:cc:dd:ee:02 state-lan "${2:-}"
}

# addressing <network>... — write the addressing file with an entry per name:
# state-lan's addressing, or a ghost network's.
addressing() {
  : >"$ADDR_FILE"
  local n
  for n in "$@"; do
    case "$n" in
    state-lan) printf -- '---\nkind: Network\nname: state-lan\n%s\n' "$STATE_LAN_ADDRESSING" >>"$ADDR_FILE" ;;
    git-lan) printf -- '---\nkind: Network\nname: git-lan\ncidr: 10.0.1.0/24\n' >>"$ADDR_FILE" ;;
    ghost-lan) printf -- '---\nkind: Network\nname: ghost-lan\ncidr: 10.0.9.0/24\n' >>"$ADDR_FILE" ;;
    esac
  done
}

# sync_rejected <status> <desc> <log-fragment> — POST /sync must fail with
# <status> (409: git doesn't resolve against the web app's addressing; 502:
# the commit's addressing is wrong on its own) and log the rejection naming
# the network.
sync_rejected() {
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$base_url/sync")
  check_eq "$2: POST /sync is rejected with $1" "$1" "$code"
  check_log "$2: the log names the network" "$3"
}

seed() { curl -s -X POST "$base_url/instances/$1/seed"; }

echo "== 1. Prerequisites: build bootstrap, generate the operator's cert =="
check "build bootstrap binary" go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap binary not built or not executable: $BOOTSTRAP_BIN" >&2
  exit 2
fi
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name "sync-resolves-network-addressing"
addressing state-lan

echo
echo "== 2. Bring up the stack with the addressing file mounted =="
check "docker compose up --build succeeds" compose up --build -d
wait_web_ready "$base_url"

echo
echo "== 3. A mixed fleet syncs, assigns and renders =="
push_fleet "sync-resolves-addressing" "$(fleet '')"
sync_resp=$(curl -s -X POST "$base_url/sync")
check_json "POST /sync syncs both networks and instances" "$sync_resp" '.networks == 2 and .instances == 2'
check_json "POST /sync warns that state-lan's addressing was loaded" "$sync_resp" \
  '.warnings | any(startswith("network \"state-lan\": addressing loaded from") and contains("cidr 10.0.2.0/24"))'
check_log "the log reports state-lan's addressing load" 'addressing: network "state-lan": addressing loaded'
status=$(curl -s "$base_url/status")
check_json "GET /status carries the warning in sync_warnings" "$status" '.sync_warnings | any(contains("state-lan"))'
networks=$(curl -s "$base_url/networks")
check_json "GET /networks serves state-lan with its state addressing" "$networks" \
  'map(select(.Name == "state-lan"))[0] | .CIDR == "10.0.2.0/24" and .Gateway == "10.0.2.1"'

git_seed=$(seed gitnode)
check_json "gitnode's seed has its IPAM address and gateway on git-lan" "$git_seed" \
  '.network_yaml | contains("10.0.1.200/24") and contains("10.0.1.1")'
state_seed=$(seed statenode)
check_json "statenode's seed has its IPAM address and gateway on state-lan" "$state_seed" \
  '.network_yaml | contains("10.0.2.200/24") and contains("10.0.2.1")'

echo
echo "== 4. The same addressing from git renders a byte-identical seed =="
addressing # empty: state-lan is now addressed in git
push_fleet "sync-resolves-addressing" "$(fleet "$STATE_LAN_ADDRESSING")"
sync_resp=$(curl -s -X POST "$base_url/sync")
check_json "the all-git fleet syncs" "$sync_resp" '.networks == 2 and .instances == 2'
check_json "moving identical addressing into git reports no change" "$sync_resp" '.warnings == null'
check_eq "statenode's seed is byte-identical in both modes" "$state_seed" "$(seed statenode)"
check_json "GET /status drops state-lan's warning once it's addressed in git" "$(curl -s "$base_url/status")" \
  '(.sync_warnings // []) | any(contains("state-lan")) | not'
good_commit=$(curl -s "$base_url/status" | jq -r .commit)

echo
echo "== 5. Each conflicting or incomplete case is rejected, naming the network =="
# state-lan in git and in the file.
addressing state-lan
sync_rejected 409 "addressed in both git and state" 'network "state-lan" is addressed both in git and in the addressing file'

push_fleet "sync-resolves-addressing" "$(fleet '')"
addressing state-lan ghost-lan
sync_rejected 409 "a state entry for an undeclared network" 'network "ghost-lan" is not declared in git'

addressing state-lan
push_fleet "sync-resolves-addressing" "$(fleet 'gateway: 10.0.2.1')"
sync_rejected 502 "partial addressing in git" 'network "state-lan" sets gateway but no cidr'

push_fleet "sync-resolves-addressing" "$(fleet '' 'static_ip: 10.0.2.201')"
sync_rejected 502 "a git static_ip on a state-addressed network" 'network "state-lan" takes its addressing from the web app'

push_fleet "sync-resolves-addressing" "$(fleet '')"
addressing
sync_rejected 409 "a state-addressed network with no addressing" 'network "state-lan" has no addressing'

check_eq "rejected syncs leave the last good commit in place" "$good_commit" "$(curl -s "$base_url/status" | jq -r .commit)"

echo
echo "== 6. Without CONFIG_REPO_PRIVATE, git addressing is rejected =="
export VALIDATE_REPO_PRIVATE=
check "recreate web without CONFIG_REPO_PRIVATE" compose up -d web
wait_web_ready "$base_url"
addressing state-lan
sync_rejected 409 "git addressing with CONFIG_REPO_PRIVATE unset" 'network "git-lan" is addressed in git, but CONFIG_REPO_PRIVATE is not "true"'

# A public repo: both networks name-only, both addressed from the file.
printf -- '---\nkind: Network\nname: git-lan\ncidr: 10.0.1.0/24\ngateway: 10.0.1.1\ndhcp_excluded_range: 10.0.1.200-10.0.1.250\n' >>"$ADDR_FILE"
public_fleet="kind: Network
name: git-lan
---
kind: Network
name: state-lan
$(instance gitnode aa:bb:cc:dd:ee:01 git-lan)
$(instance statenode aa:bb:cc:dd:ee:02 state-lan)"
push_fleet "sync-resolves-addressing" "$public_fleet"
sync_resp=$(curl -s -X POST "$base_url/sync")
check_json "a names-only fleet syncs without CONFIG_REPO_PRIVATE" "$sync_resp" '.networks == 2 and .instances == 2'
check_json "gitnode's seed renders from state addressing" "$(seed gitnode)" '.network_yaml | contains("10.0.1.200/24")'

summary
