#! /bin/bash

set -e

# Pins the Alpine base image the VM-family validate scripts launch from (#131).
#
# Same env overrides as the validate-* scripts, and the same defaults — like
# 2-setup-dev-network.sh, this exists to create what they expect, so the two
# must not drift.
#
# Why pin at all: the suite used to launch "images:alpine/edge" directly at
# nine call sites. "edge" is a moving tag, and the local copy expires on the
# default 10-day images.remote_cache_expiry, so the suite both re-downloaded
# periodically and silently ran against a drifting base image — a flakiness
# source nobody would attribute correctly. #131 recorded fingerprint
# 19237dd97601 (20260716_13:00); three days later the host had 20260718_13:00
# under a different fingerprint.
#
# Two aliases, not one: a container image and a VM image are separate images
# with separate fingerprints, and an alias resolves to exactly one — so
# `incus launch <alias> --vm` needs its own.
#
# Deliberately no --auto-update: that would reintroduce the drift being fixed.
# Refreshing is a conscious act — delete the alias and re-run this script.
REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
PROJECT="${VALIDATE_INCUS_PROJECT:-default}"
ALPINE_CT="${VALIDATE_ALPINE_CT:-validate-alpine}"
ALPINE_VM="${VALIDATE_ALPINE_VM:-validate-alpine-vm}"
SOURCE="${VALIDATE_ALPINE_SOURCE:-images:alpine/edge}"

if ! incus remote list -f csv | grep -q "${REMOTE}"; then
  echo "Incus remote '$REMOTE' not configured yet, skipping image pin."
  exit 0
fi

has_alias() {
  incus image list "$REMOTE:" --project "$PROJECT" -c l -f csv |
    cut -d, -f1 | grep -qx "$1"
}

if ! has_alias "$ALPINE_CT"; then
  echo "Pinning $SOURCE -> $ALPINE_CT (container) ..."
  incus image copy "$SOURCE" "$REMOTE:" --alias "$ALPINE_CT" --project "$PROJECT"
fi

if ! has_alias "$ALPINE_VM"; then
  echo "Pinning $SOURCE -> $ALPINE_VM (virtual machine) ..."
  incus image copy "$SOURCE" "$REMOTE:" --alias "$ALPINE_VM" --vm --project "$PROJECT"
fi

echo "Pinned base images '$ALPINE_CT' and '$ALPINE_VM' ready in project '$PROJECT' on remote '$REMOTE'."
