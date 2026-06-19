#! /bin/bash
# Validates GH issue #6 ("Dev environment") "Done when" criteria:
#   1. Dev container contains all required packages
#   2. Dev container can interact with host machine incus
#   3. We have simulated our network environment in incus
#
# Intended to run INSIDE the devcontainer.

set -uo pipefail

REMOTE="homelab-host"
PROJECT="homelab-dev"
NETWORK="home-lan"

pass=0
fail=0

check() {
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    echo "PASS: $desc"
    pass=$((pass + 1))
  else
    echo "FAIL: $desc"
    fail=$((fail + 1))
  fi
}

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

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
