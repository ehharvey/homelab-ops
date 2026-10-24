#! /bin/bash
# finding.sh — the findings log: things noticed outside the current issue (#314).
#   add --kind bug|drift|follow-up --where <path:line> [--origin <n>] [--branch <b>] [--session <id>] "<one line why>"
#   list [--unfiled] [--limit N] [--json]
#   count [--unfiled]
#   mark-filed <id> <issue#>
#
# Findings used to live in a session's context (an issue-worker's "## Noticed"
# hand-back, or a note to post "at the end of the session") and were lost when
# the session ended or compacted. The log is the source of truth instead:
# .claude/hooks/finding-noticed.sh appends issue-worker Noticed bullets,
# sessions run `add` directly, and /file-findings files the unfiled ones as
# `proposed` issues.
#
# One JSONL record per finding:
#   {id, ts, session, branch, origin, kind, where, text, filed_as}
# `origin` and `filed_as` are issue numbers or null. Unfiled means filed_as is
# null.
#
# The log is $(git rev-parse --git-common-dir)/claude-findings.jsonl: one file
# shared by every worktree, outside any working tree (so it can't be committed),
# and untouched by `make wt-gc`. FINDINGS_LOG overrides it, which is how the
# tests keep off the real log. add and mark-filed hold an flock on <log>.lock,
# since parallel workers' hooks append at once; mark-filed rewrites atomically
# (temp file + mv).
set -euo pipefail

usage() {
	sed -n '3,6p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

die() {
	echo "finding.sh: $*" >&2
	exit 1
}

log_path() {
	if [ -n "${FINDINGS_LOG:-}" ]; then
		echo "$FINDINGS_LOG"
		return
	fi
	# Resolve against the repo holding this script, not the caller's cwd: a hook
	# may run from anywhere. Absolute, because plain --git-common-dir is
	# relative (".git") in the main checkout (as in scripts/worktree.sh).
	local common
	common=$(git -C "$(dirname "$0")" rev-parse --path-format=absolute --git-common-dir) ||
		die "not inside a git repository"
	echo "$common/claude-findings.jsonl"
}

# issue_number <s> — the digits of "#123", "123" or "eharvey/#123"; empty if none.
issue_number() {
	local s="$1"
	if [[ "$s" =~ ^#?([0-9]+)$ ]] || [[ "$s" =~ ^eharvey/#([0-9]+)$ ]]; then
		echo "${BASH_REMATCH[1]}"
	fi
}

cmd_add() {
	local kind="" where="" origin="" branch="" session="${CLAUDE_SESSION_ID:-}" text=""
	while [ $# -gt 0 ]; do
		case "$1" in
		--kind) kind="${2:-}"; shift ;;
		--where) where="${2:-}"; shift ;;
		--origin) origin="${2:-}"; shift ;;
		--branch) branch="${2:-}"; shift ;;
		--session) session="${2:-}"; shift ;;
		--) shift; text="$*"; break ;;
		-*) die "add: unknown flag $1" ;;
		*) [ -z "$text" ] || die "add: one quoted text argument, please"; text="$1" ;;
		esac
		shift
	done
	case "$kind" in
	bug | drift | follow-up) ;;
	*) die "add: --kind must be bug, drift or follow-up (got '${kind}')" ;;
	esac
	[ -n "$where" ] || die "add: --where <path:line> is required"
	[ -n "$text" ] || die "add: the one-line text is required"
	[[ "$text" != *$'\n'* ]] || die "add: text must be one line"

	if [ -z "$branch" ]; then
		branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)
	fi
	local origin_n=""
	if [ -n "$origin" ]; then
		origin_n=$(issue_number "$origin")
		[ -n "$origin_n" ] || die "add: --origin must be an issue number (got '$origin')"
	else
		origin_n=$(issue_number "$branch")
	fi

	local log ts id
	log=$(log_path)
	ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	id="$(date -u +%Y%m%dT%H%M%SZ)-$(printf '%s\n%s\n%s\n%s' "$where" "$text" "$session" "$(date +%N)$$" | sha256sum | cut -c1-6)"

	mkdir -p "$(dirname "$log")"
	exec 9>>"$log.lock"
	flock 9
	touch "$log"
	# Idempotent per session: a hook that fires twice for one stop must not
	# double-record. Only when the session is known, since without one two
	# identical notes can't be told apart from one note made twice.
	if [ -n "$session" ]; then
		local dup
		dup=$(jq -r --arg s "$session" --arg w "$where" --arg t "$text" \
			'select(.session == $s and .where == $w and .text == $t) | .id' "$log" | head -n1)
		if [ -n "$dup" ]; then
			echo "$dup (already recorded)"
			return 0
		fi
	fi
	jq -cn --arg id "$id" --arg ts "$ts" --arg session "$session" --arg branch "$branch" \
		--arg origin "$origin_n" --arg kind "$kind" --arg where "$where" --arg text "$text" \
		'{id: $id, ts: $ts,
		  session: (if $session == "" then null else $session end),
		  branch: (if $branch == "" or $branch == "HEAD" then null else $branch end),
		  origin: (if $origin == "" then null else ($origin | tonumber) end),
		  kind: $kind, where: $where, text: $text, filed_as: null}' >>"$log"
	echo "$id"
}

