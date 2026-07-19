#! /bin/bash
# Validates GH issue #6 ("Dev environment") "Done when" criteria:
#   1. Dev container contains all required packages
#   2. Dev container can interact with host machine incus
#   3. We have simulated our network environment in incus
#
# Intended to run INSIDE the devcontainer.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="the devcontainer has its tooling and can drive the host's Incus (#6)"
VALIDATE_GROUP="incus"
VALIDATE_NEEDS="an Incus remote"
VALIDATE_DURATION="~1s"

validate_parse_args "$@"

# Note the tool checks below stay `check`, not `require_cmd`: for this script
# "is incus installed" IS the assertion, not a precondition for one. Everywhere
# else in the suite it's the reverse.

# Overridable so this can run somewhere other than the devcontainer — notably
# on the Incus host itself, where Incus is a local unix socket and no remote
# named "homelab-host" exists (see #115's CI design).
#
# PROJECT defaults to "default", not "homelab-dev": that project is stuck with
# features.networks=true and therefore sees no networks at all, so this script's
# "network exists" assertion could never pass against it (#96, #131). #91 already
# targets "default" for the same reason, and home-lan lives there.
REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
PROJECT="${VALIDATE_INCUS_PROJECT:-default}"
NETWORK="${VALIDATE_INCUS_NETWORK:-home-lan}"

echo "== 1. Required packages =="
check "incus client installed" command -v incus
check "opentofu installed" command -v tofu
check "docker installed" command -v docker
check "gh cli installed" command -v gh
check "go installed" command -v go

echo
echo "== 2. Dev container can interact with host incus =="
check "incus remote '$REMOTE' configured" bash -c "incus remote list -f csv | cut -d, -f1 | sed 's/ (current)\$//' | grep -qx '$REMOTE'"
check "incus remote '$REMOTE' reachable" incus info "$REMOTE:"

echo
echo "== 3. Simulated network environment in incus =="
check "project '$PROJECT' exists" bash -c "incus project list '$REMOTE:' -f csv | cut -d, -f1 | sed 's/ (current)\$//' | grep -qx '$PROJECT'"
check "network '$NETWORK' exists" bash -c "incus network list '$REMOTE:' --project '$PROJECT' -f csv | cut -d, -f1 | sed 's/ (current)\$//' | grep -qx '$NETWORK'"

summary
