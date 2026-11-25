#! /bin/bash
# Provisions the validate/hardware queue's VM (#664). setup.sh pushes main's
# copy of the job's files to /opt/hardware-queue/main.new and runs this inside
# the VM, as root; it is idempotent, so every setup.sh run reruns it.
#
# Installs what `run.sh --describe` lists for the incus and incus-vm groups,
# plus what devcontainer-reaches-host-incus.sh checks for (it is in the incus
# group and asserts the devcontainer's tools: incus, tofu, docker, gh, go):
#
#   git make jq curl openssl python3 gzip unzip timeout flock setsid blkid
#   docker + compose (Docker's apt repo: stock Docker, which a VM runs without
#                     security.nesting, docs/Decisions.md §20)
#   incus-client     zabbly's stable line, or lts-<major>.0 when the server is
#                    on an older major: client and server majors must agree
#                    (docs/Decisions.md §21)
#   go               the newest stable release from go.dev, checksum-verified
#   flasher-tool     go install at the third_party/incus-os commit main pins
#   gh               Debian's package; the job calls it with its token, and
#                    it has no login of its own
#   tofu             a pinned OpenTofu release, checksum-verified
#
# Then the user `hq` (uid 1000: the incus-bridge compose service runs as
# 1000:1000 and its socket is mode 600), the job's directories, the code swap
# (main.new -> main) and the systemd unit. It does not touch the Incus remote
# or the token; setup.sh does those.
#
# Environment (from setup.sh): HQ_INCUS_MAJOR, the server's major version;
# HQ_FLASHER_REF, the incus-os commit; HQ_TOFU_VERSION.

set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

: "${HQ_INCUS_MAJOR:?the major version of the Incus server}"
: "${HQ_FLASHER_REF:?the third_party/incus-os commit}"
HQ_TOFU_VERSION="${HQ_TOFU_VERSION:-1.13.2}"
NEW=/opt/hardware-queue/main.new
CODE=/opt/hardware-queue/main

say() { echo "provision: $*"; }

say "packages"
apt-get update -q
apt-get install -y -q ca-certificates curl gnupg git make jq openssl python3 gzip unzip \
	util-linux procps coreutils gh systemd-timesyncd >/dev/null
# shellcheck source=/dev/null # the VM's, not this checkout's
. /etc/os-release
arch="$(dpkg --print-architecture)"
install -d -m 0755 /etc/apt/keyrings

if [ ! -f /etc/apt/sources.list.d/docker.sources ]; then
	say "Docker's apt repository"
	curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
	cat >/etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/debian
Suites: $VERSION_CODENAME
Components: stable
Architectures: $arch
Signed-By: /etc/apt/keyrings/docker.asc
EOF
fi
# The client line follows the server's major: stable while it is current,
# else that major's LTS line.
channel=stable
if [ "$HQ_INCUS_MAJOR" -lt 7 ]; then
	channel="lts-$HQ_INCUS_MAJOR.0"
fi
say "zabbly incus/$channel"
curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
cat >/etc/apt/sources.list.d/zabbly-incus.sources <<EOF
Types: deb
URIs: https://pkgs.zabbly.com/incus/$channel
Suites: $VERSION_CODENAME
Components: main
Architectures: $arch
Signed-By: /etc/apt/keyrings/zabbly.asc
EOF
apt-get update -q
apt-get install -y -q docker-ce docker-ce-cli containerd.io docker-buildx-plugin \
	docker-compose-plugin incus-client >/dev/null
systemctl enable --now docker >/dev/null
client_major="$(incus --version | cut -d. -f1)"
if [ "$client_major" != "$HQ_INCUS_MAJOR" ]; then
	echo "provision: ERROR: incus client $(incus --version) from incus/$channel, server major $HQ_INCUS_MAJOR" >&2
	exit 1
fi

