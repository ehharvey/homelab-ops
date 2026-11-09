#! /bin/bash
# Validates GH issue #243 (docs/Deployment.md § The appliance,
# docs/Decisions.md §29): the web app appliance boots from the generic NixOS
# image and a seed from `bootstrap render-appliance-seed`, in a real Incus VM,
# and serves.
#
#   1. `make appliance-image` builds the raw-efi image from
#      deploy/nixos/flake.lock, which plays the pinned base-image alias's role
#      (docs/Decisions.md §21): the image changes only when the lock does.
#   2. Two copies of examples/webapp/ are filled in as
#      web-starts-from-deployment-config-directory.sh fills one: an operator
#      peer, client.crt, a generated secrets/wireguard.key (0700/0600), and
#      CONFIG_REPO_URL pointed at a fixture repo served from a probe container
#      on home-lan. Each is rendered into a seed: the first with its secrets,
#      --image (the digest deploy/compose.yaml pins) and --reboot-time; the
#      second with --no-secrets and --ssh-authorized-keys.
#   3. First VM, booted from the image with seed 1: cloud-init reaches
#      `status: done`, and its cached user-data is shredded (the WireGuard
#      key is nowhere under /var/lib/cloud); the seed's settings are applied;
#      the compose service runs from the pinned digest; only the WireGuard
#      UDP port is open; nixos-upgrade is enabled with allowReboot and the
#      rendered reboot window. An operator WireGuard peer reaches the API
#      with `bootstrap api` (#196) and a sync succeeds. nixos-upgrade.service
#      then runs for real, with the box's own Nix: it re-locks nixpkgs to the
#      release branch's head and builds that system. After a reboot the box
#      runs the upgraded generation, cloud-init stays off, the service comes
#      back on its own, and a sync succeeds again. Once #197 lands, every
#      reboot seals the store, so this step will need an `unlock` before the
#      sync.
#   4. Second VM, with seed 2 (--no-secrets): the web service waits while
#      secrets/ is absent; root logs in with the seed's SSH key; secrets/ is
#      pushed in over scp, as docs/Deployment.md's runbook says; the service
#      starts from the pinned digest, and a sync over the tunnel succeeds.
#
# Getting the bytes onto homelab-host. A `disk source=<path>` device resolves
# on the server, not here, so, as in node-boots-and-trusts-bootstrap-cert.sh,
# the image and the seeds are streamed through a disposable Alpine "writer"
# VM into block-type custom volumes:
#   - the raw-efi image is dd'd onto one, which becomes the appliance's boot
#     disk (boot.priority=10). The VM's own root, from `incus init --empty`,
#     stays small and unused. The second VM boots from an `incus storage
#     volume copy` of it, made before either VM starts;
#   - each seed goes onto a small one that the writer formats FAT, labelled
#     CIDATA, holding user-data and meta-data: the laptop's seed stick.
#     (Incus's own cloud-init:config drive would carry the user-data, but
#     with Incus's meta-data rather than the rendered one.)
# Everything on home-lan is reached from the probe container: the bridge is
# routable only from the Incus host and its instances.
#
# The image includes the incus-agent loader (appliance.nix), which is how
# `incus exec` reaches the appliance here.
#
# Needs network on home-lan: the appliance pulls the published web image from
# GHCR, its first boot fetches the few packages its settings change from
# cache.nixos.org, and the upgrade run fetches nixpkgs' branch head from
# GitHub and the upgraded system from cache.nixos.org. Shares home-lan with the other incus-vm scripts, so don't
# run it beside them.
#
# Intended to run INSIDE the devcontainer, from the repo root. Override the
# image with VALIDATE_APPLIANCE_IMAGE=<path> (skips the build) and the web
# image with VALIDATE_WEB_IMAGE=<repo>@sha256:<hex>.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"
# shellcheck source=scripts/validate/lib-incus.sh
. "$ROOT_DIR/scripts/validate/lib-incus.sh"

VALIDATE_PROVES="the generic NixOS appliance image boots with a render-appliance-seed seed, shreds cloud-init's cached copy of the key, runs the web app from the pinned digest with only WireGuard UDP open and autoUpgrade's reboot window, runs nixos-upgrade for real and boots its generation, serves a sync over the tunnel across a reboot, and waits for secrets/ under --no-secrets (#243)"
VALIDATE_GROUP="incus-vm"
VALIDATE_NEEDS="incus go jq docker pinned-base-images network-on-home-lan"
VALIDATE_DURATION="~45m (a TCG image build adds ~10m on a first run)"
# The appliance's host isn't a node OS backend: it is the same whatever the
# nodes run.
VALIDATE_SUPPORTED_OS="any"

