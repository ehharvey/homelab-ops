#! /bin/bash
# Validates GH issue #300: the harness keeps a hung docker credential helper
# from hanging a compose script's pulls and builds.
#
# In the devcontainer, credsStore names VS Code's dev-containers helper, which
# hangs when no VS Code client is attached. This drives lib.sh's
# require_docker_credential_helper (which require_compose calls) against stub
# helpers on PATH and a fixture DOCKER_CONFIG, never the real one:
#   1. A helper that sleeps: one NOTE line naming it, DOCKER_CONFIG exported
#      as a scratch dir whose config.json lacks credsStore but keeps the rest,
#      the config dir's other entries linked in, the fixture config untouched,
#      and the probe bounded by its timeout. The scratch dir is gone once the
#      script that made it exits.
#   2. A helper that answers (not found, like an attached client with nothing
#      stored): nothing printed, DOCKER_CONFIG unchanged.
#   3. A hung helper whose scratch config can't be built (TMPDIR missing):
#      exit 2 through check_prereqs, the ERROR naming the helper.
#   4. No credsStore: no probe, nothing changes.
#
# What it can't prove: that the real dev-containers helper hangs the same way
# headless, or that docker then pulls. #300's Test plan covers that with a run
# of image-route-streams-seeded-image.sh under the hanging stub.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR" || exit 2
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="the harness bypasses a docker credential helper that doesn't answer, before any pull (#300)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="jq, timeout"
VALIDATE_DURATION="~5s"

validate_parse_args "$@"

require_cmd jq timeout
check_prereqs

WORK_DIR="$(mktemp -d)"
cleanup() {
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

BIN="$WORK_DIR/bin"
mkdir -p "$BIN"
# Like the dev-containers helper headless: a shim whose child never answers.
# It records the child's PID, to show the probe's timeout kills it too.
cat >"$BIN/docker-credential-hang" <<EOF
#!/bin/sh
sleep 60 &
echo \$! >"$WORK_DIR/hang.pid"
wait
EOF
cat >"$BIN/docker-credential-answers" <<'EOF'
#!/bin/sh
cat >/dev/null
echo "credentials not found in native keychain"
exit 1
EOF
chmod +x "$BIN"/docker-credential-*

# fixture_config <dir> <credsStore or empty> — a docker config dir with an
# auths entry and a cli-plugins dir, so the scratch copy has something to keep.
fixture_config() {
	local dir="$1" store="$2"
	mkdir -p "$dir/cli-plugins"
	if [ -n "$store" ]; then
		printf '{"auths":{"example.test":{}},"credsStore":"%s"}\n' "$store" >"$dir/config.json"
	else
		printf '{"auths":{"example.test":{}}}\n' >"$dir/config.json"
	fi
}

# probe <docker config dir> [env...] — sources lib.sh in a fresh bash, runs
# require_compose's probe and check_prereqs, then prints DOCKER_CONFIG on its
# last line. Sets OUT (stdout), ERR (stderr), RC and SECS. While that bash is
# still alive, so before the scratch dir's watcher can remove it, it saves what
# DOCKER_CONFIG holds to $WORK_DIR/seen: config.json, compacted; whether
# cli-plugins is a symlink; config.json's mode.
probe() {
	local cfg="$1" start
	shift
	rm -f "$WORK_DIR/seen"
	start=$(date +%s)
	# shellcheck disable=SC2016 # expanded by the inner bash
	OUT=$(env PATH="$BIN:$PATH" DOCKER_CONFIG="$cfg" VALIDATE_CREDENTIAL_HELPER_TIMEOUT=1 "$@" \
		bash -c '. "$1"; require_docker_credential_helper; check_prereqs
			{ jq -c . "$DOCKER_CONFIG/config.json"
			  if [ -L "$DOCKER_CONFIG/cli-plugins" ]; then echo linked; else echo not-linked; fi
			  stat -c %a "$DOCKER_CONFIG/config.json"; } >"$2" 2>&1
			echo "DOCKER_CONFIG=$DOCKER_CONFIG"' \
		_ "$ROOT_DIR/scripts/validate/lib.sh" "$WORK_DIR/seen" 2>"$WORK_DIR/err")
	RC=$?
	SECS=$(($(date +%s) - start))
	ERR=$(cat "$WORK_DIR/err")
}

echo "== 1. A helper that never answers is bypassed =="
fixture_config "$WORK_DIR/hang" hang
before=$(cat "$WORK_DIR/hang/config.json")
probe "$WORK_DIR/hang"
check_eq "probe exits 0" 0 "$RC"
check "probe is bounded by its timeout (${SECS}s)" test "$SECS" -le 4
check_eq "exactly one NOTE line" 1 "$(grep -c '^NOTE: docker credential helper docker-credential-hang did not answer' <<<"$OUT")"
check_eq "nothing else printed" 2 "$(wc -l <<<"$OUT" | tr -d ' ')"
scratch=$(sed -n 's/^DOCKER_CONFIG=//p' <<<"$OUT")
check "DOCKER_CONFIG points elsewhere" test -n "$scratch" -a "$scratch" != "$WORK_DIR/hang"
check_eq "the fixture config is untouched" "$before" "$(cat "$WORK_DIR/hang/config.json")"
check_json "scratch config.json drops credsStore and keeps auths" "$(sed -n 1p "$WORK_DIR/seen")" \
	'(has("credsStore") | not) and .auths["example.test"] == {}'
check_eq "scratch dir links cli-plugins" linked "$(sed -n 2p "$WORK_DIR/seen")"
check_eq "scratch config.json is private" 600 "$(sed -n 3p "$WORK_DIR/seen")"
# The scratch dir outlives the bash that made it by up to a second.
for _ in 1 2 3 4 5; do [ -e "$scratch" ] || break; sleep 1; done
check "scratch dir removed after the script exits" test -n "$scratch" -a ! -e "$scratch"
# The probe's helper is killed, child and all, not left running.
# shellcheck disable=SC2016 # expanded by the inner bash
check "the hung helper's child is killed" bash -c '[ -s "$1" ] && ! kill -0 "$(cat "$1")" 2>/dev/null' _ "$WORK_DIR/hang.pid"

echo "== 2. A helper that answers is left alone =="
fixture_config "$WORK_DIR/answers" answers
probe "$WORK_DIR/answers"
check_eq "probe exits 0" 0 "$RC"
check_eq "only DOCKER_CONFIG printed, unchanged" "DOCKER_CONFIG=$WORK_DIR/answers" "$OUT"

echo "== 3. A scratch config that can't be built is a hard prerequisite =="
probe "$WORK_DIR/hang" TMPDIR="$WORK_DIR/no-such-dir"
check_eq "exits 2" 2 "$RC"
check "ERROR names the helper" grep -q 'ERROR: docker credential helper docker-credential-hang .*could not be built' <<<"$ERR"
check "it stops before any work" bash -c "! grep -q '^DOCKER_CONFIG=' <<<\"\$1\"" _ "$OUT"

echo "== 4. No credsStore, no probe =="
fixture_config "$WORK_DIR/none" ""
probe "$WORK_DIR/none"
check_eq "probe exits 0" 0 "$RC"
check_eq "DOCKER_CONFIG unchanged" "DOCKER_CONFIG=$WORK_DIR/none" "$OUT"

summary
