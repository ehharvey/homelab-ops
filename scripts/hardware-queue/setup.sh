#! /bin/bash
# Builds, or updates, the validate/hardware queue's VM on the Incus host
# (#664). Run it from a checkout, as the operator: it uses your Incus remote
# (full trust) and your own `gh` login, and hands the VM only the job's token.
#
#   scripts/hardware-queue/setup.sh                 build or update from origin/main
#   scripts/hardware-queue/setup.sh --code-ref <r>  ... from another commit, to prove
#                                                   a change to the job itself before
#                                                   it merges; never a PR head
#   scripts/hardware-queue/setup.sh --no-start      everything but (re)starting the job;
#                                                   it is stopped, and starts at the
#                                                   VM's next boot
#
# Deleting the VM and rerunning this restores a working job; rerunning it on a
# live VM updates the job's code, tools, token and settings, and restarts the
# job (a run in progress keeps its `pending` and reruns). Token rotation: make
# a new token, put it in .env, rerun this.
#
# Refuses, before contacting anything, when the checkout's .env (or the main
# checkout's, when the token is read from there) is tracked by git or not
# ignored: the token is never to be committable.
#
# What it does:
#   1. The token: ISSUE664_VALIDATE_BOT_PAT from the environment, else the
#      first .env (this checkout's, then the main checkout's) that sets it. A
#      fine-grained token for this repository only: Contents read, Pull
#      requests read, Commit statuses write.
#   2. With your `gh`: HQ_OPERATOR is the only collaborator with write access,
#      and there are no deploy keys and no pending invitations (the job's token
#      can see only the first; the other two need administration read).
#   3. With the job's token: it reads the repository, and its expiry.
#   4. On the Incus remote: a VM HQ_VM_NAME (images:debian/13, boot.autostart)
#      on home-lan at HQ_SETUP_ADDR, created if missing. 192.168.1.253 sits
#      outside home-lan's DHCP range (.2-.199) and outside the addresses the
#      scripts use (.50, .200-.250 for IPAM, .201 for node0).
#   5. Pushes main's copy of the job (scripts/hardware-queue,
#      scripts/validate/lib-hardware-lock.sh, docker-compose.yml,
#      dev/incus-bridge) and runs provision.sh in the VM.
#   6. Gives the VM's `hq` user a full-trust client certificate on the remote,
#      under the same remote name, through a one-time trust token. A
#      project-restricted certificate can't pass the suite (#664), so PR code
#      run there has admin over the host; see scripts/validate/README.md
#      § "The hardware queue".
#   7. Writes /etc/hardware-queue/setup.env (the incus-bridge address and
#      server name) and the token (mode 0400, owner hq), and restarts the job.
#
# Settings (environment): HQ_SETUP_NETWORK (home-lan), HQ_SETUP_ADDR
# (192.168.1.253), HQ_SETUP_IMAGE (images:debian/13), HQ_SETUP_CPU (4),
# HQ_SETUP_MEMORY (8GiB), HQ_SETUP_DISK (80GiB). The remote, project, VM name,
# repository and operator are config.env's.

set -uo pipefail

die() {
	echo "setup: ERROR: $*" >&2
	exit 1
}
say() { echo "setup: $*" >&2; }

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/hardware-queue/config.env
. "$HERE/config.env"
CODE_REF=origin/main
START=1
while [ $# -gt 0 ]; do
	case "$1" in
	--code-ref)
		CODE_REF="${2:?--code-ref needs a commit}"
		shift
		;;
	--no-start) START=0 ;;
	-h | --help)
		sed -n '2,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $1" ;;
	esac
	shift
done

REMOTE="$HQ_INCUS_REMOTE"
PROJECT="$HQ_INCUS_PROJECT"
VM="$HQ_VM_NAME"
NETWORK="${HQ_SETUP_NETWORK:-home-lan}"
ADDR="${HQ_SETUP_ADDR:-192.168.1.253}"
IMAGE="${HQ_SETUP_IMAGE:-images:debian/13}"
TOKEN_VAR=ISSUE664_VALIDATE_BOT_PAT

