#!/usr/bin/env bash
# Repo hygiene (#388): checks for drift in issue state that no PR check can
# catch, because labels, links and milestones change without a PR. /next prints
# the `deps` group as its "Dependency inconsistencies" section, and
# .github/workflows/hygiene.yml runs every check nightly and writes the summary
# into the "Repo hygiene" issue.
#
#   scripts/hygiene.sh                   one JSON line per check (the default)
#   scripts/hygiene.sh --summary         the human-readable report
#   --group deps|roadmap                 only that group's checks
#   --data <dir>                         reuse open.json, states.json and
#                                        paused.json from <dir> (/next's fetch)
#   --from <file>                        render a saved JSON-lines run, no fetch
#
# Each JSON line is {"check": "<id>", "count": N, "issues": [...], "detail": "..."}.
# `count` is how many findings the check has, `issues` the issue numbers they
# name, and `detail` the human-readable findings, one per line (empty when
# count is 0). proposed-age is the exception: its detail always reports how
# many proposed issues wait and the oldest, and only the ones past the limit
# count.
#
# Exit: 0 when every count is 0, 1 when any is above 0, 3 (SKIP, as in
# scripts/validate/lib.sh) when gh has no auth, anything else on an error.
#
# Checks, `deps` group (moved from .claude/skills/next/next.sh):
#   ready-proposed       Ready and proposed together
#   ready-blocked        Ready with an open blocked-by link
#   ready-done-when      Ready, but the Done-when names no validate script or
#                        real-run check (.claude/skills/file-task/done-when.jq, #410)
#   blocker-not-planned  blocked by an issue closed as not planned or duplicate
#   cycle                blocked-by links that loop
#   prose-no-link        dependency prose with no matching link
# `roadmap` group:
#   milestone-heading    an open "Phase N: <title>" milestone with no matching
#                        "## Phase N — <title>" heading in docs/Roadmap.md
#   phase-milestone      an open phase-N issue without the Phase N milestone
#   template-phases      roadmap_task.yml's phase dropdown isn't every "## "
#                        heading of docs/Roadmap.md, verbatim and in order
#   phase-off-roadmap    an open phase-N issue docs/Roadmap.md never mentions,
#                        whose body doesn't say "Not a Roadmap checklist item"
#   proposed-age         proposed issues older than PROPOSED_MAX_DAYS
#
# The local-state checks (unfiled scripts/finding.sh entries, stale worktrees
# and branches) stay in /next: CI can't see them.
# shellcheck disable=SC2016 # the $vars in single quotes are jq's
set -euo pipefail

# A proposed issue waiting longer than this many days fails proposed-age.
PROPOSED_MAX_DAYS=${HYGIENE_PROPOSED_MAX_DAYS:-14}

usage() { sed -n '8,14p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

summary=0 group=all data="" from=""
while [ $# -gt 0 ]; do
	case $1 in
	--summary) summary=1 ;;
	--group)
		group=${2:?--group needs deps or roadmap}
		shift
		;;
	--data)
		data=${2:?--data needs a directory}
		shift
		;;
	--from)
		from=${2:?--from needs a file}
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "hygiene.sh: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done
case $group in all | deps | roadmap) ;; *)
	echo "hygiene.sh: --group is deps or roadmap, not $group" >&2
	exit 2
	;;
esac

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
skills="$root/.claude/skills"
# shellcheck source=.claude/skills/lib.sh
. "$skills/lib.sh"
cd "$root"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# render <jsonl>: print it, or its summary, and exit by the counts.
render() {
	if [ "$summary" = 1 ]; then
		jq -s -r --arg group "$group" '
		  def deps: .check | IN("ready-proposed", "ready-blocked", "ready-done-when",
		    "blocker-not-planned", "cycle", "prose-no-link");
		  def found: map(select(.count > 0) | .detail) | if length == 0 then "(none)" else .[] end;
		  if $group == "deps" then map(select(deps)) | found
		  elif $group == "roadmap" then map(select(deps | not)) | found
		  else
		    "== Dependency inconsistencies ==", (map(select(deps)) | found), "",
		    "== Roadmap, milestone and triage drift ==", (map(select(deps | not)) | found), "",
		    "== Counts (any above 0 fails the nightly run) ==",
		    (.[] | "\(.check): \(.count)\(if .count == 0 and .detail != "" then " (\(.detail))" else "" end)")
		  end' "$1"
	else
		cat "$1"
	fi
	jq -s -e 'all(.count == 0)' "$1" >/dev/null || return 1
}

if [ -n "$from" ]; then
	rc=0
	render "$from" || rc=$?
	exit "$rc"
fi

# Without gh auth there's nothing to check: a SKIP, not a FAIL. `gh auth token`
# answers from GH_TOKEN or the stored login without a network call.
gh_ready=0
need_gh() {
	[ "$gh_ready" = 1 ] && return
	if ! command -v gh >/dev/null || ! gh auth token >/dev/null 2>&1; then
		echo "SKIP: repo hygiene checks [gh-auth] — gh has no auth (gh auth login, or set GH_TOKEN)" >&2
		exit 3
	fi
	repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
	gh_ready=1
}

