#! /bin/bash

set -e

REMOTE="homelab-host"
PROJECT="homelab-dev"
NETWORK="home-lan"

if ! incus remote list -f csv | grep -q "${REMOTE}"; then
  echo "Incus remote '$REMOTE' not configured yet, skipping dev network setup."
  exit 0
fi

if ! incus project list "$REMOTE:" -f csv | cut -d, -f1 | grep -qx "$PROJECT"; then
  incus project create "$REMOTE:$PROJECT"
  incus project switch "$REMOTE:$PROJECT"
fi

if ! incus network list "$REMOTE:" --project "$PROJECT" -f csv | cut -d, -f1 | grep -qx "$NETWORK"; then
  incus network create "$REMOTE:$NETWORK" --project "$PROJECT" \
    ipv4.address=192.168.1.1/24 \
    ipv4.nat=true \
    ipv4.dhcp.ranges=192.168.1.2-192.168.1.199 \
    ipv6.address=none
fi

echo "Dev network '$NETWORK' ready in project '$PROJECT' on remote '$REMOTE'."
