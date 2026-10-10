#! /bin/bash
# Validates GH issue #296: .github/workflows/bump-incusos.yml's pin job opens a
# PR that changes only scripts/incusos-base.version when the CDN's newest
# `stable` IncusOS is not the pinned one, and a test-mode dispatch does that
# without touching a real bump branch or PR. Nothing lints workflows here, and
# every moving part is GitHub's (a dispatch, a runner, a PR), so a real run is
# the only check there is.
#
# THIS MUTATES THE REAL REPOSITORY. It is opt-in: without
# VALIDATE_GITHUB_WRITES=1 it skips (exit 3) before contacting GitHub, and it
# refuses to run in CI at all, where dispatching a workflow from a workflow
# would recurse. It is in no `make` target.
#
# What it does:
#   1. Builds a throwaway branch: the commit under test (this checkout's HEAD)
#      plus one commit setting the pin to an older version, and pushes it as
#      validate-296/pin-<time>-<pid>.
#   2. Dispatches bump-incusos.yml from that branch with test_mode=true and
#      waits for that run, found by the branch it was dispatched from (which
#      only this run of this script ever pushed), not by "the latest run".
#   3. Asserts the run succeeded, the submodule job was skipped, and exactly
#      one PR came of it: on chore/bump-incusos-base-test-<run id>, into the
#      throwaway branch, opened by the workflow, one commit, one file, taking
#      the pin from the old value to the newest stable.
#   4. Closes that PR and deletes both branches, then asserts the real
#      chore/bump-incus-os and chore/bump-incusos-base refs and their PRs are
#      exactly as they were before step 1.
#
# Why test mode exists (learned the hard way, see #296): both jobs commit on
# top of whatever ref the workflow was dispatched from. A plain dispatch from a
# feature branch therefore rebuilt chore/bump-incus-os on that branch and put
# its commits into the real submodule bump PR. test_mode skips the submodule
# job and gives the pin PR a per-run branch, so this script can prove the pin
# job from an unmerged commit and leave no trace.
#
# Cleanup runs in an EXIT trap, so it also happens on a failed assertion, a
# timeout or Ctrl-C. It stops a run that is still going first (it would
# otherwise push its PR branch after the cleanup), and it only closes and
# deletes what this run made: the throwaway branch it pushed, the PR(s) whose
# base is that branch, and the per-run branch of the run(s) dispatched from it.
# Nothing is matched by name pattern. Leaked refs are still easy to spot:
# `validate-296/...` and `chore/bump-incusos-base-test-...`.
#
# The throwaway push uses --no-verify. .githooks/pre-push enforces "one commit
# ahead of main" on everything pushed, because that is what may reach main; this
# branch is two commits ahead by construction (the commit under test + the pin
# change) and never becomes a PR into main, so the rule does not apply to it and
# the hook cannot know that. The hook itself is untouched.
#
# Knobs: VALIDATE_OLD_PIN (default 202609271243; any 12-digit version that is
# not the newest stable), VALIDATE_RUN_TIMEOUT (seconds to wait for the run,
# default 300).
#
# Fail-fast up to the end of the run, like the other `github` scripts: each
# step needs the last. The assertions about the finished run accumulate.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="bump-incusos.yml's pin job opens a PR changing only the pin file; a test-mode dispatch touches no real bump branch or PR (#296)"
VALIDATE_GROUP="github"
VALIDATE_NEEDS="VALIDATE_GITHUB_WRITES=1 (opt-in: pushes branches, dispatches a workflow, opens and closes a real PR), gh authenticated with push access, git curl openssl jq"
VALIDATE_DURATION="~1m"

validate_parse_args "$@"

# --- Opt-in, before anything reaches GitHub ----------------------------------
WHAT="test-mode dispatch of bump-incusos.yml opens a pin-only PR"
if [ -n "${CI:-}${GITHUB_ACTIONS:-}" ]; then
  skip_check "$WHAT" opt-in "never runs in CI: it dispatches a workflow, and from a workflow that would recurse"
  summary
fi
if [ "${VALIDATE_GITHUB_WRITES:-}" != 1 ]; then
  skip_check "$WHAT" opt-in \
    "this pushes branches to the real repository, dispatches a workflow and opens a PR — set VALIDATE_GITHUB_WRITES=1 to run it"
  summary
fi

