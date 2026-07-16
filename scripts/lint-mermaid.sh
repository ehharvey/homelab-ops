#! /bin/bash
# Parses every ```mermaid block in the given markdown files (default: docs/*.md)
# and fails if any of them doesn't parse.
#
# Why this exists: docs/AppManager.md's self-upgrade sequence diagram never
# rendered on GitHub between #105 (which added it) and #111 (which fixed it) —
# GitHub showed "Unable to render rich display / Parse error" in its place. A
# `;` is a statement separator in mermaid, so a Note's text split in two and the
# remainder parsed as an actor name. #106 was opened specifically to correct
# that same diagram's *logic*, so it was read and reviewed while silently not
# displaying at all: reviewing a diagram's source in a PR diff tells you nothing
# about whether it renders. See #112.
#
# Runs the official mermaid-cli image rather than adding node/npm to this Go
# repo — same "pinned tool image via Docker, no local toolchain" shape as
# `make lint`'s golangci-lint, so local and CI behave identically.
#
# `mmdc -i <file.md>` walks a markdown file's mermaid blocks itself and exits
# non-zero on a parse error, reporting mermaid's own line numbers. Deliberately
# not hand-extracting the fences: that would mean reimplementing the block
# scanner and renumbering errors back to the source file.
#
# Usage:
#   scripts/lint-mermaid.sh                 # all of docs/*.md
#   scripts/lint-mermaid.sh path/to/one.md  # specific files
set -euo pipefail

# Pinned, like Makefile's LINT_IMAGE — an unpinned mermaid would let a grammar
# change break CI on a commit that touched no docs.
MERMAID_IMAGE="${MERMAID_IMAGE:-ghcr.io/mermaid-js/mermaid-cli/mermaid-cli:11.4.2}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if ! docker info >/dev/null 2>&1; then
  echo "lint-mermaid: docker is required (same as 'make lint')" >&2
  exit 2
fi

files=("$@")
if [ ${#files[@]} -eq 0 ]; then
  # Nothing but docs/*.md today; kept as a glob so a new doc is covered with no
  # edit here.
  mapfile -t files < <(find docs -maxdepth 1 -name '*.md' | sort)
fi

# mmdc writes the rendered markdown + one .svg per diagram. We only care about
# the exit status, so send the artifacts somewhere disposable.
outdir="$(mktemp -d)"
trap 'rm -rf "$outdir"' EXIT

checked=0
failed=0

for f in "${files[@]}"; do
  # Skip files with no diagrams: mmdc would still spin up a browser to copy the
  # file through, which is the slowest part of this script.
  if ! grep -q '^```mermaid' "$f"; then
    continue
  fi
  checked=$((checked + 1))

  log="$outdir/$(basename "$f").log"
  # -u keeps the rendered artifacts owned by the caller rather than root.
  if docker run --rm \
      -u "$(id -u):$(id -g)" \
      -v "$repo_root:/data" \
      -v "$outdir:/out" \
      -w /data \
      "$MERMAID_IMAGE" \
      -i "/data/$f" -o "/out/$(basename "$f")" \
      >"$log" 2>&1; then
    echo "  ok    $f"
  else
    failed=$((failed + 1))
    echo "  FAIL  $f"
    # mmdc reports the line number *relative to the failing block*, and doesn't
    # say which block that was — so print where each block starts and let the
    # reader add the offset. Cheaper than extracting every fence to its own file
    # just to renumber the errors.
    fences="$(grep -n '^```mermaid' "$f" | cut -d: -f1 | tr '\n' ' ')"
    echo "        mermaid blocks in this file open at line(s): ${fences% }"
    echo "        (the error's line number below is relative to its own block)"
    # Keep mermaid's message, caret and expected-token list; drop the puppeteer
    # stack and the browser's startup chatter, which say nothing about the doc.
    grep -vE '^\[@zenuml/core\]|^[[:space:]]*at |^Parser3\.parseError|^Found [0-9]+ mermaid charts' "$log" \
      | sed '/^[[:space:]]*$/d' \
      | sed 's/^/        /'
  fi
done

echo
if [ "$failed" -gt 0 ]; then
  echo "lint-mermaid: $failed of $checked file(s) have a diagram that does not parse."
  echo "GitHub renders these with the same grammar — a parse error means the page"
  echo "shows an error box instead of the diagram."
  exit 1
fi

echo "lint-mermaid: $checked file(s) checked, all diagrams parse."
