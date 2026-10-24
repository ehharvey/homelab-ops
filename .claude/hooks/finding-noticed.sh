#! /bin/bash
# SubagentStop hook (#314): record each "## Noticed" bullet of an issue-worker
# hand-back in the findings log (scripts/finding.sh), so nothing a worker
# noticed depends on the root session's context surviving.
#
# Input: the hook JSON on stdin. The worker's final message is
# `last_assistant_message`; if that is absent, the last assistant message in
# `agent_transcript_path` is used instead.
#
# The format parsed is the one .claude/agents/issue-worker.md § Hand-back
# report prescribes; change both together:
#   ## Hand-back #<n>
#   branch: eharvey/#<n> @ <sha> ...
#   ## Noticed
#   - <path>:<line> — <one line> (bug | drift | follow-up)
# or `none` under the heading. A bullet may wrap onto indented lines.
#
# It only reads the worker's output and never blocks the stop: every path,
# including a parse error, exits 0, with any problem reported on stderr.
set -uo pipefail

finding="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/finding.sh"

note() { echo "finding-noticed: $*" >&2; }

input=$(cat) || exit 0
if ! jq -e . >/dev/null 2>&1 <<<"$input"; then
	note "hook input is not JSON; nothing recorded"
	exit 0
fi

# The settings matcher already limits this to issue-worker; checked again so
# the script is safe to register more broadly or run by hand.
agent_type=$(jq -r '.agent_type // ""' <<<"$input")
case "$agent_type" in
issue-worker | *:issue-worker) ;;
*) exit 0 ;;
esac

session=$(jq -r '.agent_id // .session_id // ""' <<<"$input")
msg=$(jq -r '.last_assistant_message // ""' <<<"$input")
if [ -z "$msg" ]; then
	transcript=$(jq -r '.agent_transcript_path // ""' <<<"$input")
	transcript="${transcript/#\~/$HOME}"
	if [ -n "$transcript" ] && [ -r "$transcript" ]; then
		msg=$(jq -rs '[.[] | select(.type == "assistant") | .message.content
			| if type == "string" then . else ([.[]? | select(.type == "text") | .text] | join("\n")) end
			| select(. != "")] | last // ""' "$transcript" 2>/dev/null) || msg=""
	fi
fi
if [ -z "$msg" ]; then
	note "no final message in the hook input or transcript; nothing recorded"
	exit 0
fi

# The origin issue and branch, from the hand-back's own header rather than the
# hook's cwd, which may be the root session's checkout rather than the worker's.
origin=$(sed -nE 's/^## Hand-back #([0-9]+).*/\1/p' <<<"$msg" | head -n1)
branch=$(sed -nE 's/^branch:[[:space:]]*([^[:space:]]+).*/\1/p' <<<"$msg" | head -n1)

# The bullets under "## Noticed", up to the next heading. Fence lines are
# dropped (a worker may wrap the whole hand-back in ```), continuation lines are
# joined onto their bullet, and `none` yields nothing.
mapfile -t bullets < <(awk '
	/^```/ { next }
	/^##[[:space:]]+Noticed[[:space:]]*$/ { in_sec = 1; found = 1; next }
	in_sec && /^#/ { in_sec = 0 }
	!in_sec { next }
	/^[-*][[:space:]]/ { if (cur != "") print cur; cur = $0; next }
	/^[[:space:]]+[^[:space:]]/ && cur != "" { sub(/^[[:space:]]+/, " "); cur = cur $0; next }
	/^[[:space:]]*$/ { next }
	{ if (cur != "") print cur; cur = ""; print "!" $0 }
	END { if (cur != "") print cur; if (!found) print "?" }
' <<<"$msg")

recorded=0 bad=0
for b in "${bullets[@]}"; do
	case "$b" in
	"?")
		note "no '## Noticed' section in the issue-worker hand-back${origin:+ for #$origin}; nothing recorded"
		exit 0
		;;
	"!"*)
		line="${b#!}"
		# A bare `none` (any case, optional period or backticks) is the empty list.
		if [[ "${line,,}" =~ ^[[:space:]\`]*none[[:space:]\`.]*$ ]]; then continue; fi
		note "not a Noticed bullet, skipped: $line"
		bad=$((bad + 1))
		continue
		;;
	esac
	body="${b#[-*]}"
	body="${body#"${body%%[![:space:]]*}"}"
	body="${body%"${body##*[![:space:]]}"}"
	if [[ "${body,,}" =~ ^\`?none\`?\.?$ ]]; then continue; fi
	if [[ "$body" != *" — "* ]] || ! [[ "$body" =~ \((bug|drift|follow-up)\)$ ]]; then
		note "bullet not in '- <path>:<line> — <why> (bug | drift | follow-up)' form, skipped: $b"
		bad=$((bad + 1))
		continue
	fi
	kind="${BASH_REMATCH[1]}"
	where="${body%%" — "*}"
	where="${where//\`/}"
	text="${body#*" — "}"
	text="${text%"($kind)"}"
	text="${text%"${text##*[![:space:]]}"}"
	if [ -z "$where" ] || [ -z "$text" ]; then
		note "bullet missing a location or text, skipped: $b"
		bad=$((bad + 1))
		continue
	fi
	args=(add --kind "$kind" --where "$where" --session "$session")
	[ -z "$origin" ] || args+=(--origin "$origin")
	[ -z "$branch" ] || args+=(--branch "$branch")
	if "$finding" "${args[@]}" -- "$text" >/dev/null; then
		recorded=$((recorded + 1))
	else
		note "finding.sh add failed for: $b"
		bad=$((bad + 1))
	fi
done

[ "$bad" -eq 0 ] || note "$recorded recorded, $bad skipped${origin:+ from #$origin}"
exit 0
