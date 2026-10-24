#! /bin/bash
# Installs the goimports that `make fmt` runs (#262). Without it, `make fmt`
# stops with exit 127 after gofmt, so import grouping goes unenforced until
# `make lint` (whose goimports formatter is on in .golangci.yml).
#
# Pinned, never @latest: the version lives only in GOIMPORTS_VERSION below, the
# same one-place rule as LINT_IMAGE in the Makefile. To bump it, change it here
# and rebuild (or re-run this script).
#
# Runs on every container start, so it is idempotent: it skips the install when
# the binary on disk already reports the pinned module version. An offline
# start warns and exits 0, because postStartCommand chains the scripts with &&
# and a failure here would skip the ones after it.
set -euo pipefail

GOIMPORTS_VERSION="v0.52.0"
MODULE="golang.org/x/tools"

BIN="$(go env GOPATH)/bin/goimports"

if [ -x "$BIN" ] && go version -m "$BIN" 2>/dev/null |
	awk -v m="$MODULE" -v v="$GOIMPORTS_VERSION" '$1 == "mod" && $2 == m && $3 == v { found = 1 } END { exit !found }'; then
	echo "goimports ${GOIMPORTS_VERSION} already installed at ${BIN}"
	exit 0
fi

echo "Installing goimports ${GOIMPORTS_VERSION}..."
# `go install pkg@version` ignores the repo's go.mod; GOFLAGS is cleared so a
# stray -mod=vendor from the environment cannot break it.
if ! GOFLAGS= go install "${MODULE}/cmd/goimports@${GOIMPORTS_VERSION}"; then
	echo "WARNING: could not install goimports ${GOIMPORTS_VERSION} (offline?)." >&2
	echo "         'make fmt' needs it; re-run .devcontainer/scripts/3-install-goimports.sh once online." >&2
	exit 0
fi

echo "goimports ${GOIMPORTS_VERSION} installed at ${BIN}"
