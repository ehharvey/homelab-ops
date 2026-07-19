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
# shellcheck source=scripts/validate/lib-incus.sh
. "$ROOT_DIR/scripts/validate/lib-incus.sh"

VALIDATE_PROVES="the devcontainer has its tooling and can drive the host's Incus (#6)"
VALIDATE_GROUP="incus"
VALIDATE_NEEDS="an Incus remote, jq"
VALIDATE_DURATION="~1s"

validate_parse_args "$@"

# Note the tool checks below stay `check`, not `require_cmd`: for this script
# "is incus installed" IS the assertion, not a precondition for one. Everywhere
# else in the suite it's the reverse.

# Overridable so this can run somewhere other than the devcontainer — notably
# on the Incus host itself, where Incus is a local unix socket and no remote
# named "homelab-host" exists (see #115's CI design).
#
# PROJECT defaults to "default". This used to target a "homelab-dev" project,
# which got stuck with features.networks=true and could therefore see no
# networks at all, so the "network exists" assertion below could never pass
# against it (#96). #132 repointed the suite at "default", where home-lan
# lives; #131 deleted homelab-dev outright.
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

# Skew is silent until it isn't: #131 found client 7.1 against server 6.0.4,
# which surfaced only as a stray "Can't specify column L when not clustered".
# Printed unconditionally so in-major drift stays visible without being fatal.
echo "   client $(incus_client_version), server $(incus_server_version) on '$REMOTE'"
check "incus client and server share a major version" incus_versions_compatible

echo
echo "== 3. Simulated network environment in incus =="
check "project '$PROJECT' exists" bash -c "incus project list '$REMOTE:' -f csv | cut -d, -f1 | sed 's/ (current)\$//' | grep -qx '$PROJECT'"
check "network '$NETWORK' exists" bash -c "incus network list '$REMOTE:' --project '$PROJECT' -f csv | cut -d, -f1 | sed 's/ (current)\$//' | grep -qx '$NETWORK'"

summary