validate_parse_args "$@"
cd "$ROOT_DIR" || exit 1

REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
PROJECT="${VALIDATE_INCUS_PROJECT:-default}"
NETWORK="${VALIDATE_INCUS_NETWORK:-home-lan}"
POOL="${VALIDATE_INCUS_POOL:-default}"

# Soft prerequisites, a SKIP (exit 3): no Incus remote to boot on, or no
# Docker to build the image with.
if ! incus info "$REMOTE:" >/dev/null 2>&1; then
  skip_check "the appliance boots from the image and a seed" incus-remote "incus remote '$REMOTE' not reachable"
  summary
fi
if [ -z "${VALIDATE_APPLIANCE_IMAGE:-}" ] && ! docker info >/dev/null 2>&1; then
  skip_check "the appliance boots from the image and a seed" docker "no Docker to build the image with (make appliance-image); or set VALIDATE_APPLIANCE_IMAGE"
  summary
fi
require_cmd go jq
require_incus_project "$REMOTE" "$PROJECT"
require_incus_network "$REMOTE" "$PROJECT" "$NETWORK"
require_incus_image "$REMOTE" "$PROJECT" "$VALIDATE_ALPINE_VM"
require_incus_image "$REMOTE" "$PROJECT" "$VALIDATE_ALPINE_CT"
check_prereqs

WORK_DIR="$(mktemp -d)"
BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
HARNESS="$WORK_DIR/validate-tunnel-harness"
CERT_DIR="$WORK_DIR/cert"
OPERATOR_KEY="$WORK_DIR/operator.key"
OPERATOR_ADDR=10.100.0.240
REBOOT_TIME_1=03:17
# Crosses midnight: the window's end wraps to 00:30.
REBOOT_TIME_2=23:30
IP_1=192.168.1.205
IP_2=192.168.1.206
TAG="validate-appliance-$$"
VM_1="$TAG-1"
VM_2="$TAG-2"
WRITER="$TAG-writer"
PROBE="$TAG-probe"
IMG_VOL_1="$TAG-disk-1"
IMG_VOL_2="$TAG-disk-2"
SEED_VOL_1="$TAG-seed-1"
SEED_VOL_2="$TAG-seed-2"
CONTAINER=homelab-ops-web-1
WEB_IMAGE="${VALIDATE_WEB_IMAGE:-$(sed -nE 's/^    image: (.*)$/\1/p' deploy/compose.yaml | head -1)}"

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  local i
  for i in "$VM_1" "$VM_2" "$WRITER" "$PROBE"; do
    incus delete --force --project "$PROJECT" "$REMOTE:$i" >/dev/null 2>&1
  done
  for i in "$IMG_VOL_1" "$IMG_VOL_2" "$SEED_VOL_1" "$SEED_VOL_2"; do
    incus storage volume delete "$REMOTE:$POOL" "$i" --project "$PROJECT" >/dev/null 2>&1
  done
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# on <instance> <cmd...> — run in an instance.
on() {
  local i="$1"
  shift
  # NixOS keeps its commands under /run/current-system/sw, which incus exec's
  # default PATH lacks; Alpine's are in the usual places.
  incus exec --project "$PROJECT" --env PATH=/run/wrappers/bin:/run/current-system/sw/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    "$REMOTE:$i" -- "$@"
}

probe() { on "$PROBE" "$@"; }

# timer_at <vm> <HH:MM> — nixos-upgrade.timer fires daily at HH:MM.
# shellcheck disable=SC2329 # run through check
timer_at() {
  on "$1" systemctl cat nixos-upgrade.timer | grep -qx "OnCalendar=$2"
}

# inactive <vm> <unit> — systemd answers, and the unit isn't active.
# shellcheck disable=SC2329 # run through check
inactive() {
  local state
  state=$(on "$1" systemctl is-active "$2")
  [ -n "$state" ] && [ "$state" != active ]
}

# web_waits <vm> — the web service hasn't started, and no container exists.
# shellcheck disable=SC2329 # run through check
web_waits() {
  local ids
  inactive "$1" homelab-ops-web.service && ids=$(on "$1" docker ps -aq) && [ -z "$ids" ]
}

