#! /bin/bash
# Points the repo at .githooks/ so the pre-push one-commit check (#119) is
# active in a fresh devcontainer without anyone remembering to run `make hooks`.
set -euo pipefail

DEVCONTAINER_FILE=$(find /workspaces/ -name "devcontainer.json" -type f -print -quit)

if [ -z "$DEVCONTAINER_FILE" ]; then
    echo "devcontainer.json not found in /workspaces"
    exit 1
fi

# .../<repo>/.devcontainer/devcontainer.json -> <repo>
REPO_ROOT=$(dirname "$(dirname "$DEVCONTAINER_FILE")")

# Relative on purpose: core.hooksPath lives in the shared repo config, and a
# relative value resolves against each worktree's own root — so this one call
# also covers every checkout under .claude/worktrees/. An absolute path would
# aim them all at the main checkout's hooks.
git -C "$REPO_ROOT" config core.hooksPath .githooks

echo "core.hooksPath set to .githooks in $REPO_ROOT"
