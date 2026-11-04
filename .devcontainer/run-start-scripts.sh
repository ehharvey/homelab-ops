#! /bin/bash
# Runs every .devcontainer/scripts/*.sh on container start (devcontainer.json's
# postStartCommand), then prints which of them failed (#328).
#
# Each script runs whatever the earlier ones returned, as the old one-liner
# did. What the one-liner lost was the outcome: `for f in ...; do $f; done`
# kept only the last script's exit status, so any other script could fail with
# no trace beyond its own output, and a failing last one skipped the
# `&& rm -rf` and leaked the temp dir. This records every non-zero exit, always
# removes the temp dir, and exits 1 when any script failed, so VS Code shows a
# failure notice instead of a clean start.
#
# The scripts are copied to a temp dir and made executable there, so a script
# committed without its +x bit still runs and the checkout's file modes are
# left alone. They run from the current directory, which the dev-containers CLI
# sets to the workspace root.
#
# DEVCONTAINER_START_SCRIPTS overrides the source directory, for trying this
# against a copy of scripts/ (e.g. with a deliberately failing script added).
set -u

SRC="${DEVCONTAINER_START_SCRIPTS:-.devcontainer/scripts}"

TMP="$(mktemp -d)" || {
  echo "devcontainer start: could not create a temp dir; no start scripts ran." >&2
  exit 1
}
trap 'rm -rf "$TMP"' EXIT
# bash skips the EXIT trap when a signal it doesn't trap kills it.
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if ! cp -r "$SRC"/. "$TMP"/; then
  echo "devcontainer start: could not copy $SRC; no start scripts ran." >&2
  exit 1
fi

shopt -s nullglob
scripts=("$TMP"/*.sh)
if [ "${#scripts[@]}" -eq 0 ]; then
  echo "devcontainer start: no *.sh in $SRC; nothing to run." >&2
  exit 1
fi
chmod +x "${scripts[@]}"

failed=()
for f in "${scripts[@]}"; do
  name="$(basename "$f")"
  echo "==> $name"
  "$f"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    failed+=("$name: exit $rc")
  fi
done

echo
if [ "${#failed[@]}" -eq 0 ]; then
  echo "devcontainer start: all ${#scripts[@]} start scripts succeeded."
  exit 0
fi

echo "devcontainer start: ${#failed[@]} of ${#scripts[@]} start scripts FAILED:"
for line in "${failed[@]}"; do
  echo "  - $line"
done
exit 1