# wait_for <seconds> <cmd...> — retry cmd every 5s until it succeeds.
wait_for() {
  local timeout_s="$1" waited=0
  shift
  while [ "$waited" -lt "$timeout_s" ]; do
    "$@" >/dev/null 2>&1 && return 0
    sleep 5
    waited=$((waited + 5))
  done
  return 1
}

# check_wait <desc> <seconds> <cmd...>
check_wait() {
  local desc="$1"
  shift
  if wait_for "$@"; then
    record_pass "$desc"
  else
    record_fail "$desc" "not within ${1}s"
  fi
}

# set_web_env <dir> <key> <value> — replace KEY's line in <dir>/web.env.
# shellcheck disable=SC2329 # run through check or check_wait
set_web_env() {
  local dir="$1" key="$2" value="$3"
  grep -v "^$key=" "$dir/web.env" >"$dir/web.env.tmp"
  printf '%s=%s\n' "$key" "$value" >>"$dir/web.env.tmp"
  cat "$dir/web.env.tmp" >"$dir/web.env"
  rm -f "$dir/web.env.tmp"
}

# make_config_dir <dir> <appliance ip> — a filled-in copy of examples/webapp/.
# shellcheck disable=SC2329 # run through check or check_wait
make_config_dir() {
  local dir="$1" ip="$2"
  cp -r examples/webapp "$dir" &&
    printf '%s=%s  # validate operator\n' "$OPERATOR_ADDR" "$OPERATOR_PUB" >>"$dir/operator-peers" &&
    cp "$CERT_DIR/client.crt" "$dir/client.crt" &&
    mkdir -m 0700 "$dir/secrets" &&
    "$HARNESS" -mode genkey -private-key-file "$dir/secrets/wireguard.key" >/dev/null &&
    chmod 0600 "$dir/secrets/wireguard.key" &&
    set_web_env "$dir" WIREGUARD_ENDPOINT "$ip:51820" &&
    set_web_env "$dir" CONFIG_REPO_URL "git://$PROBE_IP/fleet.git" &&
    set_web_env "$dir" CONFIG_REPO_PRIVATE true
}

writer_disks() {
  on "$WRITER" sh -c "awk '\$4 ~ /^sd[a-z]+\$/ {print \$4}' /proc/partitions" | sort
}

# attach_to_writer <volume> — attach a custom volume to the writer and print
# the disk it shows up as.
attach_to_writer() {
  local vol="$1" before after dev _
  before=$(writer_disks)
  incus config device add --project "$PROJECT" "$REMOTE:$WRITER" "$vol" disk pool="$POOL" source="$vol" >/dev/null || return 1
  for _ in $(seq 1 30); do
    after=$(writer_disks)
    dev=$(comm -13 <(echo "$before") <(echo "$after") | head -1)
    [ -n "$dev" ] && break
    sleep 2
  done
  [ -n "$dev" ] && echo "$dev"
}

# write_seed_volume <volume> <seed dir> — a FAT volume labelled CIDATA
# holding the seed's user-data and meta-data.
# shellcheck disable=SC2329 # run through check or check_wait
write_seed_volume() {
  local vol="$1" seed="$2" dev
  dev=$(attach_to_writer "$vol") || return 1
  incus file push --project "$PROJECT" "$seed/user-data" "$seed/meta-data" "$REMOTE:$WRITER/tmp/" >/dev/null &&
    on "$WRITER" sh -c "mkfs.vfat -n CIDATA /dev/$dev >/dev/null && mkdir -p /mnt/cidata && mount /dev/$dev /mnt/cidata &&
      cp /tmp/user-data /tmp/meta-data /mnt/cidata/ && umount /mnt/cidata && rm /tmp/user-data /tmp/meta-data"
}

# boot_appliance <vm> <disk volume> <seed volume> <ip>
# shellcheck disable=SC2329 # run through check or check_wait
boot_appliance() {
  local vm="$1" disk="$2" seed="$3" ip="$4"
  incus init --empty --vm --project "$PROJECT" "$REMOTE:$vm" \
    -c security.secureboot=false -c limits.cpu=2 -c limits.memory=4GiB \
    --storage "$POOL" -d root,size=1GiB &&
    incus config device add --project "$PROJECT" "$REMOTE:$vm" disk disk pool="$POOL" source="$disk" boot.priority=10 &&
    incus config device add --project "$PROJECT" "$REMOTE:$vm" cidata disk pool="$POOL" source="$seed" &&
    incus config device add --project "$PROJECT" "$REMOTE:$vm" eth0 nic network="$NETWORK" ipv4.address="$ip" &&
    incus start --project "$PROJECT" "$REMOTE:$vm"
}