# --- 0. .env must never be committable ------------------------------------------

top="$(git -C "$HERE" rev-parse --show-toplevel 2>/dev/null)" || die "run this from a git checkout"
common="$(git -C "$top" rev-parse --path-format=absolute --git-common-dir)"
main_top="$(dirname "$common")"

# env_guard <checkout> — its .env is neither tracked nor unignored.
env_guard() {
	if [ -n "$(git -C "$1" ls-files -- .env)" ]; then
		die "$1/.env is tracked by git; untrack it (git rm --cached .env) and keep the token out of history"
	fi
	if ! git -C "$1" check-ignore -q .env; then
		die "$1/.env is not ignored by git (git check-ignore -q .env fails); .gitignore needs its .env rule"
	fi
}
env_guard "$top"

# --- 1. The token -----------------------------------------------------------------

# Read, not sourced: .env is data. Tolerates CRLF, surrounding whitespace, quotes
# and an unquoted trailing " # comment".
env_value() {
	sed -n "s/^\(export \)\{0,1\}$TOKEN_VAR=//p" "$1" | tail -n 1 | tr -d '\r' |
		sed -E "s/^[[:space:]]+//; s/[[:space:]]+\$//; s/^\"([^\"]*)\".*/\1/; s/^'([^']*)'.*/\1/; s/[[:space:]]+#.*\$//; s/[[:space:]]+\$//"
}
TOKEN="${!TOKEN_VAR:-}"
src="the environment"
if [ -z "$TOKEN" ]; then
	for d in "$top" "$main_top"; do
		[ -f "$d/.env" ] || continue
		env_guard "$d"
		TOKEN="$(env_value "$d/.env")"
		src="$d/.env"
		[ -z "$TOKEN" ] || break
	done
fi
unset "$TOKEN_VAR"
[ -n "$TOKEN" ] || die "$TOKEN_VAR is not set, in the environment or in $top/.env or $main_top/.env"
say "token from $src"

# --- 2. The repository's writers, with the operator's own gh ----------------------

writers="$(gh api "repos/$HQ_REPO/collaborators?per_page=100" --paginate \
	--jq '.[] | select(.permissions.push or .permissions.maintain or .permissions.admin) | .login')" ||
	die "listing collaborators with your gh failed"
[ "$writers" = "$HQ_OPERATOR" ] ||
	die "write access is not $HQ_OPERATOR's alone: $(echo "$writers" | tr '\n' ' ')"
keys="$(gh api "repos/$HQ_REPO/keys" --jq 'length')" ||
	die "listing deploy keys failed (needs your own gh login, with administration read)"
[ "$keys" = 0 ] || die "$HQ_REPO has $keys deploy key(s); a key can push code the queue would run"
inv="$(gh api "repos/$HQ_REPO/invitations" --jq 'length')" || die "listing invitations failed"
[ "$inv" = 0 ] || die "$HQ_REPO has $inv pending invitation(s)"
say "writers: $HQ_OPERATOR only; no deploy keys, no invitations"

# --- 3. The job's token -------------------------------------------------------------

resp="$(GH_TOKEN="$TOKEN" gh api -i "repos/$HQ_REPO/commits/main/status" 2>&1)" ||
	die "the job's token can't read commit statuses on $HQ_REPO: $(head -n 1 <<<"$resp")"
expiry="$(tr -d '\r' <<<"$resp" | grep -i -m 1 '^github-authentication-token-expiration:' | cut -d: -f2- | sed 's/^ //')"
say "the job's token reads $HQ_REPO; expires ${expiry:-never (no expiry header)}"
# Reading isn't enough: a token without Commit statuses: write would pass here,
# then rerun the same head forever. Post to main's head under a context of its
# own, so no real result is touched. Statuses can't be deleted, so each run of
# setup.sh leaves one more success under that context on main's head.
main_sha="$(GH_TOKEN="$TOKEN" gh api "repos/$HQ_REPO/commits/main" --jq .sha)" ||
	die "the job's token can't read main's head on $HQ_REPO"
