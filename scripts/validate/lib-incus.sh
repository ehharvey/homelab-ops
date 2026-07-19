#! /bin/bash
# Incus helpers for the validate suite (#140). Source after lib.sh.
#
# Sourced by the scripts whose VALIDATE_GROUP is "incus" or "incus-vm" — the
# ones that drive a real Incus remote, and in the VM case boot a real node off
# a seeded .img.
#
# These read $REMOTE and $PROJECT, which every such script declares via the
# VALIDATE_INCUS_* overrides (#132) so the suite can also run on the Incus host
# itself, where Incus is a local unix socket and no "homelab-host" remote
# exists.

# console_log — the VM's console output, NUL bytes stripped.
#
# Stripping is not cosmetic: the console log is full of them, and grep treats a
# file containing NULs as binary and silently reports "binary file matches"
# instead of the line you wanted.
console_log() {
	incus console --project "$PROJECT" "$REMOTE:$VM_NAME" --show-log 2>&1 | tr -d '\0'
}

# wait_for_console_text <needle> <timeout_s> — poll the console log until the
# fixed string appears. Returns 1 on timeout so the caller can decide whether
# that's a FAIL or a SKIP.
#
# Polling the console is the only way in: a freshly-installing node has no
# network, no SSH, and no Incus API yet, so there is nothing else to ask.
wait_for_console_text() {
	local needle="$1" timeout_s="$2" waited=0
	while [ "$waited" -lt "$timeout_s" ]; do
		if console_log | grep -qF "$needle"; then
			return 0
		fi
		sleep 5
		waited=$((waited + 5))
	done
	return 1
}

# incus_exec_bg <logfile> <incus-exec-args...> — run a command inside an
# instance in the background, logging to logfile.
#
# Backgrounds the `incus exec` client process on this side rather than
# nohup/disown inside the container, so the process dies with us and there is
# no orphan left behind on the remote. Used to run the real `web` binary inside
# a container for the duration of a test; these aren't container-level services,
# so cleanup is our job via cleanup_bg_pids.
BG_PIDS=()
incus_exec_bg() {
	local logfile="$1"
	shift
	incus exec --project "$PROJECT" "$@" >"$logfile" 2>&1 &
	BG_PIDS+=("$!")
}

cleanup_bg_pids() {
	local pid
	for pid in "${BG_PIDS[@]:-}"; do
		kill "$pid" >/dev/null 2>&1
	done
}

# require_flasher_tool — hard prerequisite for anything calling `build-image`.
#
# Exists because of #136: build-image shells out to flasher-tool, an
# operator-installed binary the repo doesn't vendor. Without this check its
# absence produced five cascading failures reading like "the VM never booted and
# cert trust is broken", while the CLI's own error said exactly what was wrong
# before `check` discarded it.
require_flasher_tool() {
	command -v flasher-tool >/dev/null 2>&1 ||
		_prereq_missing "flasher-tool not found; install via 'go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool@latest' (see #68 re: pinning)"
}

# incus_instance_exists <name> — used by cleanup paths that must not fail when
# an instance was never created.
incus_instance_exists() {
	incus info --project "$PROJECT" "$REMOTE:$1" >/dev/null 2>&1
}