# shellcheck disable=SC2329 # run through check or check_wait
container_running() {
  [ "$(on "$1" docker inspect --format '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" = true ]
}

# shellcheck disable=SC2329 # run by check_wait
settings_applied() {
  on "$1" bash -c 'cmp -s <(jq -cS . /etc/homelab-ops/appliance/settings.json) <(jq -cS . /etc/homelab-ops/settings.applied.json)'
}

# shellcheck disable=SC2329 # run by check_wait
cloud_init_done() {
  on "$1" cloud-init status --wait 2>/dev/null | grep -qx 'status: done'
}

# app_public_key <vm> — the web app's WireGuard public key, from its log.
app_public_key() {
  on "$1" docker logs "$CONTAINER" 2>&1 | grep -oE 'wireguard public key [A-Za-z0-9+/=]+' | tail -1 | awk '{print $4}'
}

# api <ip> <app public key> <args...> — `bootstrap api` from the probe, as
# the operator peer.
api() {
  local ip="$1" pub="$2"
  shift 2
  probe /root/bootstrap api --private-key-file /root/operator.key --local-addr "$OPERATOR_ADDR" \
    --server-public-key "$pub" --endpoint "$ip:51820" "$@"
}

# check_sync <desc> <vm> <ip> — a sync over the tunnel reports the fixture's
# HEAD.
check_sync() {
  local desc="$1" vm="$2" ip="$3" pub out
  pub=$(app_public_key "$vm")
  if [ -z "$pub" ]; then
    record_fail "$desc" "no 'wireguard public key' in the web app's log"
    return
  fi
  if ! out=$(api "$ip" "$pub" sync 2>&1); then
    record_fail "$desc" "bootstrap api sync: $(tail -1 <<<"$out")"
    return
  fi
  check_json "$desc" "$out" ".commit == \"$FIXTURE_HEAD\""
}

# check_runs_pinned_digest <desc> <vm>
check_runs_pinned_digest() {
  local desc="$1" vm="$2" image digests
  image=$(on "$vm" docker inspect --format '{{.Config.Image}}' "$CONTAINER" 2>/dev/null)
  digests=$(on "$vm" docker image inspect --format '{{json .RepoDigests}}' "$WEB_IMAGE" 2>/dev/null)
  if [ "$image" = "$WEB_IMAGE" ] && grep -qF "\"$WEB_IMAGE\"" <<<"$digests"; then
    record_pass "$desc"
  else
    record_fail "$desc" "container image '$image', image digests $digests; want $WEB_IMAGE"
  fi
}

# check_compose_cli <desc> <vm> — the runbook's web image upgrade runs
# `docker compose` (the CLI plugin) in /etc/homelab-ops: it must work there
# and see the service's project. A failure names what the CLI printed.
check_compose_cli() {
  local desc="$1" vm="$2" out
  # shellcheck disable=SC2016 # expanded by the appliance's bash
  if out=$(on "$vm" bash -c 'cd /etc/homelab-ops && docker compose version && docker compose ps --format "{{.Name}}"' 2>&1) &&
    grep -qx "$CONTAINER" <<<"$out"; then
    record_pass "$desc"
  else
    record_fail "$desc" "$(tr '\n' ';' <<<"$out" | tail -c 600)"
  fi
}

# check_upgrade <vm> <reboot time> <window end>
check_upgrade() {
  local vm="$1" t="$2" end="$3" script exec_path missing="" want
  check "nixos-upgrade.timer is enabled" on "$vm" systemctl is-enabled nixos-upgrade.timer
  check "nixos-upgrade runs daily at the reboot time ($t)" timer_at "$vm" "$t"
  exec_path=$(on "$vm" systemctl show -P ExecStart nixos-upgrade.service | grep -o '/nix/store/[^ ;]*' | head -1)
  script=$(on "$vm" cat "$exec_path" 2>/dev/null)
  for want in "lower=\"$t\"" "upper=\"$end\"" "/bin/shutdown -r +1" \
    "/run/booted-system/{initrd,kernel,kernel-modules}" \
    "--update-input appliance --update-input appliance/nixpkgs"; do
    grep -qF -- "$want" <<<"$script" || missing="$missing [$want]"
  done
  if [ -z "$missing" ]; then
    record_pass "nixos-upgrade: allowReboot, only on a kernel/initrd/modules change, in the window $t-$end, following main and the nixpkgs branch head"
  else
    record_fail "nixos-upgrade: allowReboot in the window $t-$end" "its script lacks$missing"
  fi
}