WORKFLOW="bump-incusos.yml"
PIN_FILE="scripts/incusos-base.version"
FETCH="$ROOT_DIR/scripts/fetch-incusos-base.sh"
OLD_PIN="${VALIDATE_OLD_PIN:-202609271243}"
RUN_TIMEOUT="${VALIDATE_RUN_TIMEOUT:-300}"
REAL_BRANCHES="chore/bump-incus-os chore/bump-incusos-base"

# --- Hard prerequisites (exit 2, before doing any work) ----------------------
require_cmd gh git curl openssl jq
check_prereqs

REPO=""
LATEST=""
if ! gh auth status >/dev/null 2>&1; then
  _prereq_missing "gh is not authenticated"
elif ! REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null) || [ -z "$REPO" ]; then
  _prereq_missing "could not resolve the GitHub repository (network?)"
elif [ "$(gh api "repos/$REPO" --jq .permissions.push 2>/dev/null)" != "true" ]; then
  _prereq_missing "gh has no push access to $REPO"
fi
# The run executes HEAD's workflow and fetch script, not the working tree's.
if ! git -C "$ROOT_DIR" show "HEAD:.github/workflows/$WORKFLOW" 2>/dev/null | grep -q 'test_mode'; then
  _prereq_missing "HEAD's $WORKFLOW has no test_mode input; a dispatch would touch the real bump PRs"
fi
if ! git -C "$ROOT_DIR" diff --quiet HEAD -- ".github/workflows/$WORKFLOW" scripts/fetch-incusos-base.sh scripts/incusos-keys "$PIN_FILE"; then
  _prereq_missing "uncommitted changes to the workflow, the fetch script, its keys or the pin: this tests HEAD, so commit them first"
fi
if ! LATEST=$("$FETCH" --latest-stable 2>/dev/null); then
  _prereq_missing "could not resolve the newest stable IncusOS from the CDN (network?)"
elif ! [[ "$OLD_PIN" =~ ^[0-9]{12}$ ]] || [ "$OLD_PIN" = "$LATEST" ]; then
  _prereq_missing "VALIDATE_OLD_PIN=$OLD_PIN must be a 12-digit version other than the newest stable ($LATEST)"
fi
# Another run of the workflow would move the real bump branches under this
# one's "nothing real changed" assertion, for a reason that isn't its own.
if [ -n "$REPO" ]; then
  if ! busy=$(gh run list --repo "$REPO" --workflow "$WORKFLOW" --limit 20 --json status \
    --jq '[.[] | select(.status != "completed")] | length' 2>/dev/null); then
    _prereq_missing "could not list $WORKFLOW runs"
  elif [ "$busy" != 0 ]; then
    _prereq_missing "$busy run(s) of $WORKFLOW are in progress; run this again when they finish"
  fi
fi
check_prereqs

STAMP="$(date +%s)-$$"
BRANCH="validate-296/pin-$STAMP"
WORK_DIR="$(mktemp -d)"
WT="$WORK_DIR/throwaway"
PUSHED=""
DISPATCHED=""
TORN_DOWN=""

pass() { record_pass "$*"; }
fail() {
  record_fail "$@"
  summary
}

remote_ref() { git -C "$ROOT_DIR" ls-remote origin "refs/heads/$1" | cut -f1; }

# our_runs — ids of the runs dispatched from the throwaway branch. Only this
# run of this script ever pushed that branch, so these are its runs and no one
# else's.
our_runs() {
  gh run list --repo "$REPO" --workflow "$WORKFLOW" --branch "$BRANCH" --limit 20 \
    --json databaseId --jq '.[].databaseId' 2>/dev/null
}

run_field() { gh run view "$1" --repo "$REPO" --json "$2" --jq ".$2" 2>/dev/null; }

# await_run_done <id> <seconds> — 0 once the run has completed.
await_run_done() {
  local id="$1" deadline=$((SECONDS + $2))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(run_field "$id" status)" = "completed" ] && return 0
    sleep 5
  done
  return 1
}

# snapshot — the real bump branches' SHAs and every PR on them. Fails rather
# than returning a partial answer: two empty snapshots would compare equal.
snapshot() {
  local b prs
  git -C "$ROOT_DIR" ls-remote origin $(printf 'refs/heads/%s ' $REAL_BRANCHES) || return 1
  for b in $REAL_BRANCHES; do
    prs=$(gh pr list --repo "$REPO" --head "$b" --state all --limit 200 \
      --json number,state,headRefOid,title \
      --jq 'sort_by(.number)[] | "\(.number) \(.state) \(.headRefOid) \(.title)"') || return 1
    [ -z "$prs" ] || sed "s|^|PR on $b: #|" <<<"$prs"
  done
}

