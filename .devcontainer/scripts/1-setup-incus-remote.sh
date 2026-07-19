#! /bin/bash

set -e

SCRIPT_DIR="$(dirname "$(realpath "$0")")"

# cd cascade upwards until we find a .git directory
while [ ! -d ".git" ] && [ "$PWD" != "/" ]; do
  cd ..
done

TOKEN_FILE=".devcontainer/host-setup/.trust-token"

if [ ! -f "$TOKEN_FILE" ]; then
  echo "No Incus trust token found at $TOKEN_FILE."
  echo "Run .devcontainer/host-setup/setup-incus-trust.sh on the HOST machine, then reload this devcontainer."
  exit 0
fi

sudo chown "$USER" "$TOKEN_FILE"
sudo chown -R "$USER" "$HOME/.config/incus"

incus remote switch local >/dev/null 2>&1 || true

incus remote remove homelab-host >/dev/null 2>&1 || true
incus remote add homelab-host https://host.docker.internal:8443 --token "$(cat "$TOKEN_FILE")" --accept-certificate
rm -f "$TOKEN_FILE"

incus remote switch homelab-host

echo "Incus remote 'homelab-host' configured."

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