# select_filter <unfiled> — the jq filter list/count share.
select_filter() {
	if [ "$1" = 1 ]; then echo 'select(.filed_as == null)'; else echo '.'; fi
}

cmd_list() {
	local unfiled=0 limit=50 json=0
	while [ $# -gt 0 ]; do
		case "$1" in
		--unfiled) unfiled=1 ;;
		--limit) limit="${2:-}"; shift ;;
		--json) json=1 ;;
		*) die "list: unknown argument $1" ;;
		esac
		shift
	done
	[[ "$limit" =~ ^[0-9]+$ ]] || die "list: --limit takes a number"
	local log
	log=$(log_path)
	[ -s "$log" ] || return 0
	local filter total
	filter=$(select_filter "$unfiled")
	total=$(jq -c "$filter" "$log" | wc -l)
	if [ "$json" = 1 ]; then
		jq -c "$filter" "$log" | tail -n "$limit"
	else
		jq -r "$filter"' | [.id, .kind, .where, .text,
			(if .origin then "from #\(.origin)" else "from ?" end),
			(if .filed_as then "filed #\(.filed_as)" else "unfiled" end)] | join("  ")' "$log" |
			tail -n "$limit"
	fi
	if [ "$total" -gt "$limit" ]; then
		echo "finding.sh: showing the last $limit of $total; pass --limit for more" >&2
	fi
}

cmd_count() {
	local unfiled=0
	case "${1:-}" in
	--unfiled) unfiled=1 ;;
	"") ;;
	*) die "count: unknown argument $1" ;;
	esac
	local log
	log=$(log_path)
	if [ -s "$log" ]; then
		jq -c "$(select_filter "$unfiled")" "$log" | wc -l
	else
		echo 0
	fi
}

cmd_mark_filed() {
	[ $# -eq 2 ] || usage
	local id="$1" issue
	issue=$(issue_number "$2")
	[ -n "$issue" ] || die "mark-filed: '$2' is not an issue number"
	local log
	log=$(log_path)
	[ -s "$log" ] || die "mark-filed: no findings in $log"
	exec 9>>"$log.lock"
	flock 9
	local n
	n=$(jq -r --arg id "$id" 'select(.id == $id) | .id' "$log" | wc -l)
	[ "$n" -ge 1 ] || die "mark-filed: no finding with id $id"
	local tmp
	tmp=$(mktemp "$log.XXXXXX")
	if ! jq -c --arg id "$id" --argjson n "$issue" \
		'if .id == $id then .filed_as = $n else . end' "$log" >"$tmp"; then
		rm -f "$tmp"
		die "mark-filed: could not rewrite $log"
	fi
	mv "$tmp" "$log"
	echo "$id filed as #$issue"
}

[ $# -ge 1 ] || usage
sub="$1"
shift
case "$sub" in
add) cmd_add "$@" ;;
list) cmd_list "$@" ;;
count) cmd_count "$@" ;;
mark-filed) cmd_mark_filed "$@" ;;
-h | --help | help) usage ;;
*) usage ;;
esac