teardown() {
  [ -z "$TORN_DOWN" ] || return 0
  TORN_DOWN=1
  cd "$ROOT_DIR" || return
  if [ -n "$PUSHED" ]; then
    echo "--- teardown: closing what this run opened and deleting its branches"
    local id n i ids=""
    # A dispatch GitHub accepted may not be listed yet; give it a moment so its
    # run cannot outlive this cleanup unseen.
    for i in 1 2 3 4 5 6; do
      ids=$(our_runs)
      [ -n "$ids" ] || [ -z "$DISPATCHED" ] && break
      sleep 5
    done
    for id in $ids; do
      if [ "$(run_field "$id" status)" != "completed" ]; then
        echo "    cancelling run $id, still in progress"
        gh run cancel "$id" --repo "$REPO" >/dev/null 2>&1
        await_run_done "$id" 120 || echo "    WARNING: run $id did not stop within 120s; check for a leaked chore/bump-incusos-base-test-$id" >&2
      fi
    done
    # Only a run dispatched from the throwaway branch can target it.
    for n in $(gh pr list --repo "$REPO" --base "$BRANCH" --state open --json number --jq '.[].number' 2>/dev/null); do
      gh pr close "$n" --repo "$REPO" >/dev/null 2>&1
    done
    for id in $ids; do
      [ -z "$(remote_ref "chore/bump-incusos-base-test-$id")" ] ||
        git push -q --no-verify origin --delete "chore/bump-incusos-base-test-$id" >/dev/null 2>&1
    done
    git push -q --no-verify origin --delete "$BRANCH" >/dev/null 2>&1
  fi
  git worktree remove -f "$WT" >/dev/null 2>&1
  rm -rf "$WORK_DIR"
}
trap teardown EXIT
trap 'exit 130' INT TERM

echo "== 0. The real bump branches, before =="
before=$(snapshot) || fail "could not read the real bump branches and their PRs"
if [ -n "$before" ]; then
  sed 's/^/    /' <<<"$before" | cut -c1-150
else
  echo "    (none exist)"
fi
pass "recorded $(wc -l <<<"$before" | tr -d ' ') line(s) of state for: $REAL_BRANCHES"

echo
echo "== 1. Throwaway branch: HEAD + the pin set to $OLD_PIN =="
head_sha=$(git -C "$ROOT_DIR" rev-parse HEAD)
git -C "$ROOT_DIR" worktree add -q --detach "$WT" "$head_sha" || fail "could not create a temporary worktree of HEAD"
echo "$OLD_PIN" >"$WT/$PIN_FILE"
git -C "$WT" add "$PIN_FILE"
git -C "$WT" commit -q -m "DO NOT MERGE: validate #296, pin set to $OLD_PIN [skip ci]" \
  -m "Throwaway commit pushed by scripts/validate/bump-workflow-opens-pin-only-pr.sh. Deleted automatically." ||
  fail "could not commit the older pin (is HEAD's pin already $OLD_PIN?)"
throwaway_sha=$(git -C "$WT" rev-parse HEAD)
PUSHED=1
# --no-verify: see the header. Two commits ahead of main by construction, and
# never a PR into main.
git -C "$WT" push -q --no-verify origin "HEAD:refs/heads/$BRANCH" || fail "could not push $BRANCH"
[ "$(remote_ref "$BRANCH")" = "$throwaway_sha" ] || fail "origin/$BRANCH is not the commit just pushed"
pass "pushed $BRANCH (${head_sha:0:7} + pin $OLD_PIN)"

echo
echo "== 2. Dispatch $WORKFLOW from it in test mode, and wait for that run =="
DISPATCHED=1
out=$(gh workflow run "$WORKFLOW" --repo "$REPO" --ref "$BRANCH" -f test_mode=true 2>&1) ||
  fail "GitHub refused the dispatch" "$out"

RUN_ID=""
for _ in $(seq 1 30); do
  RUN_ID=$(gh run list --repo "$REPO" --workflow "$WORKFLOW" --branch "$BRANCH" --event workflow_dispatch \
    --limit 5 --json databaseId,headSha --jq "[.[] | select(.headSha == \"$throwaway_sha\")][0].databaseId // empty" 2>/dev/null)
  [ -n "$RUN_ID" ] && break
  sleep 3
done
[ -n "$RUN_ID" ] || fail "the dispatch was accepted but no run on $BRANCH appeared within 90s"
TEST_BRANCH="chore/bump-incusos-base-test-$RUN_ID"
echo "    run $RUN_ID: https://github.com/$REPO/actions/runs/$RUN_ID"

