#!/usr/bin/env bash
# Lints every tracked shell script with shellcheck (#418), run as `make lint-sh`
# and by CI's lint job.
#
# The version is pinned in .shellcheck-version alone, the same way
# .golangci-lint-version pins golangci-lint (#329): the Makefile builds
# SHELLCHECK_IMAGE from it and passes it in, and run standalone this script
# builds the same tag itself. No local shellcheck is needed, only Docker.
#
# The file set comes from git, never a glob, so the worktrees under
# .claude/worktrees/ are never scanned: every tracked *.sh, plus every tracked
# extensionless file whose shebang runs sh or bash (a git hook, say). Gitlinks
# (the third_party/ submodule) and symlinks are skipped.
#
# It runs from the repo root with -x, so the root-relative source= directives
# (scripts/validate/lib.sh, say) resolve, at the default severity: every
# finding fails. Silence a false positive where it is, with a disable=SCnnnn
# directive that gives its reason after a second #, never repo-wide.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

image="${SHELLCHECK_IMAGE:-koalaman/shellcheck:$(tr -d '[:space:]' < .shellcheck-version)}"
shebang='^#![[:space:]]*/(usr/)?bin/(env[[:space:]]+)?(ba)?sh([[:space:]]|$)'

files=()
# Each entry is "<mode> <object> <stage>\t<path>".
while IFS= read -r -d '' entry; do
  mode=${entry%% *}
  path=${entry#*$'\t'}
  [[ $mode == 160000 || $mode == 120000 || ! -f $path ]] && continue
  if [[ $path == *.sh ]]; then
    files+=("$path")
    continue
  fi
  first=$(head -c 256 -- "$path" | head -n 1 | tr -d '\0') || true
  [[ $first =~ $shebang ]] && files+=("$path")
done < <(git ls-files -s -z)

if [[ ${#files[@]} -eq 0 ]]; then
  echo "lint-sh: no shell scripts tracked" >&2
  exit 1
fi

echo "lint-sh: $image over ${#files[@]} files"
exec docker run --rm -v "$PWD:/mnt:ro" -w /mnt "$image" -x -- "${files[@]}"
