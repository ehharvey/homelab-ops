#! /bin/bash

set -e

SCRIPT_DIR="$(dirname "$(realpath "$0")")"

# cd cascade upwards until we find a .git directory
while [ ! -d ".git" ] && [ "$PWD" != "/" ]; do
  cd ..
done

# One more cd to get to parent of repo
cd ..

sudo mkdir -p wiki
sudo chown -R $(whoami):$(whoami) wiki

# Check if wiki is already cloned
if [ -d "wiki/.git" ]; then
  echo "Wiki already cloned, skipping."
  exit 0
fi

git clone https://github.com/ehharvey/homelab-ops.wiki.git wiki