# check_upgrade_runs <vm> — nixos-upgrade.service, started for real: the
# daily OS update path, with the box's own nix and nixos-rebuild. The one
# change from a real box: the local flake's appliance input points at the
# image's own appliance sources instead of github:...main, which has no
# deploy/nixos until #243 merges. --update-input appliance/nixpkgs still
# re-resolves nixpkgs to the release branch's head on GitHub.
check_upgrade_runs() {
  local vm="$1" src out lock
  # shellcheck disable=SC2016 # expanded by the appliance's bash
  src=$(on "$vm" bash -c 'cat "$(systemctl show -P ExecStart homelab-appliance-settings.service | grep -o "/nix/store/[^ ;]*" | head -1)"' 2>/dev/null |
    grep -oE "override-input appliance 'path:[^']+'" | head -1 | cut -d"'" -f2)
  if [ -z "$src" ]; then
    record_fail "nixos-upgrade.service runs and succeeds" "no appliance source in homelab-appliance-settings' script"
    return
  fi
  # shellcheck disable=SC2016 # expanded by the appliance's bash
  if ! on "$vm" bash -c 'sed -i -E "s|^( *inputs\.appliance\.url = ).*|\1\"$1\";|" /etc/homelab-ops/appliance/flake.nix &&
      grep -qF "$1" /etc/homelab-ops/appliance/flake.nix' _ "$src"; then
    record_fail "nixos-upgrade.service runs and succeeds" "couldn't point the local flake at $src"
    return
  fi
  echo "nixos-upgrade.service: re-locking nixpkgs and building the upgrade (several minutes) ..."
  if ! out=$(on "$vm" timeout 3000 systemctl start nixos-upgrade.service 2>&1); then
    record_fail "nixos-upgrade.service runs and succeeds" "$(tail -1 <<<"$out"); $(on "$vm" journalctl -u nixos-upgrade.service -n 15 --no-pager 2>&1 | tr '\n' ';')"
    return
  fi
  check_eq "nixos-upgrade.service's result" success "$(on "$vm" systemctl show -P Result nixos-upgrade.service)"
  lock=$(on "$vm" cat /etc/homelab-ops/appliance/flake.lock 2>/dev/null)
  check_json "nixos-upgrade wrote flake.lock, nixpkgs locked from its GitHub branch" "$lock" \
    '[.nodes[] | select(.locked.type? == "github" and .locked.repo? == "nixpkgs")] | length == 1'
}

# check_only_wireguard_open <vm> <ip> [ssh]
check_only_wireguard_open() {
  local vm="$1" ip="$2" ssh="${3:-}" ports tcp scan open want
  ports=$(on "$vm" docker port "$CONTAINER" 2>/dev/null)
  if [ -n "$ports" ] && ! grep -qv '^51820/udp ' <<<"$ports"; then
    record_pass "the compose service publishes 51820/udp alone"
  else
    record_fail "the compose service publishes 51820/udp alone" "docker port: $(tr '\n' ';' <<<"$ports")"
  fi
  tcp=$(on "$vm" ss -Htln 2>/dev/null | awk '{print $4}' | grep -vE '^(127\.[0-9.]+|\[::1\]|::1):' | LC_ALL=C sort -u | tr '\n' ' ')
  want=""
  [ -n "$ssh" ] && want="0.0.0.0:22 [::]:22 "
  check_eq "TCP listeners beyond loopback" "$want" "$tcp"
  scan=$(probe nmap -Pn -sT --max-retries 1 -p 1-1024,2375,2376,5000,8080,8081 "$ip" 2>&1)
  open=$(grep -oE '^[0-9]+/tcp +open' <<<"$scan" | awk -F/ '{print $1}' | tr '\n' ' ')
  want=""
  [ -n "$ssh" ] && want="22 "
  check_eq "open TCP ports seen from the LAN" "$want" "$open"
}

echo "== 1. Build the image, bootstrap and the tunnel harness =="
if [ -n "${VALIDATE_APPLIANCE_IMAGE:-}" ]; then
  IMG="$VALIDATE_APPLIANCE_IMAGE"
  check "VALIDATE_APPLIANCE_IMAGE names a file" test -f "$IMG"