GH_TOKEN="$TOKEN" gh api -X POST "repos/$HQ_REPO/statuses/$main_sha" \
	-f state=success -f context=validate/hardware-setup \
	-f description="hardware-queue setup: the token can write statuses" >/dev/null 2>&1 ||
	die "the job's token can't write commit statuses on $HQ_REPO (needs Commit statuses: write)"
say "the job's token writes statuses too"

# --- 4. The VM ----------------------------------------------------------------------

info="$(incus query "$REMOTE:/1.0")" || die "Incus remote '$REMOTE' unreachable"
server_major="$(jq -r '.environment.server_version' <<<"$info" | cut -d. -f1)"
server_name="$(jq -r '.environment.server_name' <<<"$info")"
gw="$(incus network get "$REMOTE:$NETWORK" ipv4.address --project "$PROJECT")" || die "no network $NETWORK on $REMOTE"
gw="${gw%/*}"
for a in "${VALIDATE_ALPINE_CT:-validate-alpine}" "${VALIDATE_ALPINE_VM:-validate-alpine-vm}"; do
	incus image list "$REMOTE:" --project "$PROJECT" -f json | jq -e --arg a "$a" 'any(.[].aliases[]?; .name == $a)' >/dev/null ||
		die "pinned image alias $a is missing on $REMOTE (run .devcontainer/scripts/3-pin-validate-images.sh)"
done
holder="$(incus network list-leases "$REMOTE:$NETWORK" --project "$PROJECT" -f json |
	jq -r --arg a "$ADDR" --arg vm "$VM" '.[] | select(.address == $a and .hostname != $vm) | .hostname' | head -n 1)"
[ -z "$holder" ] || die "$ADDR on $NETWORK is leased to $holder"

vmx() { incus exec --project "$PROJECT" "$REMOTE:$VM" "$@"; }

if ! incus info --project "$PROJECT" "$REMOTE:$VM" >/dev/null 2>&1; then
	say "creating VM $VM from $IMAGE on $REMOTE ($NETWORK, $ADDR)"
	incus init "$IMAGE" "$REMOTE:$VM" --vm --project "$PROJECT" \
		-c limits.cpu="${HQ_SETUP_CPU:-4}" -c limits.memory="${HQ_SETUP_MEMORY:-8GiB}" \
		-c boot.autostart=true -c user.homelab-ops.role=hardware-queue \
		-d root,size="${HQ_SETUP_DISK:-80GiB}" -n "$NETWORK" >/dev/null || die "creating $VM failed"
	incus config device set "$REMOTE:$VM" eth0 ipv4.address="$ADDR" --project "$PROJECT" ||
		die "pinning $VM's address to $ADDR failed"
else
	say "updating VM $VM on $REMOTE"
	incus config set "$REMOTE:$VM" boot.autostart=true --project "$PROJECT" || die "setting boot.autostart failed"
fi
if [ "$(incus query "$REMOTE:/1.0/instances/$VM?project=$PROJECT" | jq -r .status)" != Running ]; then
	incus start "$REMOTE:$VM" --project "$PROJECT" || die "starting $VM failed"
fi
for _ in $(seq 1 90); do
	vmx -- getent hosts deb.debian.org >/dev/null 2>&1 && break
	sleep 2
done
vmx -- getent hosts deb.debian.org >/dev/null 2>&1 || die "$VM has no agent or no network after 3 minutes"

# --- 5. main's copy of the job, and the tools --------------------------------------

if [ "$CODE_REF" = origin/main ]; then
	git -C "$top" fetch --quiet origin main || die "fetching origin/main failed"
else
	say "NOTE: job code from $CODE_REF, not origin/main: for proving a change to the job itself"
