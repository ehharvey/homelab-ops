#! /bin/bash
# Points the repo at .githooks/ so the pre-push one-commit check (#119) is
# active in a fresh devcontainer without anyone remembering to run `make hooks`,
# and sets commit.cleanup=scissors as `make hooks` does (#334).
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
# also covers every checkout under .claude/worktrees/. It doesn't stay relative:
# Claude Code's worktree setup rewrites it to the main checkout's absolute
# .githooks (#279), which aims every worktree at main's hooks until the next
# container start resets it here. .githooks/run-own-copy.sh covers that, as long
# as the main checkout's copy of the hook has it, by handing off to the
# worktree's own copy.
git -C "$REPO_ROOT" config core.hooksPath .githooks

# The default `strip` cleanup deletes every `#`-leading line of a message git
# opens for editing, `## Plan` and `## Test plan` included (#334). Scissors
# keeps them and still cuts git's own help text. Shared config again, so every
# worktree gets it.
git -C "$REPO_ROOT" config commit.cleanup scissors

echo "core.hooksPath set to .githooks and commit.cleanup to scissors in $REPO_ROOT"