# input <name>: the path of <name>.json, from --data when it's there, else
# fetched into $tmp.
input() {
	if [ -n "$data" ] && [ -f "$data/$1.json" ]; then
		echo "$data/$1.json"
		return
	fi
	if [ ! -f "$tmp/$1.json" ]; then
		case $1 in
		open) need_gh && open_issues_json "$repo" >"$tmp/open.json" ;;
		states) need_gh && issue_states_json >"$tmp/states.json" ;;
		paused) roadmap_tsv | paused_json >"$tmp/paused.json" ;;
		milestones)
			need_gh
			gh api --paginate "repos/$repo/milestones?state=open&per_page=100" --jq '.[].title' |
				jq -R -s 'split("\n") | map(select(. != ""))' >"$tmp/milestones.json"
			;;
		esac
	fi
	echo "$tmp/$1.json"
}

# Resolve every input before any jq runs, so a SKIP exits here rather than in a
# command substitution.
need_gh_inputs() {
	if [ -z "$data" ] || [ ! -f "$data/open.json" ] || [ "$group" != deps ]; then need_gh; fi
}
need_gh_inputs
open=$(input open)
out="$tmp/checks.jsonl"
: >"$out"

if [ "$group" != roadmap ]; then
	states=$(input states)
	paused=$(input paused)
	# Dependency prose with no matching link, for issues filed or edited outside
	# /file-task. Phrasings are #234's keyword set plus the ones its review found
	# in Roadmap phase lines ("needed by", "follows", "should land before" …).
	# Only the #N list right after the phrase counts, quoted text is skipped (that's
	# a mention, not a dependency), and only open targets count: prose about a
	# closed issue can't make /next wrong.
	jq -c -L "$skills" --slurpfile states "$states" --slurpfile paused "$paused" '
	  include "file-task/done-when";
	  def st($n): $states[0][$n | tostring] // "?";
	  def blockers: [.blockedBy.nodes[] | select(.state == "OPEN") | .number];
	  def tag($n): if ($paused[0] | index($n)) then "#\($n) (paused)" else "#\($n)" end;
	  def ready: any(.labels.nodes[]; .name == "Ready");
	  # One JSON line per check: f yields a finding {n, m} per hit, n being the
	  # issue (or, for a cycle, the issues) it names and m its report line.
	  def check($id; f): [f] | {check: $id, count: length, issues: ([.[].n | arrays[], numbers] | unique), detail: (map(.m) | join("\n"))};
	  (map({key: (.number | tostring), value: [blockers[]]}) | from_entries) as $g
	  | def reach($s): {seen: [], f: $g[$s | tostring]}
	      | until(.f | length == 0; (.f - .seen) as $new | .seen += $new | .f = ([$new[] | $g[tostring][]?] | unique))
	      | .seen;
	    def prose($re): [(.body // "") | gsub("\"[^\"\n]*\"|“[^”\n]*”|`[^`\n]*`"; "")
	        | scan("(?i)(?:" + $re + ") *(#[0-9]+(?:(?:,| and| or|/) *#[0-9]+)*)") | .[0]
	        | scan("#([0-9]+)") | .[0] | tonumber]
	      | unique | map(select(st(.) == "OPEN"));
	  (map(.number) | map(select(. as $n | reach($n) | index($n)))) as $cyc
	  | check("ready-proposed"; .[] | select(ready) | select(any(.labels.nodes[]; .name == "proposed"))
	      | {n: .number, m: "#\(.number) has both Ready and proposed; triage drops proposed (until then /run-chain skips it)"}),
	    check("ready-blocked"; .[] | select(ready) | select(blockers | length > 0)
	      | {n: .number, m: "#\(.number) is Ready but blocked by open \(blockers | map(tag(.)) | join(" "))"}),
	    check("ready-done-when"; .[] | select(ready) | select(.body | done_when_ok | not)
	      | {n: .number, m: "#\(.number) is Ready but \(done_when_gap); see /file-task Ready criteria (/run-chain skips it)"}),
	    check("blocker-not-planned"; .[] | . as $i | .blockedBy.nodes[] | select(.stateReason == "NOT_PLANNED" or .stateReason == "DUPLICATE")
	      | {n: $i.number, m: "#\($i.number) is blocked by #\(.number), closed as \(.stateReason | ascii_downcase | sub("_"; " ")); drop the link or rethink #\($i.number)"}),
	    check("cycle"; $cyc | map(. as $n | [$n] + ([reach($n)[] | select(. as $m | $cyc | index($m)) | select(reach(.) | index($n))]) | unique) | unique[]
	      | {n: ., m: "cycle: \(map("#\(.)") | join(" ↔ "))"}),
	    check("prose-no-link"; .[] | . as $i
	      | ((prose("depends on|blocked (?:by|on)|waits? on|needs|follows|follow-up to|(?:lands?|land it|sequence it|any time) after")
	          - [.blockedBy.nodes[].number] - [$i.number]) | map("blocked by #\(.)")) as $by
	      | ((prose("(?:un)?blocks|prerequisite (?:for|of)|needed by|(?:should |must )?(?:lands?|be done|done|merged?) before")
	          - [.blocking.nodes[].number] - [$i.number]) | map("blocking #\(.)")) as $bl
	      | select(($by + $bl) | length > 0)
	      | {n: $i.number, m: "#\($i.number) prose says \($by + $bl | join(", ")), with no link"})
	' "$open" >>"$out"
fi

if [ "$group" != deps ]; then
	milestones=$(input milestones)
	# The Roadmap's "## " headings, and the phase dropdown's options.
	sed -n 's/^## //p' docs/Roadmap.md | jq -R -s 'split("\n") | map(select(. != ""))' >"$tmp/headings.json"
	awk '
		/^[ \t]*id: phase[ \t]*$/ { f = 1; next }
		f && /^[ \t]*validations:/ { exit }
		f && /^[ \t]*- "/ { sub(/^[ \t]*- "/, ""); sub(/"[ \t]*$/, ""); print }
	' .github/ISSUE_TEMPLATE/roadmap_task.yml | jq -R -s 'split("\n") | map(select(. != ""))' >"$tmp/options.json"
	jq -c --slurpfile ms "$milestones" --slurpfile heads "$tmp/headings.json" \
		--slurpfile opts "$tmp/options.json" --rawfile roadmap docs/Roadmap.md \
		--argjson max "$PROPOSED_MAX_DAYS" '
	  def check($id; f): [f] | {check: $id, count: length, issues: ([.[].n | numbers] | unique), detail: (map(.m) | join("\n"))};
	  # "Phase N: title" and "Phase N — title" as {n, t}; null for anything else.
	  def phase: [capture("^Phase (?<n>[0-9]+(?:\\.[0-9]+)?)(?: *:| +—) *(?<t>.*)$")] | first;
	  def phases: [.labels.nodes[].name | capture("^phase-(?<n>[0-9]+(?:\\.[0-9]+)?)$").n];
	  ($heads[0] | map(phase | select(.))) as $hp
	  | ($ms[0] | map(select(test("^Phase [0-9]")))) as $pms
	  | check("milestone-heading"; $pms[] | . as $m | phase as $p
	      | if $p == null then {m: "milestone \"\($m)\" is not in the form \"Phase N: <title>\""}
	        elif any($hp[]; . == $p) then empty
	        else ([$hp[] | select(.n == $p.n)] | first) as $h
	          | {m: "milestone \"\($m)\" matches no docs/Roadmap.md heading; \(if $h then "the Roadmap has \"## Phase \($h.n) — \($h.t)\"" else "the Roadmap has no Phase \($p.n)" end)"}
	        end),
	    check("phase-milestone"; .[] | . as $i | phases[] | . as $n
	      | ($i.milestone.title // null) as $have
	      | select(($have // "" | phase | .n?) != $n)
	      | ([$pms[] | select(phase.n == $n)] | first) as $want
	      | {n: $i.number, m: "#\($i.number) is labelled phase-\($n) but \(if $have then "its milestone is \"\($have)\"" else "has no milestone" end); \(if $want then "gh issue edit \($i.number) --milestone \"\($want)\"" else "no open Phase \($n) milestone exists" end)"}),
	    check("template-phases"; $opts[0] as $o | $heads[0] as $h
	      | if $o == $h then empty
	        elif ($o | sort) == ($h | sort) then {m: "the roadmap_task.yml phase dropdown lists the docs/Roadmap.md headings out of order"}
	        else (($h - $o)[] as $x | {m: "the roadmap_task.yml phase dropdown lacks \"\($x)\", a docs/Roadmap.md heading"}),
	             (($o - $h)[] as $x | {m: "the roadmap_task.yml phase dropdown lists \"\($x)\", which is not a docs/Roadmap.md heading"})
	        end),
	    check("phase-off-roadmap"; .[] | select(phases | length > 0)
	      | .number as $n | select($roadmap | test("#\($n)(?![0-9])") | not)
	      | select(.body // "" | test("not a roadmap checklist item"; "i") | not)
	      | {n: .number, m: "#\(.number) is labelled \(phases | map("phase-\(.)") | join(", ")) but docs/Roadmap.md never mentions it; add it there, or say \"Not a Roadmap checklist item\" in its body"}),
	    ([.[] | select(any(.labels.nodes[]; .name == "proposed"))
	      | {n: .number, age: ((now - (.createdAt | fromdateiso8601)) / 86400 | floor)}] | sort_by(-.age, .n)) as $p
	    | [$p[] | select(.age > $max)] as $old
	    | {check: "proposed-age", count: ($old | length), issues: ($old | map(.n) | sort),
	       detail: ("\($p | length) proposed awaiting triage\(if ($p | length) > 0 then ", oldest #\($p[0].n) at \($p[0].age)d" else "" end)"
	         + (if ($old | length) > 0 then "; over the \($max)-day limit: \($old | map("#\(.n) (\(.age)d)") | join(", ")); add Ready and drop proposed, or close" else "" end))}
	' "$open" >>"$out"
fi

rc=0
render "$out" || rc=$?
exit "$rc"