fi
sha="$(git -C "$top" rev-parse --verify "$CODE_REF^{commit}")" || die "no commit $CODE_REF"
flasher="$(git -C "$top" rev-parse "$sha:third_party/incus-os")" || die "no third_party/incus-os at $CODE_REF"
say "job code at ${sha:0:12}"
vmx -- sh -c 'rm -rf /opt/hardware-queue/main.new && mkdir -p /opt/hardware-queue/main.new' || die "preparing $VM failed"
git -C "$top" archive --format=tar "$sha" scripts/hardware-queue scripts/validate/lib-hardware-lock.sh \
	docker-compose.yml dev/incus-bridge | vmx -- tar -x -C /opt/hardware-queue/main.new ||
	die "pushing the job's code failed"
echo "$sha" | vmx -- sh -c 'cat >/opt/hardware-queue/main.new/COMMIT'
vmx --env HQ_INCUS_MAJOR="$server_major" --env HQ_FLASHER_REF="$flasher" -- \
	bash /opt/hardware-queue/main.new/scripts/hardware-queue/provision.sh || die "provisioning $VM failed"

# --- 6. The VM's Incus client: full trust ------------------------------------------

hq() { vmx -- su - hq -c "$1"; }
auth="$(hq "incus query $REMOTE:/1.0 2>/dev/null | jq -r .auth")"
if [ "$auth" != trusted ]; then
	say "trusting $VM's client certificate on $REMOTE (full trust)"
	for fp in $(incus config trust list "$REMOTE:" -f json | jq -r --arg n "$VM" '.[] | select(.name == $n) | .fingerprint'); do
		incus config trust remove "$REMOTE:$fp" || die "removing $VM's old certificate failed"
	done
	# The server's certificate, as your own (already pinned) client sees it:
	# the VM's `remote add` asks for it, and gets this, never a blind yes.
	fp="$(jq -r '.environment.certificate_fingerprint' <<<"$info")"
	[[ "$fp" =~ ^[0-9a-f]{64}$ ]] || die "no certificate fingerprint from $REMOTE"
	trust="$(incus config trust add "$REMOTE:$VM" | tail -n 1)"
	[[ "$trust" =~ ^[A-Za-z0-9+/=]+$ ]] || die "could not issue a trust token on $REMOTE"
	hq "incus remote switch local >/dev/null 2>&1; incus remote remove $REMOTE >/dev/null 2>&1; echo $fp | incus remote add $REMOTE https://$gw:8443 --token $trust >/dev/null && incus remote switch $REMOTE" ||
		{
			incus config trust revoke-token "$REMOTE:$VM" >/dev/null 2>&1
			die "adding remote $REMOTE in $VM failed"
		}
fi
[ "$(hq "incus query $REMOTE:/1.0 | jq -r .auth")" = trusted ] || die "$VM's client is not trusted on $REMOTE"
client="$(hq 'incus --version')"
[ "${client%%.*}" = "$server_major" ] || die "client $client in $VM, server major $server_major"

# --- 7. Settings, token, restart ----------------------------------------------------

printf 'HQ_BRIDGE_HOST_ADDR=%s:8443\nHQ_BRIDGE_SERVER_NAME=%s\n' "$gw" "$server_name" |
	vmx -- install -m 0644 /dev/stdin /etc/hardware-queue/setup.env || die "writing setup.env failed"
printf '%s' "$TOKEN" | vmx -- install -m 0400 -o hq -g hq /dev/stdin /etc/hardware-queue/token ||
	die "writing the token failed"
if [ "$START" = 0 ]; then
	vmx -- systemctl stop hardware-queue || die "stopping hardware-queue failed"
	say "hardware-queue is stopped (--no-start) in $VM ($ADDR), job code ${sha:0:12}, incus client $client"
	exit 0
fi
vmx -- systemctl restart hardware-queue || die "starting hardware-queue failed"
sleep 3
state="$(vmx -- systemctl is-active hardware-queue)"
say "hardware-queue is $state in $VM ($ADDR), job code ${sha:0:12}, incus client $client"
say "follow it with: incus exec $REMOTE:$VM -- journalctl -fu hardware-queue"
[ "$state" = active ]