else
  IMG="$ROOT_DIR/bootstrap-output/appliance/homelab-ops-appliance.img"
  echo "make appliance-image (a first run builds under TCG for ~10m) ..."
  if make appliance-image >"$WORK_DIR/image.log" 2>&1; then
    record_pass "make appliance-image builds the image from flake.lock"
  else
    record_fail "make appliance-image builds the image from flake.lock" "$(tail -3 "$WORK_DIR/image.log")"
  fi
fi
check "build bootstrap (static, for the probe)" env CGO_ENABLED=0 go build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
check "build validate-tunnel-harness" go build -o "$HARNESS" ./cmd/validate-tunnel-harness
if [ ! -f "$IMG" ] || [ ! -x "$BOOTSTRAP_BIN" ] || [ ! -x "$HARNESS" ]; then
  echo "ERROR: no image, bootstrap or harness to go on with" >&2
  summary
fi
check "deploy/compose.yaml pins the web image by digest" bash -c "[[ '$WEB_IMAGE' =~ @sha256:[0-9a-f]{64}\$ ]]"

echo
echo "== 2. A probe on $NETWORK serving the fixture fleet repo; the config dirs and seeds =="
check "probe container ready" bash -c "
  set -e
  incus launch --project '$PROJECT' '$REMOTE:$VALIDATE_ALPINE_CT' '$REMOTE:$PROBE' --network '$NETWORK' --storage '$POOL' >/dev/null
  for _ in \$(seq 1 30); do
    incus exec --project '$PROJECT' '$REMOTE:$PROBE' -- apk add --no-cache git git-daemon openssh-client nmap >/dev/null 2>&1 && break
    sleep 2
  done
  incus exec --project '$PROJECT' '$REMOTE:$PROBE' -- sh -c 'command -v git && command -v ssh && command -v nmap'
"
# The name as a separate filter: `incus list <remote>:<name>` lists nothing.
PROBE_IP=$(incus list --project "$PROJECT" "$REMOTE:" "$PROBE" -c 4 -f csv | grep -oE '^[0-9.]+' | head -1)
check "probe has an address on $NETWORK" test -n "$PROBE_IP"
incus file push --project "$PROJECT" dev/git-fixture/fleet.yaml "$REMOTE:$PROBE/root/fleet.yaml" >/dev/null
check "the probe serves the fixture fleet over git://" probe sh -c '
  set -e
  mkdir -p /work /srv/git && cd /work
  git init -q -b main . && git config user.email validate@example.invalid && git config user.name validate
  cp /root/fleet.yaml . && git add fleet.yaml && git commit -qm "validate fixture"
  git clone -q --bare /work /srv/git/fleet.git
  git daemon --base-path=/srv/git --export-all --reuseaddr --detach /srv/git'
FIXTURE_HEAD=$(probe git -C /work rev-parse HEAD 2>/dev/null)
check "probe ssh key generated" probe sh -c "mkdir -p -m 0700 /root/.ssh && ssh-keygen -q -t ed25519 -N '' -f /root/.ssh/id_ed25519"
incus file pull --project "$PROJECT" "$REMOTE:$PROBE/root/.ssh/id_ed25519.pub" "$WORK_DIR/probe.pub" >/dev/null 2>&1
incus file push --project "$PROJECT" "$BOOTSTRAP_BIN" "$REMOTE:$PROBE/root/bootstrap" --mode 0755 >/dev/null

OPERATOR_PUB=$("$HARNESS" -mode genkey -private-key-file "$OPERATOR_KEY")
check "operator keypair generated" test -n "$OPERATOR_PUB"
incus file push --project "$PROJECT" "$OPERATOR_KEY" "$REMOTE:$PROBE/root/operator.key" --mode 0600 >/dev/null
check "gen-cert exits 0" "$BOOTSTRAP_BIN" gen-cert --output-dir "$CERT_DIR" --common-name web-appliance-boots-from-image-and-seed
check "config dir 1 filled in" make_config_dir "$WORK_DIR/web-1" "$IP_1"
check "config dir 2 filled in" make_config_dir "$WORK_DIR/web-2" "$IP_2"
check "render-appliance-seed: dir 1, --image, --reboot-time $REBOOT_TIME_1" "$BOOTSTRAP_BIN" render-appliance-seed \
  --config-dir "$WORK_DIR/web-1" --image "$WEB_IMAGE" --reboot-time "$REBOOT_TIME_1" --output "$WORK_DIR/seed-1"
