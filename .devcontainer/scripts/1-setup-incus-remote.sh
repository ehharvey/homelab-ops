#! /bin/bash

set -e

# The trust step (#374) runs on every attach (devcontainer.json's
# postAttachCommand, with --trust-only) as well as at start (postStartCommand,
# with everything else). A fresh start therefore runs it twice; the second run
# finds the remote already `trusted` and only refreshes the fingerprint cache.
TRUST_ONLY=false
if [ "${1:-}" = "--trust-only" ]; then
  TRUST_ONLY=true
fi

# cd cascade upwards until we find a .git directory
while [ ! -d ".git" ] && [ "$PWD" != "/" ]; do
  cd ..
done

TOKEN_FILE=".devcontainer/host-setup/.trust-token"
FINGERPRINT_FILE=".devcontainer/host-setup/.client-fingerprint"
CLIENT_CERT="$HOME/.config/incus/client.crt"

# Cache the client cert's fingerprint where the host-side
# setup-incus-trust.sh (initializeCommand) can read it: while a `devcontainer`
# trust entry has this fingerprint, the host skips its revoke-and-reissue. The
# form matches Incus's own (shared/tls.CertFingerprint, the `fingerprint` field
# of `incus config trust list -f json`): SHA-256 of the DER bytes, 64 lowercase
# hex digits. Never fatal under `set -e`: if the fingerprint can't be computed
# or written, drop the cache so the host falls back to issuing a token.
write_client_fingerprint() {
  local fingerprint
  fingerprint="$(set -o pipefail; openssl x509 -in "$CLIENT_CERT" -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1 | tr 'A-F' 'a-f')" || fingerprint=""
  if [[ "$fingerprint" =~ ^[0-9a-f]{64}$ ]] &&
    printf '%s\n' "$fingerprint" 2>/dev/null > "$FINGERPRINT_FILE.tmp" &&
    mv -f "$FINGERPRINT_FILE.tmp" "$FINGERPRINT_FILE" 2>/dev/null; then
    return 0
  fi
  echo "WARNING: could not cache the Incus client cert fingerprint in $FINGERPRINT_FILE;" \
    "the host will reissue a trust token on the next connect." >&2
  rm -f "$FINGERPRINT_FILE.tmp" "$FINGERPRINT_FILE" 2>/dev/null || true
}

sudo chown -R "$USER" "$HOME/.config/incus"

# Probe before looking for a token: on a reload (or the second run of a fresh
# start) the remote is already trusted and there is nothing to consume.
# `|| true`: an unreachable host or a missing remote must not abort the start.
AUTH="$(incus query homelab-host:/1.0 2>/dev/null | jq -r '.auth' 2>/dev/null || true)"

if [ "$AUTH" = "trusted" ]; then
  # A token the host issued anyway (e.g. the cache was missing) is now moot.
  rm -f "$TOKEN_FILE"
  write_client_fingerprint
  echo "Incus remote 'homelab-host' already trusted."
else
  if [ ! -f "$TOKEN_FILE" ]; then
    echo "No Incus trust token found at $TOKEN_FILE."
    echo "Run .devcontainer/host-setup/setup-incus-trust.sh on the HOST machine, then reload this devcontainer."
    exit 0
  fi

  sudo chown "$USER" "$TOKEN_FILE"

  incus remote switch local >/dev/null 2>&1 || true

  incus remote remove homelab-host >/dev/null 2>&1 || true
  incus remote add homelab-host https://host.docker.internal:8443 --token "$(cat "$TOKEN_FILE")" --accept-certificate
  rm -f "$TOKEN_FILE"
  write_client_fingerprint
fi

incus remote switch homelab-host

echo "Incus remote 'homelab-host' configured."

if [ "$TRUST_ONLY" = true ]; then
  exit 0
fi

# Warn early on a client/server major-version split (#131). Surfacing it here
# is far cheaper than discovering it mid-run: the 7.1-vs-6.0.4 skew #131 found
# raised no error at all, just a stray "Can't specify column L when not
# clustered" that nobody would trace back to a version mismatch.
#
# Warning only, never fatal — this runs under `set -e` on postStartCommand, and
# a devcontainer that refuses to start because the host is mid-upgrade is a bad
# trade. Hence the `|| true` guard on the probe.
#
# The comparison is duplicated from scripts/validate/lib-incus.sh rather than
# sourced: pulling in the validate harness (lib.sh + lib-incus.sh) to get four
# lines would couple devcontainer bring-up to the test suite. Major-only, for
# the reason documented there — the client tracks zabbly `stable` and rebuilds
# independently of the host, so in-major drift is normal and must not warn.
CLIENT_MAJOR="$(incus --version 2>/dev/null | cut -d. -f1 || true)"
SERVER_MAJOR="$(incus query homelab-host:/1.0 2>/dev/null | jq -r '.environment.server_version' 2>/dev/null | cut -d. -f1 || true)"

if [ -n "$CLIENT_MAJOR" ] && [ -n "$SERVER_MAJOR" ] && [ "$CLIENT_MAJOR" != "$SERVER_MAJOR" ]; then
  echo
  echo "WARNING: Incus client/server major-version skew — client $(incus --version), server $(incus query homelab-host:/1.0 | jq -r '.environment.server_version')."
  echo "         Upgrade the host's Incus, or point .devcontainer/Dockerfile at a matching zabbly line."
  echo "         'scripts/validate/run.sh --group incus' asserts this and will fail until it's fixed."
  echo
fi
