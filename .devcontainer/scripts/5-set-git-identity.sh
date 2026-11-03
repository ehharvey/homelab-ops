#! /bin/bash
# Sets the repo's commit identity to GitHub's noreply address (#451), so no
# commit made here carries the personal address main's history was rewritten
# to remove. .githooks/pre-push and the `one-commit` CI job refuse a commit
# that has any other (scripts/check-commit-identity.sh); this makes the default
# right. postStartCommand runs it with the rest of this directory.
set -euo pipefail

DEVCONTAINER_FILE=$(find /workspaces/ -name "devcontainer.json" -type f -print -quit)

if [ -z "$DEVCONTAINER_FILE" ]; then
    echo "devcontainer.json not found in /workspaces"
    exit 1
fi

# .../<repo>/.devcontainer/devcontainer.json -> <repo>
REPO_ROOT=$(dirname "$(dirname "$DEVCONTAINER_FILE")")

# The shared repo config, so every worktree under .claude/worktrees/ gets it
# too. Repo-local outranks ~/.gitconfig, which the container may mount from
# the host with another address in it.
git -C "$REPO_ROOT" config user.name "Emil Harvey"
git -C "$REPO_ROOT" config user.email "36806437+ehharvey@users.noreply.github.com"

echo "user.name and user.email set to Emil Harvey <36806437+ehharvey@users.noreply.github.com> in $REPO_ROOT"