check "render-appliance-seed: dir 2, --no-secrets, --ssh-authorized-keys, --reboot-time $REBOOT_TIME_2" "$BOOTSTRAP_BIN" render-appliance-seed \
  --config-dir "$WORK_DIR/web-2" --image "$WEB_IMAGE" --reboot-time "$REBOOT_TIME_2" \
  --no-secrets --ssh-authorized-keys "$WORK_DIR/probe.pub" --output "$WORK_DIR/seed-2"

echo
echo "== 3. Stream the image and the seeds onto $REMOTE through a writer VM =="
img_bytes=$(stat -c%s "$IMG")
# Room past the image for the root partition to grow into: the Docker images
# and the first-boot rebuild.
vol_gib=$(((img_bytes + (1 << 30) - 1) / (1 << 30) + 12))
check "block volumes created" bash -c "
  set -e
  incus storage volume create '$REMOTE:$POOL' '$IMG_VOL_1' --type=block size=${vol_gib}GiB --project '$PROJECT' >/dev/null
  incus storage volume create '$REMOTE:$POOL' '$SEED_VOL_1' --type=block size=64MiB --project '$PROJECT' >/dev/null
  incus storage volume create '$REMOTE:$POOL' '$SEED_VOL_2' --type=block size=64MiB --project '$PROJECT' >/dev/null
"
check "writer VM ready, with dosfstools" bash -c "
  set -e
  incus launch --project '$PROJECT' '$REMOTE:$VALIDATE_ALPINE_VM' '$REMOTE:$WRITER' --vm --network '$NETWORK' --storage '$POOL' \
    -c security.secureboot=false -c limits.cpu=2 -c limits.memory=1GiB >/dev/null
  for _ in \$(seq 1 60); do
    incus exec --project '$PROJECT' '$REMOTE:$WRITER' -- apk add --no-cache dosfstools >/dev/null 2>&1 && break
    sleep 3
  done
  incus exec --project '$PROJECT' '$REMOTE:$WRITER' -- sh -c 'command -v mkfs.vfat'
"
if dev=$(attach_to_writer "$IMG_VOL_1"); then
  echo "dd $(basename "$IMG") ($((img_bytes >> 20)) MiB) onto /dev/$dev ..."
  check "the raw-efi image is written to its volume" bash -c \
    "incus exec --project '$PROJECT' '$REMOTE:$WRITER' -- sh -c 'dd of=/dev/$dev bs=4M 2>/dev/null && sync' <'$IMG'"
else
  record_fail "the raw-efi image is written to its volume" "the volume never showed up in the writer"
fi
check "seed 1 is on a FAT volume labelled CIDATA" write_seed_volume "$SEED_VOL_1" "$WORK_DIR/seed-1"
check "seed 2 is on a FAT volume labelled CIDATA" write_seed_volume "$SEED_VOL_2" "$WORK_DIR/seed-2"
incus delete --force --project "$PROJECT" "$REMOTE:$WRITER" >/dev/null 2>&1
check "the image volume is copied for the second VM" \
  incus storage volume copy --project "$PROJECT" "$REMOTE:$POOL/$IMG_VOL_1" "$REMOTE:$POOL/$IMG_VOL_2"

echo
echo "== 4. VM 1: the image with seed 1 (secrets inline) =="
check "VM 1 boots from the image with the seed attached" boot_appliance "$VM_1" "$IMG_VOL_1" "$SEED_VOL_1" "$IP_1"
check_wait "the incus agent answers (the appliance booted)" 600 on "$VM_1" true
check_wait "cloud-init: status: done" 900 cloud_init_done "$VM_1"
check_wait "the seed is marked consumed once cloud-init's cache is scrubbed" 300 on "$VM_1" test -e /var/lib/homelab-ops/seed-consumed
WG_KEY_1=$(cat "$WORK_DIR/web-1/secrets/wireguard.key")
# shellcheck disable=SC2016 # expanded by the appliance's bash
check "the WireGuard key is in secrets/ and nowhere under /var/lib/cloud" on "$VM_1" bash -c \
  'grep -qF -- "$1" /etc/homelab-ops/web/secrets/wireguard.key && ! grep -rqF -- "$1" /var/lib/cloud' _ "$WG_KEY_1"
