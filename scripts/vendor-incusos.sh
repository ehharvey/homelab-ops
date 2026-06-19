#!/bin/bash
# Regenerates internal/incusos/ from the third_party/incus-os submodule.
#
# internal/incusos holds plain copies of the upstream IncusOS seed structs
# (install.yaml/network.yaml/applications.yaml field definitions) so our
# Go build never depends on incus-osd's module graph (tailscale.com, the
# full incus/v7 tree, etc.) for four small leaf files. The submodule is
# the git-tracked pointer to the exact upstream commit; this script is how
# that commit's structs make their way into our own module.
#
# Run after bumping the submodule (`git -C third_party/incus-os checkout
# <new-sha>`) to pick up the new commit's struct definitions.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

SUBMODULE_DIR="third_party/incus-os"
SRC_API="$SUBMODULE_DIR/incus-osd/api"
DST="internal/incusos"

if [ ! -f "$SRC_API/system_network.go" ]; then
  echo "error: $SRC_API/system_network.go not found." >&2
  echo "Run: git submodule update --init && git -C $SUBMODULE_DIR sparse-checkout init --cone && git -C $SUBMODULE_DIR sparse-checkout set incus-osd/api" >&2
  exit 1
fi

COMMIT=$(git -C "$SUBMODULE_DIR" rev-parse HEAD)

mkdir -p "$DST/api" "$DST/api/seed"

header() {
  local src_path="$1"
  local note="$2"
  cat <<EOF
// Vendored from github.com/lxc/incus-os @ $COMMIT
// ($src_path), Apache-2.0 license — see third_party/incus-os/COPYING.
// $note
// Regenerate via scripts/vendor-incusos.sh; do not hand-edit beyond what
// that script does.

EOF
}

{
  header "incus-osd/api/system_network.go" "Unmodified."
  cat "$SRC_API/system_network.go"
} > "$DST/api/system_network.go"

{
  header "incus-osd/api/doc.go" "Unmodified."
  cat "$SRC_API/doc.go"
} > "$DST/api/doc.go"

{
  header "incus-osd/api/seed/install.go" "Unmodified."
  cat "$SRC_API/seed/install.go"
} > "$DST/api/seed/install.go"

{
  header "incus-osd/api/seed/applications.go" "Unmodified."
  cat "$SRC_API/seed/applications.go"
} > "$DST/api/seed/applications.go"

{
  header "incus-osd/api/seed/doc.go" "Unmodified."
  cat "$SRC_API/seed/doc.go"
} > "$DST/api/seed/doc.go"

{
  header "incus-osd/api/seed/network.go" "Modified: import path rewritten to this module's vendored api package."
  sed 's#github.com/lxc/incus-os/incus-osd/api#github.com/ehharvey/homelab-ops/internal/incusos/api#' "$SRC_API/seed/network.go"
} > "$DST/api/seed/network.go"

cp "$SUBMODULE_DIR/COPYING" "$DST/LICENSE"

gofmt -w "$DST"

echo "Vendored internal/incusos from incus-os @ $COMMIT"