await_run_done "$RUN_ID" "$RUN_TIMEOUT" || fail "run $RUN_ID did not finish within ${RUN_TIMEOUT}s"
conclusion=$(run_field "$RUN_ID" conclusion)
if [ "$conclusion" != "success" ]; then
  gh run view "$RUN_ID" --repo "$REPO" --log-failed 2>/dev/null | tail -n 20
  fail "run $RUN_ID concluded '$conclusion', wanted success"
fi
pass "test-mode run $RUN_ID succeeded"

echo
echo "== 3. What the run did =="
jobs=$(gh run view "$RUN_ID" --repo "$REPO" --json jobs --jq '[.jobs[] | "\(.name)=\(.conclusion)"] | sort | join(" ")')
check_eq "the submodule job was skipped and the pin job ran" "bump-base-pin=success check-and-bump=skipped" "$jobs"

# The PR is listed a moment after the run reports success.
prs="[]"
for _ in $(seq 1 10); do
  prs=$(gh pr list --repo "$REPO" --base "$BRANCH" --state all --json number,headRefName,state,author,title 2>/dev/null || echo "[]")
  [ "$(jq length <<<"$prs")" != 0 ] && break
  sleep 3
done
check_eq "exactly one PR targets the throwaway branch" "1" "$(jq length <<<"$prs")"
PR=$(jq -r '.[0].number // empty' <<<"$prs")
if [ -z "$PR" ]; then
  record_fail "the run opened a PR" "none found with base $BRANCH"
else
  echo "    PR #$PR: $(jq -r '.[0].title' <<<"$prs")"
  check_eq "PR #$PR is on the per-run test branch, not chore/bump-incusos-base" "$TEST_BRANCH" "$(jq -r '.[0].headRefName' <<<"$prs")"
  check_eq "it is open" "OPEN" "$(jq -r '.[0].state' <<<"$prs")"
  check_json "it was opened by the workflow (github-actions)" "$prs" '.[0].author.login | test("github-actions")'
  check_json "its title marks it as a test run" "$prs" '.[0].title | startswith("DO NOT MERGE (test run): ")'
  detail=$(gh pr view "$PR" --repo "$REPO" --json files,commits 2>/dev/null || echo "{}")
  check_eq "its only changed file is $PIN_FILE" "$PIN_FILE" "$(jq -r '[.files[]?.path] | join(" ")' <<<"$detail")"
  check_eq "it is one commit" "1" "$(jq -r '.commits | length' <<<"$detail")"
  new_pin=$(gh api -H "Accept: application/vnd.github.raw+json" "repos/$REPO/contents/$PIN_FILE?ref=$TEST_BRANCH" 2>/dev/null | tr -d '[:space:]')
  check_eq "it takes the pin from $OLD_PIN to the newest stable" "$LATEST" "$new_pin"
fi
check_eq "no PR was opened from the throwaway branch itself" "0" \
  "$(gh pr list --repo "$REPO" --head "$BRANCH" --state all --json number --jq length 2>/dev/null)"

echo
echo "== 4. Teardown, and the real bump branches after =="
teardown
if [ -n "$PR" ]; then
  check_eq "PR #$PR is closed" "CLOSED" "$(gh pr view "$PR" --repo "$REPO" --json state --jq .state 2>/dev/null)"
fi
check_eq "the test branch $TEST_BRANCH is deleted" "" "$(remote_ref "$TEST_BRANCH")"
check_eq "the throwaway branch $BRANCH is deleted" "" "$(remote_ref "$BRANCH")"

if after=$(snapshot); then
  if [ "$before" = "$after" ]; then
    pass "the real bump branches and their PRs are exactly as before: $REAL_BRANCHES"
  else
    others=$(gh run list --repo "$REPO" --workflow "$WORKFLOW" --limit 10 --json databaseId,headBranch,createdAt \
      --jq "[.[] | select(.databaseId != $RUN_ID) | \"\(.databaseId) on \(.headBranch) at \(.createdAt)\"] | join(\"; \")" 2>/dev/null)
    diff <(echo "$before") <(echo "$after") | sed 's/^/    /' | cut -c1-200
    record_fail "the real bump branches and their PRs are exactly as before" \
      "they changed during the test run (diff above). Other recent $WORKFLOW runs, which may be the cause instead: ${others:-none}"
  fi
else
  record_fail "the real bump branches and their PRs are exactly as before" "could not read them after the run"
fi

summary