# shellcheck disable=SC2016 # expanded by the appliance's bash
check "the seed's files are in place with their modes" on "$VM_1" bash -c '
  [ "$(stat -c %a /etc/homelab-ops/web/secrets)" = 700 ] &&
  [ "$(stat -c %a /etc/homelab-ops/web/secrets/wireguard.key)" = 600 ] &&
  [ "$(stat -c %a /etc/homelab-ops/web/web.env)" = 644 ] && test -f /etc/homelab-ops/compose.yaml'
check_wait "the seed's settings are applied (first-boot nixos-rebuild)" 1500 settings_applied "$VM_1"
check_wait "the compose service is running" 900 container_running "$VM_1"
check_runs_pinned_digest "the compose service runs from the pinned digest" "$VM_1"
check_only_wireguard_open "$VM_1" "$IP_1"
check_compose_cli "docker compose works in /etc/homelab-ops for a manual web image upgrade" "$VM_1"
check "sshd is off without --ssh-authorized-keys" inactive "$VM_1" sshd.service
check_upgrade "$VM_1" "$REBOOT_TIME_1" "04:17"
check_sync "operator peer: bootstrap api sync over the tunnel reports the fixture's HEAD" "$VM_1" "$IP_1"
check_upgrade_runs "$VM_1"

echo "-- reboot --"
check "VM 1 reboots" incus restart --project "$PROJECT" "$REMOTE:$VM_1" --timeout 120
check_wait "the incus agent answers after the reboot" 600 on "$VM_1" true
# shellcheck disable=SC2016 # expanded by the appliance's bash
check "it booted the generation nixos-upgrade built" on "$VM_1" bash -c \
  '[ "$(readlink -f /run/booted-system)" = "$(readlink -f /nix/var/nix/profiles/system)" ]'
check_eq "cloud-init stays off once the seed is consumed" no "$(on "$VM_1" systemctl show -P ConditionResult cloud-init.service)"
# Once #197 lands, the store is sealed here and needs `bootstrap api unlock`
# before the sync below.
check_wait "the compose service comes back on its own" 600 container_running "$VM_1"
check_runs_pinned_digest "after the reboot it still runs from the pinned digest" "$VM_1"
check_sync "after the reboot: a sync over the tunnel succeeds" "$VM_1" "$IP_1"
incus delete --force --project "$PROJECT" "$REMOTE:$VM_1" >/dev/null 2>&1

echo
echo "== 5. VM 2: the image with seed 2 (--no-secrets, SSH keys) =="
check "VM 2 boots from the image with the seed attached" boot_appliance "$VM_2" "$IMG_VOL_2" "$SEED_VOL_2" "$IP_2"
check_wait "the incus agent answers (the appliance booted)" 600 on "$VM_2" true
check_wait "cloud-init: status: done" 900 cloud_init_done "$VM_2"
check_wait "the seed's settings are applied (first-boot nixos-rebuild)" 1500 settings_applied "$VM_2"
check "--no-secrets: no secrets/ on the appliance" on "$VM_2" test ! -e /etc/homelab-ops/web/secrets
check_wait "docker is up" 300 on "$VM_2" systemctl is-active docker.service
sleep 20
check "the web service waits while secrets/ is absent" web_waits "$VM_2"
check_upgrade "$VM_2" "$REBOOT_TIME_2" "00:30"
check_wait "root logs in over SSH with the seed's key" 300 probe ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o BatchMode=yes -o ConnectTimeout=5 "root@$IP_2" true
incus file push --project "$PROJECT" -r "$WORK_DIR/web-2/secrets" "$REMOTE:$PROBE/root/" >/dev/null
check "secrets/ pushed in over scp, staged then moved into place" probe sh -c "
  chmod 0700 /root/secrets && chmod 0600 /root/secrets/wireguard.key &&
  scp -q -rp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes /root/secrets root@$IP_2:/etc/homelab-ops/web/secrets.new &&
  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes root@$IP_2 mv /etc/homelab-ops/web/secrets.new /etc/homelab-ops/web/secrets"
check_wait "the compose service starts once secrets/ is there" 900 container_running "$VM_2"
check_runs_pinned_digest "it runs from the pinned digest" "$VM_2"
# With the seed's SSH keys, port 22 joins WireGuard's.
check_only_wireguard_open "$VM_2" "$IP_2" ssh
check_sync "operator peer: a sync over the tunnel succeeds" "$VM_2" "$IP_2"

summary
