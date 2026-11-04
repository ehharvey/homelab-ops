#! /bin/bash
# Validates GH issue #388: scripts/hygiene.sh, the nightly hygiene.yml job's
# checks, flags each kind of Roadmap, milestone and triage drift, passes a
# clean state, and SKIPs with exit 3 when gh has no auth.
#
# Drives the real script against fixtures, never GitHub: a throwaway repo holds
# copies of scripts/hygiene.sh and the skills' shared lib.sh, lib.jq and
# done-when.jq, plus a fixture docs/Roadmap.md and roadmap_task.yml, and a stub
# `gh` serves fixture issues and milestones.
#   1. The clean fixture: every check's count is 0, and it exits 0. Covers a
#      decimal phase (3.5) and a non-Phase milestone, which is ignored.
#   2. The drifted fixture: milestone-heading, phase-milestone (missing and
#      wrong), template-phases, phase-off-roadmap, proposed-age and one moved
#      check (ready-proposed) each count their case, and it exits 1.
#   3. --summary --from renders a saved run without calling gh.
#   4. A gh with no auth: exit 3 and a SKIP line.
#
# What it can't prove: that the moved checks print exactly what next.sh did,
# or how the live repo reads. Those are #388's real-run checks (a before/after
# diff of /next, and each new check shown failing on a real case).

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR" || exit 2
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="scripts/hygiene.sh flags Roadmap, milestone and triage drift, passes a clean state, and SKIPs without gh auth (#388)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="jq, git"
VALIDATE_DURATION="~3s"
VALIDATE_SUPPORTED_OS="any"

validate_parse_args "$@"

require_cmd jq
require_cmd git
check_prereqs

WORK_DIR="$(mktemp -d)"
# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

REPO="$WORK_DIR/repo"
mkdir -p "$REPO/.claude/skills/file-task" "$REPO/scripts" "$REPO/docs" "$REPO/.github/ISSUE_TEMPLATE"
cp .claude/skills/lib.jq .claude/skills/lib.sh "$REPO/.claude/skills/"
cp .claude/skills/file-task/done-when.jq "$REPO/.claude/skills/file-task/"
cp scripts/hygiene.sh "$REPO/scripts/"
printf '# Roadmap\n\n## Phase 1 — Fixture\n\n- [ ] an item, see #1, #4\n\n## Phase 3.5 — Decimal\n\n## Parked — Later\n' \
	>"$REPO/docs/Roadmap.md"
git -C "$REPO" init -q -b main

# template <option>… — the fixture roadmap_task.yml with that phase dropdown.
template() {
	{
		printf 'body:\n  - type: dropdown\n    id: phase\n    attributes:\n      options:\n'
		printf '        - "%s"\n' "$@"
		printf '    validations:\n      required: true\n'
	} >"$REPO/.github/ISSUE_TEMPLATE/roadmap_task.yml"
}

GOOD=$'### Done when\n\n- [ ] Real-run check: run it.\n'
# fixture <dir> <jq program over $good, $now> — the issues GraphQL page it builds.
fixture() {
	mkdir -p "$WORK_DIR/$1"
	jq -n --arg good "$GOOD" "
	  def issue(\$n; \$labels; \$milestone; \$age; \$body): {number: \$n, title: \"[Task]: \(\$n)\", body: \$body,
	    createdAt: (now - \$age * 86400 | todate), milestone: (if \$milestone then {title: \$milestone} else null end),
	    labels: {nodes: (\$labels | map({name: .}))}, parent: null, blockedBy: {nodes: []}, blocking: {nodes: []}};
	  {data: {repository: {issues: {pageInfo: {hasNextPage: false, endCursor: null}, nodes: [$2]}}}}" \
		>"$WORK_DIR/$1/graphql.json"
}

# shellcheck disable=SC2016 # $good is jq's
fixture clean '
  issue(1; ["task", "phase-1"]; "Phase 1: Fixture"; 3; $good),
  issue(2; ["task", "phase-3.5"]; "Phase 3.5: Decimal"; 3; $good + "\nNot a Roadmap checklist item.\n"),
  issue(3; ["task", "proposed"]; null; 2; $good)'
printf '%s\n' "Phase 1: Fixture" "Phase 3.5: Decimal" "Parked: Later" >"$WORK_DIR/clean/milestones.txt"

# shellcheck disable=SC2016 # $good is jq's
fixture drift '
  issue(1; ["task", "phase-1"]; "Phase 1: Wrong title"; 3; $good),
  issue(2; ["task", "phase-3.5"]; null; 3; $good),
  issue(3; ["task", "proposed"]; null; 30; $good),
  issue(4; ["task", "phase-1"]; "Phase 3.5: Decimal"; 3; $good),
  issue(5; ["task", "Ready", "proposed"]; null; 0; $good)'
