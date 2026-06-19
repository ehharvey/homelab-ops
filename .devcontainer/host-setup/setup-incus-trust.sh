#!/bin/bash
# Runs ON THE HOST MACHINE via devcontainer.json's initializeCommand (not inside
# the container). Idempotently (re)issues an Incus trust token for the
# devcontainer and drops it into this repo's working tree, where the
# container-side .devcontainer/scripts/1-setup-incus-remote.sh picks it up to
# register/refresh itself as a trusted remote against the host's Incus over
# https://host.docker.internal:8443.

set -euo pipefail

TRUST_NAME="devcontainer"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOKEN_FILE="$SCRIPT_DIR/.trust-token"

if ! incus info >/dev/null 2>&1; then
  echo "Could not reach the local Incus daemon on this host. Run 'incus admin init' first." >&2
  exit 1
fi

for fingerprint in $(incus config trust list -f json | jq -r --arg name "$TRUST_NAME" '.[] | select(.name == $name) | .fingerprint'); do
  incus config trust remove "$fingerprint"
done

incus config trust add "$TRUST_NAME" -q > "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE"

echo "Trust token (re)issued for '$TRUST_NAME' and written to $TOKEN_FILE"
