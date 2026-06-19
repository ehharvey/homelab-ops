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