say "go"
gojson="$(curl -fsSL 'https://go.dev/dl/?mode=json')"
gofile="$(jq -r --arg a "$arch" '[.[] | select(.stable)][0].files[] | select(.os == "linux" and .arch == $a and .kind == "archive") | .filename' <<<"$gojson")"
gosum="$(jq -r --arg f "$gofile" '.[].files[] | select(.filename == $f) | .sha256' <<<"$gojson")"
[[ "$gofile" =~ ^go[0-9.]+\.linux-[a-z0-9]+\.tar\.gz$ && "$gosum" =~ ^[0-9a-f]{64}$ ]] || {
	echo "provision: ERROR: go.dev/dl listed no linux-$arch archive" >&2
	exit 1
}
if [ "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')" != "${gofile%.linux-*}" ]; then
	tmp="$(mktemp -d)"
	curl -fsSL "https://go.dev/dl/$gofile" -o "$tmp/$gofile"
	echo "$gosum  $tmp/$gofile" | sha256sum -c --quiet
	rm -rf /usr/local/go
	tar -C /usr/local -xzf "$tmp/$gofile"
	rm -rf "$tmp"
fi
# The unit sets the same PATH; this is for a login shell in the VM.
# shellcheck disable=SC2016 # expanded at login, not here
echo 'export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"' >/etc/profile.d/hardware-queue-path.sh

say "tofu $HQ_TOFU_VERSION"
if [ "$(tofu version 2>/dev/null | head -n 1)" != "OpenTofu v$HQ_TOFU_VERSION" ]; then
	tmp="$(mktemp -d)"
	base="https://github.com/opentofu/opentofu/releases/download/v$HQ_TOFU_VERSION"
	zip="tofu_${HQ_TOFU_VERSION}_linux_$arch.zip"
	curl -fsSL "$base/$zip" -o "$tmp/$zip"
	curl -fsSL "$base/tofu_${HQ_TOFU_VERSION}_SHA256SUMS" -o "$tmp/SUMS"
	(cd "$tmp" && grep " $zip\$" SUMS | sha256sum -c --quiet)
	unzip -q -o "$tmp/$zip" tofu -d /usr/local/bin
	rm -rf "$tmp"
fi

say "user hq"
if ! id hq >/dev/null 2>&1; then
	if getent passwd 1000 >/dev/null; then
		echo "provision: ERROR: uid 1000 is taken by $(getent passwd 1000 | cut -d: -f1); incus-bridge runs as 1000" >&2
		exit 1
	fi
	useradd -m -u 1000 -s /bin/bash hq
fi
[ "$(id -u hq)" = 1000 ] || {
	echo "provision: ERROR: hq is not uid 1000" >&2
	exit 1
}
usermod -aG docker hq
install -d -o hq -g hq -m 0755 /var/lib/hardware-queue
install -d -o root -g root -m 0755 /etc/hardware-queue
[ -e /etc/hardware-queue/config.env ] || cat >/etc/hardware-queue/config.env <<'EOF'
# The operator's overrides for the validate/hardware queue (#664). job.sh reads
# scripts/hardware-queue/config.env (main's defaults), then setup.env (written
# by setup.sh), then this file, which setup.sh never rewrites. Restart after
# editing: systemctl restart hardware-queue
EOF

say "flasher-tool at incus-os $HQ_FLASHER_REF"
su - hq -c "/usr/local/go/bin/go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool@$HQ_FLASHER_REF" >/dev/null

say "the job's code"
[ -d "$NEW" ] || {
	echo "provision: ERROR: $NEW is missing; run setup.sh, which pushes it" >&2
	exit 1
}
rm -rf "$CODE.old"
[ ! -d "$CODE" ] || mv "$CODE" "$CODE.old"
mv "$NEW" "$CODE"
rm -rf "$CODE.old"
chown -R root:root "$CODE"
install -m 0644 "$CODE/scripts/hardware-queue/hardware-queue.service" /etc/systemd/system/hardware-queue.service
systemctl daemon-reload
systemctl enable hardware-queue >/dev/null 2>&1
say "done"
