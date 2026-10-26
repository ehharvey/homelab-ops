#!/bin/bash
# Runs ON THE HOST MACHINE via devcontainer.json's initializeCommand (not inside
# the container), on every window connect, including a plain reload that
# re-attaches to a container that is already running (#374).
#
# If the devcontainer's client cert is still trusted, it does nothing. The
# container-side trust step (.devcontainer/scripts/1-setup-incus-remote.sh)
# caches that cert's fingerprint in .client-fingerprint beside this script, and
# a `devcontainer` trust entry with that fingerprint means there is nothing to
# re-add: no revoke, no token.
#
# Otherwise (first run, a recreated homelab-ops-incus-config volume and so a
# new client cert, or a revoked entry) it removes the stale `devcontainer`
# entries, revokes any still-pending `devcontainer` tokens so they don't pile
# up, and issues a fresh trust token into .trust-token. The container's trust
# step consumes that token on the same connect (postAttachCommand) or at start
# (postStartCommand), registering itself as a trusted remote against the host's
# Incus over https://host.docker.internal:8443.
#
# Unattended and non-destructive to instances, as everything under
# .devcontainer/host-setup/ must be (docs/Decisions.md §21).

set -euo pipefail

TRUST_NAME="devcontainer"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOKEN_FILE="$SCRIPT_DIR/.trust-token"
FINGERPRINT_FILE="$SCRIPT_DIR/.client-fingerprint"

if ! incus info >/dev/null 2>&1; then
  echo "Could not reach the local Incus daemon on this host. Run 'incus admin init' first." >&2
  exit 1
fi

TRUST_JSON="$(incus config trust list -f json)"

# The cache holds the SHA-256 of the client cert's DER bytes as 64 lowercase
# hex digits, the same form as the `fingerprint` field of `trust list -f json`
# (Incus's shared/tls.CertFingerprint).
if [ -f "$FINGERPRINT_FILE" ]; then
  CACHED_FINGERPRINT="$(tr -d '[:space:]' < "$FINGERPRINT_FILE" | tr 'A-F' 'a-f')"
  if [[ "$CACHED_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]] &&
    jq -e --arg name "$TRUST_NAME" --arg fp "$CACHED_FINGERPRINT" \
      'any((. // [])[]; .name == $name and .fingerprint == $fp)' <<<"$TRUST_JSON" >/dev/null; then
    echo "'$TRUST_NAME' client cert ${CACHED_FINGERPRINT:0:12} is still trusted; nothing to do."
    exit 0
  fi
fi

for fingerprint in $(jq -r --arg name "$TRUST_NAME" '(. // [])[] | select(.name == $name) | .fingerprint' <<<"$TRUST_JSON"); do
  incus config trust remove "$fingerprint"
done

# `revoke-token <name>` deletes one pending token per call, so call it once per
# pending `devcontainer` token.
PENDING_TOKENS="$(incus config trust list-tokens -f json | jq -r --arg name "$TRUST_NAME" '[(. // [])[] | select(.client_name == $name)] | length')"
for ((i = 0; i < PENDING_TOKENS; i++)); do
  incus config trust revoke-token "$TRUST_NAME" -q
done

incus config trust add "$TRUST_NAME" -q > "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE"

echo "Trust token (re)issued for '$TRUST_NAME' and written to $TOKEN_FILE"
