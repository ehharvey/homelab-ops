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

# The pinned Alpine base images (#131).
#
# The suite used to launch "images:alpine/edge" directly at nine call sites.
# "edge" is a moving tag: it changes upstream, and the local copy expires on
# the default 10-day images.remote_cache_expiry — so the suite both
# re-downloaded periodically and silently ran against a different base image
# over time, a latent flakiness source nobody would attribute correctly. Not
# hypothetical: #131 recorded fingerprint 19237dd97601 (20260716_13:00) and
# three days later the host had 20260718_13:00 under a different fingerprint.
#
# Two aliases, not one, because a container image and a VM image are separate
# images with separate fingerprints — an alias resolves to exactly one, so
# `incus launch <alias> --vm` needs its own. Created by
# .devcontainer/scripts/3-pin-validate-images.sh; asserted by
# require_incus_image.
#
# Refreshing the pin is deliberately manual (delete the alias, re-copy) rather
# than --auto-update, which would reintroduce exactly the drift being fixed.
VALIDATE_ALPINE_CT="${VALIDATE_ALPINE_CT:-validate-alpine}"
VALIDATE_ALPINE_VM="${VALIDATE_ALPINE_VM:-validate-alpine-vm}"

# incus_client_version / incus_server_version — the two halves of the skew
# check. The server one needs the leading slash in the query path; without it
# Incus errors with "Query path must start with /".
incus_client_version() {
	incus --version 2>/dev/null
}

incus_server_version() {
	incus query "$REMOTE:/1.0" 2>/dev/null | jq -r '.environment.server_version'
}

# incus_versions_compatible — 0 if client and server share a MAJOR version.
#
# Major-only is the deliberate contract (#131). The devcontainer installs the
# client from zabbly `stable` and rebuilds independently of the host, so client
# 7.2 against server 7.1 is normal and harmless. A check that fails on that is
# one people learn to ignore, which is worse than no check at all. The defect
# worth catching is a major-line split — #131 found client 7.1 against server
# 6.0.4, which produced no error, just a stray "Can't specify column L when not
# clustered" that nobody would trace back to a version skew.
incus_versions_compatible() {
	local client server
	client="$(incus_client_version | cut -d. -f1)"
	server="$(incus_server_version | cut -d. -f1)"
	[ -n "$client" ] && [ -n "$server" ] && [ "$client" = "$server" ]
}

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
