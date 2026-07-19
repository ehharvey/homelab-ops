#! /bin/bash

set -e

# Same env overrides as the validate-* scripts, and the same defaults — this
# script exists to create what they expect, so the two must not drift.
#
# PROJECT moved from "homelab-dev" to "default" with #132. homelab-dev got stuck
# with features.networks=true and could therefore see no networks at all (#96),
# so every script targeting it failed its prerequisites. #131 deleted it — this
# script must not re-create it.
REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
PROJECT="${VALIDATE_INCUS_PROJECT:-default}"
NETWORK="${VALIDATE_INCUS_NETWORK:-home-lan}"

if ! incus remote list -f csv | grep -q "${REMOTE}"; then
  echo "Incus remote '$REMOTE' not configured yet, skipping dev network setup."
  exit 0
fi

# A no-op for the "default" default, which always exists; still guarded so an
# overridden PROJECT gets created rather than failing. Deliberately no
# `project switch` — that mutated the user's client config as a side effect,
# and is pointless now the target is the project Incus already starts you in.
if ! incus project list "$REMOTE:" -f csv | cut -d, -f1 | sed 's/ (current)$//' | grep -qx "$PROJECT"; then
  incus project create "$REMOTE:$PROJECT"
fi

if ! incus network list "$REMOTE:" --project "$PROJECT" -f csv | cut -d, -f1 | grep -qx "$NETWORK"; then
  incus network create "$REMOTE:$NETWORK" --project "$PROJECT" \
    ipv4.address=192.168.1.1/24 \
    ipv4.nat=true \
    ipv4.dhcp.ranges=192.168.1.2-192.168.1.199 \
    ipv6.address=none
fi

echo "Dev network '$NETWORK' ready in project '$PROJECT' on remote '$REMOTE'."
