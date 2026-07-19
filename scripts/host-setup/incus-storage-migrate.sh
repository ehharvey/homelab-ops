#! /bin/bash
# Migrates the dev Incus host's storage pool to a copy-on-write driver (#131).
#
# WHY: the pool was created by `incus admin init` accepting defaults, which
# gives driver `dir` — no copy-on-write, so every `incus launch` copies the
# whole rootfs instead of cloning it. #115 launches a throwaway sandbox per CI
# run from a baked base image, which is the workload that cares.
#
# Measured on this host either side of the migration, 1.4GB incompressible
# image (`dir` on server 6.0.4, `btrfs` on 7.0.1 — see docs/Decisions.md §21
# for why that version difference makes the launch row only loosely
# attributable):
#   launch from image      3,188 ms -> 1,288 ms   (2.5x)
#   copy instance         13,675 ms ->   531 ms   (26x)
# Worth knowing the honest magnitude: seconds, not minutes. #131 speculated a
# `dir` launch might cost "more than the image pulls it exists to avoid" — at
# ~3s that was not true. The real win is instance cloning going near-free.
#
# NOT auto-run, and deliberately not in .devcontainer/host-setup/ — everything
# there runs unattended via initializeCommand, and a step that destroys every
# instance on the host must never be on that path. Run it by hand, once.
#
# THIS DESTROYS EVERY INSTANCE, CUSTOM VOLUME AND IMAGE ON THE POOL. A pool's
# driver cannot be changed in place; the pool has to be deleted and recreated,
# and everything stored on it goes with it. Re-run
# .devcontainer/scripts/3-pin-validate-images.sh afterwards to restore the
# pinned base images.

set -euo pipefail

REMOTE="${VALIDATE_INCUS_REMOTE:-homelab-host}"
POOL="${VALIDATE_INCUS_POOL:-default}"
DRIVER="${INCUS_POOL_DRIVER:-btrfs}"
SIZE="${INCUS_POOL_SIZE:-100GiB}"

if [ "${1:-}" != "--yes-destroy-everything" ]; then
  cat >&2 <<EOF
usage: $0 --yes-destroy-everything

Recreates the '$POOL' storage pool on remote '$REMOTE' with driver '$DRIVER'
(size $SIZE). This DELETES every instance, custom volume and image stored on
it, across all projects. There is no backup and no undo.

Override with VALIDATE_INCUS_REMOTE / VALIDATE_INCUS_POOL / INCUS_POOL_DRIVER
/ INCUS_POOL_SIZE.
EOF
  exit 64
fi

if ! incus info "$REMOTE:" >/dev/null 2>&1; then
  echo "ERROR: incus remote '$REMOTE' not reachable" >&2
  exit 2
fi

# Not `incus storage get <pool> driver`: `get` reads config keys, and driver
# isn't one — it returns empty with exit 0, which reads as "pool missing".
current_driver="$(incus storage list "$REMOTE:" -f csv 2>/dev/null | awk -F, -v p="$POOL" '$1 == p {print $2}')"
if [ -z "$current_driver" ]; then
  echo "ERROR: storage pool '$POOL' not found on '$REMOTE'" >&2
  exit 2
fi

# Assert, don't act: a second run on an already-migrated host is a no-op, not
# a second round of destruction.
if [ "$current_driver" = "$DRIVER" ]; then
  echo "Pool '$POOL' on '$REMOTE' is already driver '$DRIVER' — nothing to do."
  exit 0
fi

# Pre-flight: the target driver must actually be available BEFORE anything is
# destroyed. Incus enumerates storage drivers from the userspace tools present
# on the host, so a server with no btrfs-progs installed reports only `dir` —
# and this script's order (clear everything, then recreate the pool) would
# otherwise delete every instance, volume, image and profile root device and
# only then fail at `storage create`, leaving the host with no pool at all.
# Caught exactly that on this host: server 7.0.1 supporting `dir` alone.
if ! incus query "$REMOTE:/1.0" |
  jq -e --arg d "$DRIVER" '.environment.storage_supported_drivers[]? | select(.Name == $d)' >/dev/null 2>&1; then
  supported="$(incus query "$REMOTE:/1.0" | jq -r '[.environment.storage_supported_drivers[]?.Name] | join(", ")')"
  cat >&2 <<EOF
ERROR: server on '$REMOTE' does not support storage driver '$DRIVER'.
       Supported: ${supported:-<none reported>}

Incus derives this from the tools installed on the host. For btrfs:

    sudo apt install btrfs-progs
    sudo systemctl restart incus

then re-run this script. Nothing has been changed.
EOF
  exit 2
fi

echo "Pool '$POOL' on '$REMOTE' is driver '$current_driver'; migrating to '$DRIVER'."
echo

projects="$(incus project list "$REMOTE:" -f csv | cut -d, -f1 | sed 's/ (current)$//')"

echo "== deleting instances =="
for p in $projects; do
  for i in $(incus list "$REMOTE:" --project "$p" -f csv -c n); do
    echo "  - $p/$i"
    incus delete --force --project "$p" "$REMOTE:$i"
  done
done

echo "== deleting custom volumes =="
for p in $projects; do
  # -c t gives the volume type; only custom volumes are ours to remove, the
  # container/virtual-machine ones went with their instances above.
  incus storage volume list "$REMOTE:$POOL" --project "$p" -f csv -c tn |
    awk -F, '$1 == "custom" {print $2}' |
    while read -r v; do
      echo "  - $p/$v"
      incus storage volume delete "$REMOTE:$POOL" "$v" --project "$p"
    done
done

echo "== deleting images (they live on the pool; re-pin afterwards) =="
for p in $projects; do
  for f in $(incus image list "$REMOTE:" --project "$p" -f csv -c f); do
    echo "  - $p/$f"
    incus image delete "$REMOTE:$f" --project "$p"
  done
done

# Each project's default profile pins a root disk to the pool, and Incus won't
# delete a pool while a profile references it. Record which projects had one so
# they can be restored identically afterwards.
echo "== detaching root disks from default profiles =="
restore_projects=""
for p in $projects; do
  if [ "$(incus profile device get "$REMOTE:default" root pool --project "$p" 2>/dev/null || true)" = "$POOL" ]; then
    echo "  - $p"
    incus profile device remove "$REMOTE:default" root --project "$p"
    restore_projects="$restore_projects $p"
  fi
done

echo "== recreating pool =="
incus storage delete "$REMOTE:$POOL"
incus storage create "$REMOTE:$POOL" "$DRIVER" size="$SIZE"

echo "== reattaching root disks =="
for p in $restore_projects; do
  echo "  - $p"
  incus profile device add "$REMOTE:default" root disk pool="$POOL" path=/ --project "$p"
done

echo
incus storage info "$REMOTE:$POOL"
echo
echo "Done. Pool '$POOL' is now driver '$DRIVER'."
echo "Next: .devcontainer/scripts/3-pin-validate-images.sh  (restores the pinned base images)"