printf '%s\n' "Phase 1: Wrong title" "Phase 3.5: Decimal" "Parked: Later" >"$WORK_DIR/drift/milestones.txt"

STUB="$WORK_DIR/stub"
mkdir -p "$STUB"
cat >"$STUB/gh" <<EOF
#!/usr/bin/env bash
case "\$1 \$2" in
"auth token") [ -z "\${STUB_NO_AUTH:-}" ] ;;
"repo view") echo fixture/repo ;;
"api graphql") cat "$WORK_DIR/\$FIXTURE/graphql.json" ;;
"api --paginate") cat "$WORK_DIR/\$FIXTURE/milestones.txt" ;;
"issue list" | "pr list") echo '[]' ;;
*) echo "stub gh: unexpected \$*" >&2; exit 9 ;;
esac
EOF
chmod +x "$STUB/gh"

# hygiene <fixture> [args…] — the script's stdout; its exit code in $RC.
hygiene() {
	local f=$1
	shift
	OUT=$(cd "$REPO" && FIXTURE=$f PATH="$STUB:$PATH" ./scripts/hygiene.sh "$@" 2>"$WORK_DIR/stderr")
	RC=$?
}
# count <check> — that check's count in $OUT.
count() { jq -r --arg c "$1" 'select(.check == $c) | .count' <<<"$OUT"; }
issues() { jq -c --arg c "$1" 'select(.check == $c) | .issues' <<<"$OUT"; }

# --- 1. Clean ---------------------------------------------------------------

template "Phase 1 — Fixture" "Phase 3.5 — Decimal" "Parked — Later"
hygiene clean
check_eq "clean: exits 0" 0 "$RC"
check_eq "clean: one JSON line per check, 11 checks" 11 "$(jq -s length <<<"$OUT")"
check_eq "clean: every count is 0" "true" "$(jq -s 'all(.count == 0)' <<<"$OUT")"
check "clean: proposed-age still reports the waiting count" \
	grep -q '"detail":"1 proposed awaiting triage, oldest #3 at 2d"' <<<"$OUT"

# --- 2. Drift ---------------------------------------------------------------

template "Phase 1 — Fixture" "Phase 3.5 — Decimal"
hygiene drift
check_eq "drift: exits 1" 1 "$RC"
check_eq "milestone-heading flags the retitled Phase 1 milestone, not Parked" 1 "$(count milestone-heading)"
check "milestone-heading names the Roadmap's heading" \
	grep -q 'the Roadmap has \\"## Phase 1 — Fixture\\"' <<<"$OUT"
check_eq "phase-milestone flags #2 (missing) and #4 (wrong), not #1" "[2,4]" "$(issues phase-milestone)"
check "phase-milestone suggests the decimal phase's milestone" \
	grep -q 'gh issue edit 2 --milestone \\"Phase 3.5: Decimal\\"' <<<"$OUT"
check_eq "template-phases flags the missing dropdown option" 1 "$(count template-phases)"
check "template-phases names it" grep -q 'dropdown lacks \\"Parked — Later\\"' <<<"$OUT"
check_eq "phase-off-roadmap flags #2 only (#1 and #4 are on the Roadmap)" "[2]" "$(issues phase-off-roadmap)"
check_eq "proposed-age flags #3 (30d), not #5 (0d)" "[3]" "$(issues proposed-age)"
check_eq "ready-proposed (a moved check) flags #5" "[5]" "$(issues ready-proposed)"

hygiene drift --group deps
check_eq "--group deps prints only the 6 moved checks" 6 "$(jq -s length <<<"$OUT")"

# --- 3. Summary from a saved run ---------------------------------------------

hygiene drift
printf '%s\n' "$OUT" >"$WORK_DIR/run.jsonl"
SUMMARY=$(cd "$REPO" && PATH="$STUB:$PATH" FIXTURE=none ./scripts/hygiene.sh --summary --from "$WORK_DIR/run.jsonl")
check_eq "--summary --from exits 1 on a drifted run" 1 "$?"
check "--summary lists the findings under their sections" \
	grep -q '^#2 is labelled phase-3.5 but has no milestone' <<<"$SUMMARY"
check "--summary counts each check" grep -qx 'template-phases: 1' <<<"$SUMMARY"

# --- 4. No gh auth -------------------------------------------------------------

OUT=$(cd "$REPO" && STUB_NO_AUTH=1 FIXTURE=clean PATH="$STUB:$PATH" ./scripts/hygiene.sh 2>&1)
check_eq "no auth: exits 3" 3 "$?"
check "no auth: says SKIP" grep -q '^SKIP: ' <<<"$OUT"

summary
