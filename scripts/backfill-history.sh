#!/usr/bin/env bash
# Rewrite main's history so every commit reads like one `make ship` lands
# today (#451): one commit per issue or PR, rebase-style, a message that
# passes scripts/check-commit-msg.sh, and one author identity on GitHub's
# noreply address. A one-off, kept for the record. It rewrites main, so the
# operator runs it by hand, following the runbook below; nothing here pushes.
#
#   backfill-history.sh [--dry-run] [--units <file>] [--messages <dir>] [--repo <dir>] [<rev>]
#   backfill-history.sh --apply <dir> [--units <file>] [<rev>]
#   backfill-history.sh --verify <old-head> <new-head> [--repo <dir>] [--map <file>]
#
# <rev> defaults to origin/main.
#
# --dry-run (the default) prints the report on stdout and changes no ref: one
# row per commit (old SHA, unit, fold group, old subject -> new subject, the
# identity change), the totals per kind, the folds, the number of issue blocks
# the new messages carry, the open issues they close, and the abbreviated SHAs
# in tracked files that the rewrite makes stale. --messages <dir> also writes each new message there.
#
# --apply <dir> makes a fresh mirror clone of origin at <dir> (BACKFILL_SOURCE
# overrides the URL), refuses unless its main is <rev>, replays the new
# history into it, and points its refs/heads/main at the new head and
# refs/heads/backup/main-pre-rewrite at the old one. It writes <dir>/commit-map
# (`<old sha> <new sha>` per old commit, oldest first; a folded-away commit
# maps to its fold's commit) and <dir>/report.txt (the dry run's report). The
# clone's remote.origin.mirror is unset, so a bare `git push` can't mirror it.
#
# --verify <old-head> <new-head> prints #451's Done-when checks, PASS or FAIL
# each, and exits 1 on any FAIL: (1) the commit-map lists the old history in
# order, its new commits are <new-head>'s oldest in order (later landings on
# top are allowed), and its folds are the report's; (2) every kept commit's tree equals its old counterpart's; (3) only
# the noreply and [bot] addresses in %ae/%ce; (4) no message has the personal
# address; (5) check-commit-msg.sh <sha> <n> passes on every issue-unit commit
# and --no-closes on every PR-unit commit; (6) the `<!-- ship:issue` count
# equals the report's. --repo is the repository holding both heads (the
# mirror); --map defaults to <repo>/commit-map, and the report sits beside it.
#
# --units <file> overrides the classification, one commit per line:
# `<sha prefix> issue <n>...`, `<sha prefix> pr <n>` or `<sha prefix> convention`.
# --repo <dir> reads another repository's history (dry run).
#
# GitHub answers are cached in <git common dir>/backfill-cache
# (BACKFILL_CACHE overrides it), so --apply builds the messages the dry run
# showed; delete it to re-read.
#
# 1. Classify each commit, oldest first, into a unit:
#    - issue: the issues (not PRs) its `Closes #<n>` lines name (at a line
#      start, outside code fences); else its merged PR's
#      closingIssuesReferences; else an inner `(#<n>)` in
#      `<subject> (#<n>) (#<m>)` that is an issue, not a PR.
#    - pr: a merged PR closing no issue, opened by no bot, whose subject
#      doesn't start `Bump `.
#    - convention: everything else (bot commits, Bump commits, no PR).
# 2. Fold adjacent commits of one unit (issue units sharing an issue, pr units
#    with the same PR) into one commit with the last one's tree. Two commits of
#    one unit that aren't adjacent make it refuse (exit 1): folding them would
#    reorder history.
# 3. Message:
#    - issue: the first issue's stripped title as subject; the old text as the
#      Plan (prefixed `## Plan` and the old subject when it has no `## Plan`
#      line), `## Test plan` with "Not recorded" when it has none; then one
#      scripts/issue-block.sh block per issue (one ship already added is kept
#      as it is), omitting the `Closes` lines the text already has, as ship
#      does; then the trailers.
#    - pr: the PR title without its `(#<n>)`; Plan and Test plan as above;
#      then a `<!-- ship:pr #<n> -->` block with `## PR #<n>: <title>` and the
#      PR's URL; then the trailers.
#    - convention: a conventional-commit subject (dependabot:
#      `build(deps): bump <dep> from <a> to <b>`; incus-os submodule and
#      base-image bumps: `build(incusos): ...`; else by the paths touched:
#      `.github/` ci, only `docs/` docs, `.devcontainer/` build(devcontainer),
#      else chore; the root commit is chore), and the old body.
#    - every message: the squash bullet repeating the PR title and the
#      `---------` line before squash trailers (the last one, with only blank
#      lines and trailers after it) go; the personal address becomes
#      the noreply one; `## ` lines are kept (whitespace cleanup only).
# 4. Identity: .mailmap maps every human author; the committer is
#    Emil Harvey <noreply> on every commit; dates and bot authors are kept.
# 5. Replay: `git commit-tree <old tree> -p <new parent> -F <msg>` per kept
#    commit, so tree equality holds by construction.
#
# Runbook (operator only; record each result in a comment on #493):
#   0. On a main that has this script: `git fetch origin`, then
#      `scripts/backfill-history.sh > "$SCRATCH/report.txt"`. Read it: the
#      folds, the totals, and the stale abbreviated SHAs.
#   1. Freeze: no open PRs and no in-flight branches (`/next`, `gh pr list`),
#      then `make wt-gc`. With no open PR, auto-rebase.yml has nothing to do.
#   2. Apply and verify, in a scratch directory:
#        scripts/backfill-history.sh --apply "$SCRATCH/mirror"
#        scripts/backfill-history.sh --verify <old main> <new main> --repo "$SCRATCH/mirror"
#      Every check must PASS.
#   3. Back up: `git -C "$SCRATCH/mirror" push origin refs/heads/backup/main-pre-rewrite`,
#      and keep the mirror. Record the backup SHA.
#   4. Force-push. The `main` ruleset (id 17956515) blocks non_fast_forward
#      and has no bypass actors:
#        gh api repos/{owner}/{repo}/rulesets/17956515 > "$SCRATCH/ruleset.json"
#      disable the rule or add yourself as a bypass actor, then
#        git -C "$SCRATCH/mirror" push --force-with-lease=main:<old main> origin main
#      restore the saved JSON (`gh api -X PUT ... --input`), fetch it again
#      and diff the two: the diff must be empty.
#   5. Record `git rev-list --count origin/main` before and after, and the
#      --verify output against the pushed head (run it in a fresh clone with
#      --map "$SCRATCH/mirror/commit-map"). Reopen any issue in the report's
#      "Open issues the new messages close" that the push closed.
#   6. Afterwards every worktree and clone resets to the new main. A branch
#      made before the rewrite rebases with
#      `git rebase --onto origin/main <old main sha>`. ci.yml's landed-identity
#      job on the force-push's run checks the whole new history, and the first
#      PR landed after it must have `git log -1 --format=%ce` the noreply
#      address; link both runs in a comment on #493.
#
# Known costs, accepted by running it: every SHA changes and the count drops by
# the folds; GitHub's signatures on the squash commits are lost; merged PRs
# and issue cross-references keep pointing at the old commits; ghcr.io images
# tagged with old SHAs stay as they are (ci.yml republishes latest); and the
# abbreviated SHAs the report lists go stale. Each issue block is the issue as
# it reads at the rewrite (#451 wants them byte-identical to ship's), so a
# block, and its subject, can describe scope rescoped after that commit landed;
# the Plan above it is the commit's own record. Unverified, expected: GitHub
# reads the new commits' `Closes #n` and `#n` references as new, so the push
# may close the open issues the report lists and add an "added a commit that
# referenced this issue" event to every issue and PR the messages name.
set -euo pipefail

NAME="Emil Harvey"
NOREPLY="36806437+ehharvey@users.noreply.github.com"
PERSONAL="emil.h.harvey@gmail.com"
NOT_RECORDED="Not recorded: landed before #119 made the Plan/Test plan sections the rule."

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
top=$(cd "$here/.." && pwd)

usage() {
	sed -n '/^#   backfill-history.sh/s/^#   /usage: /p' "${BASH_SOURCE[0]}" >&2
	exit 2
}
die() {
	echo "backfill-history: $*" >&2
	exit 1
}

mode=dry-run apply_dir="" units_file="" msg_dir="" repo="" map="" rev="" old_head="" new_head=""
while [[ $# -gt 0 ]]; do
	case $1 in
	--dry-run) mode=dry-run ;;
	--apply)
		[[ $# -ge 2 ]] || usage
		mode=apply apply_dir=$2
		shift
		;;
	--verify)
		[[ $# -ge 3 ]] || usage
		mode=verify old_head=$2 new_head=$3
		shift 2
		;;
	--units)
		[[ $# -ge 2 ]] || usage
		units_file=$2
		shift
		;;
	--messages)
		[[ $# -ge 2 ]] || usage
		msg_dir=$2
		shift
		;;
	--repo)
		[[ $# -ge 2 ]] || usage
		repo=$2
		shift
		;;
	--map)
		[[ $# -ge 2 ]] || usage
		map=$2
		shift
		;;
	-*) usage ;;
	*)
		[[ -z $rev ]] || usage
		rev=$1
		;;
	esac
	shift
done
rev=${rev:-origin/main}
src_repo=${repo:-$top}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

g() { git -C "$src_repo" "$@"; }

cache=${BACKFILL_CACHE:-$(git -C "$top" rev-parse --path-format=absolute --git-common-dir)/backfill-cache}

gh_ready=""
need_gh() {
	[[ -n $gh_ready ]] && return
	GH_REPO=${GH_REPO:-$(cd "$top" && gh repo view --json nameWithOwner --jq .nameWithOwner)}
	export GH_REPO
	gh_ready=1
}

# cached <key> <cmd>...: the command's stdout, run once per key.
cached() {
	local f="$cache/$1"
	shift
	if [[ ! -f $f ]]; then
		mkdir -p "${f%/*}"
		if ! "$@" >"$f.tmp"; then
			rm -f "$f.tmp"
			return 1
		fi
		mv "$f.tmp" "$f"
	fi
	cat "$f"
}

# issue_markers: the <n> of each `<!-- ship:issue #<n> -->` line of the
# message (stdin) outside code fences.
issue_markers() {
	awk '/^ ? ? ?(```|~~~)/ { f = !f; next }
		!f && /^<!-- ship:issue #[0-9]+ -->$/ { print substr($0, 18) + 0 }'
}

# ---------------------------------------------------------------------------
# --verify
# ---------------------------------------------------------------------------
if [[ $mode == verify ]]; then
	if [[ -n $repo ]]; then
		GIT_DIR=$(git -C "$repo" rev-parse --absolute-git-dir)
		export GIT_DIR
	fi
	gitdir=$(git rev-parse --absolute-git-dir)
	map=${map:-$gitdir/commit-map}
	report="$(dirname "$map")/report.txt"
	[[ -f $map ]] || die "no commit-map at $map (--map)"
	old_head=$(git rev-parse --verify "$old_head^{commit}") || die "no commit $old_head"
	new_head=$(git rev-parse --verify "$new_head^{commit}") || die "no commit $new_head"
	need_gh
	fails=0
	result() { # result <ok 0|1> <label> [detail]
		if [[ $1 -eq 0 ]]; then
			echo "PASS: $2"
		else
			echo "FAIL: $2${3:+ — $3}"
			fails=$((fails + 1))
		fi
	}

	git rev-list --reverse "$old_head" >"$work/old"
	git rev-list --reverse "$new_head" >"$work/new"
	n_old=$(wc -l <"$work/old")
	n_new=$(wc -l <"$work/new")
	cut -d' ' -f1 "$map" >"$work/map-old"
	cut -d' ' -f2 "$map" | awk '!s[$0]++' >"$work/map-new"
	folds=$((n_old - $(wc -l <"$work/map-new")))

	# (1) The map covers the old history in order, and its new commits are the
	# new head's oldest, in order: the pushed head may have commits on top of
	# the rewrite. The folds the map implies must be the report's.
	ok=0 why=""
	n_rw=$(wc -l <"$work/map-new")
	cmp -s "$work/old" "$work/map-old" || ok=1 why="the commit-map doesn't list the old history in order"
	head -n "$n_rw" "$work/new" | cmp -s - "$work/map-new" ||
		ok=1 why="${why:+$why; }the commit-map's new commits aren't the new head's oldest, in order"
	want_folds=""
	[[ ! -f $report ]] || want_folds=$(sed -n 's/^folds: \([0-9]*\)$/\1/p' "$report")
	[[ $folds -eq ${want_folds:--1} ]] || ok=1 why="${why:+$why; }the report's folds: ${want_folds:-none}"
	result $ok "(1) count: $n_old old - $folds folded = $n_rw rewritten (+$((n_new - n_rw)) landed since)" "$why"

	# (2) the kept commit of a fold is the last old one mapping to it.
	bad=0 checked=0
	while read -r o n; do
		[[ $(git rev-parse "$o^{tree}") == "$(git rev-parse "$n^{tree}")" ]] || {
			echo "      tree differs: ${o:0:7} -> ${n:0:7}"
			bad=1
		}
		checked=$((checked + 1))
	done < <(tac "$map" | awk '!s[$2]++' | tac)
	result $bad "(2) every kept commit's tree equals its old counterpart's ($checked checked)"

	# (3)
	others=$(git log --format='%ae%n%ce' "$new_head" | sort -u |
		grep -vxF "$NOREPLY" | grep -vE '^[0-9]+\+[^@]+\[bot\]@users\.noreply\.github\.com$' || true)
	ok=0
	[[ -z $others ]] || ok=1
	result $ok "(3) only the noreply and [bot] addresses in %ae/%ce" "$(tr '\n' ' ' <<<"$others")"

	# (4)
	hits=$(git log --format='%h %B' "$new_head" | grep -cF "$PERSONAL" || true)
	ok=0
	[[ $hits -eq 0 ]] || ok=1
	result $ok "(4) no message contains $PERSONAL" "$hits lines do"

	# (5)
	bad=0 issues=0 prs=0 blocks=0
	while read -r c; do
		msg=$(git log -1 --format=%B "$c")
		mapfile -t nums < <(issue_markers <<<"$msg")
		blocks=$((blocks + ${#nums[@]}))
		if [[ ${#nums[@]} -gt 0 ]]; then
			issues=$((issues + 1))
			for k in "${nums[@]}"; do
				out=$("$here/check-commit-msg.sh" "$c" "$k" 2>&1) || {
					echo "      ${c:0:7} #$k: $(tr '\n' ';' <<<"$out")"
					bad=1
				}
			done
		elif grep -qE '^<!-- ship:pr #[0-9]+ -->$' <<<"$msg"; then
			prs=$((prs + 1))
			out=$("$here/check-commit-msg.sh" --no-closes "$c" 2>&1) || {
				echo "      ${c:0:7} (PR): $(tr '\n' ';' <<<"$out")"
				bad=1
			}
		fi
	done <"$work/map-new"
	result $bad "(5) check-commit-msg.sh passes on $issues issue-unit commits (<sha> <n>) and $prs PR-unit commits (--no-closes)"

	# (6)
	if [[ -f $report ]]; then
		want=$(sed -n 's/^issue blocks: \([0-9]*\)$/\1/p' "$report")
		ok=0
		[[ $blocks -eq ${want:--1} ]] || ok=1
		result $ok "(6) <!-- ship:issue blocks: $blocks, the report's: ${want:-none}"
	else
		result 1 "(6) <!-- ship:issue blocks: $blocks" "no report at $report"
	fi
	[[ $fails -eq 0 ]] || exit 1
	exit 0
fi

# ---------------------------------------------------------------------------
# Classify
# ---------------------------------------------------------------------------
old_head=$(g rev-parse --verify "$rev^{commit}") || die "no commit $rev"
mapfile -t SHA < <(g rev-list --reverse "$old_head")
[[ $(g rev-list --merges --count "$old_head") -eq 0 ]] || die "$rev's history has merge commits; this replays linear history only"
N=${#SHA[@]}

declare -A OVERRIDE=()
if [[ -n $units_file ]]; then
	while read -r p rest; do
		[[ -z $p || $p == \#* ]] && continue
		full=$(g rev-parse --verify --quiet "$p^{commit}") || die "--units: no commit $p"
		OVERRIDE[$full]=$rest
	done <"$units_file"
fi

# closes_lines: the `Closes #<n>` numbers at a line start outside fences in
# the message (stdin), first occurrence order. Narrower than
# check-commit-msg.sh --closes on purpose: prose that quotes a `Closes` mid-line
# (#401's commit says `has no "Closes #92".`) doesn't make a commit part of that unit.
# The omit list keeps --closes's rule, as ship does; it only drops lines from
# the unit's own issues' blocks, which --verify (5) then checks.
closes_lines() {
	awk '/^ ? ? ?(```|~~~)/ { f = !f; next }
		!f && match($0, /^Closes #[0-9]+/) { n = substr($0, 9, RLENGTH - 8); if (!s[n]++) print n }'
}

# pr_of <sha>: `<number> <user type> <login> <title> <url>`, US-separated, of
# the merged PR into main that holds the commit, or nothing. US, not @tsv:
# @tsv escapes `\` and tabs, and a tab IFS collapses empty fields.
US=$'\x1f'
pr_fields='[.number, .user.type, .user.login, .title, .html_url] | map(tostring) | join("\u001f")'
pr_of() {
	cached "pulls/$1" gh api "repos/{owner}/{repo}/commits/$1/pulls" |
		jq -r "[.[] | select(.merged_at != null and .base.ref == \"main\")]
			| sort_by(.merged_at) | first // empty | $pr_fields"
}
pr_by_number() {
	cached "pull/$1" gh api "repos/{owner}/{repo}/pulls/$1" | jq -r "$pr_fields"
}
pr_closing() {
	cached "closing/$1" gh pr view "$1" --json closingIssuesReferences \
		--jq '[.closingIssuesReferences[].number] | join(" ")'
}
is_issue() {
	[[ $(cached "isissue/$1" gh api "repos/{owner}/{repo}/issues/$1" --jq 'has("pull_request") | not') == true ]]
}

# The new author: .mailmap applied, from this checkout's copy.
new_author() { (cd "$top" && git check-mailmap "$1 <$2>"); }

inner_re='\(#([0-9]+)\) \(#[0-9]+\)$'
declare -a KIND NUMS SUBJ PRN PRT PRU PRBOT AE AN CE CN NA
for ((i = 0; i < N; i++)); do
	c=${SHA[i]}
	g log -1 --format=%B "$c" >"$work/old.$i"
	SUBJ[i]=$(head -n1 "$work/old.$i")
	IFS=$'\t' read -r "AN[i]" "AE[i]" "CN[i]" "CE[i]" < <(g log -1 --format=$'%an\t%ae\t%cn\t%ce' "$c")
	NA[i]=$(new_author "${AN[i]}" "${AE[i]}")
	PRN[i]="" PRT[i]="" PRU[i]="" PRBOT[i]=""
	ov=${OVERRIDE[$c]:-}
	if [[ -n $ov ]]; then
		read -r kind nums <<<"$ov"
		case $kind in
		issue | convention) ;;
		pr)
			need_gh
			IFS=$US read -r "PRN[i]" _ _ "PRT[i]" "PRU[i]" < <(pr_by_number "$nums")
			;;
		*) die "--units: unknown kind '$kind' for ${c:0:7}" ;;
		esac
		KIND[i]=$kind NUMS[i]=${nums:-}
		continue
	fi
	need_gh
	# A `Closes #<n>` naming a PR closes nothing, so it can't make a unit.
	closes=""
	for x in $(closes_lines <"$work/old.$i"); do
		is_issue "$x" && closes+="$x "
	done
	pr=$(pr_of "$c")
	if [[ -n $pr ]]; then
		IFS=$US read -r "PRN[i]" type login "PRT[i]" "PRU[i]" <<<"$pr"
		if [[ $type == Bot || $login == *"[bot]" ]]; then PRBOT[i]=1; fi
	fi
	KIND[i]=convention NUMS[i]=""
	if [[ -n ${closes// /} ]]; then
		KIND[i]=issue NUMS[i]=${closes% }
	elif [[ -n ${PRN[i]} ]] && refs=$(pr_closing "${PRN[i]}") && [[ -n $refs ]]; then
		KIND[i]=issue NUMS[i]=$refs
	elif [[ ${SUBJ[i]} =~ $inner_re ]] && is_issue "${BASH_REMATCH[1]}"; then
		KIND[i]=issue NUMS[i]=${BASH_REMATCH[1]}
	elif [[ -n ${PRN[i]} && -z ${PRBOT[i]} && ${SUBJ[i]} != Bump\ * ]]; then
		KIND[i]=pr NUMS[i]=${PRN[i]}
	fi
done

# ---------------------------------------------------------------------------
# Fold
# ---------------------------------------------------------------------------
declare -a GROUP GFIRST GLAST GKIND GNUMS
ng=0
shares() { # shares "<a list>" "<b list>"
	local x y
	for x in $1; do for y in $2; do [[ $x == "$y" ]] && return 0; done; done
	return 1
}
for ((i = 0; i < N; i++)); do
	if ((i > 0)) && [[ ${KIND[i]} == "${GKIND[ng - 1]}" && ${KIND[i]} != convention ]] &&
		shares "${NUMS[i]}" "${GNUMS[ng - 1]}"; then
		GROUP[i]=$((ng - 1)) GLAST[ng - 1]=$i
		for x in ${NUMS[i]}; do
			shares "$x" "${GNUMS[ng - 1]}" || GNUMS[ng - 1]+=" $x"
		done
		continue
	fi
	GROUP[i]=$ng GFIRST[ng]=$i GLAST[ng]=$i GKIND[ng]=${KIND[i]} GNUMS[ng]=${NUMS[i]}
	ng=$((ng + 1))
done

# A unit in two groups would need folding across the commits between them.
declare -A OWNER=()
refuse=0
for ((k = 0; k < ng; k++)); do
	[[ ${GKIND[k]} == convention ]] && continue
	for x in ${GNUMS[k]}; do
		key="${GKIND[k]} #$x"
		if [[ -n ${OWNER[$key]:-} && ${OWNER[$key]} != "$k" ]]; then
			o=${OWNER[$key]}
			a=${SHA[${GLAST[o]}]} b=${SHA[${GFIRST[k]}]}
			echo "backfill-history: $key is in ${a:0:7} and ${b:0:7}, which aren't adjacent; folding them would reorder history. Refusing (fix with --units)." >&2
			refuse=1
		fi
		OWNER[$key]=$k
	done
done
[[ $refuse -eq 0 ]] || exit 1

# ---------------------------------------------------------------------------
# Messages
# ---------------------------------------------------------------------------
# last_para head|tail: ship.sh's: the text (stdin) before its last paragraph,
# or that paragraph.
last_para() {
	awk -v part="$1" '
		{ l[++n] = $0 }
		END {
			while (n > 0 && l[n] ~ /^[ \t]*$/) n--
			for (b = n; b > 0 && l[b] !~ /^[ \t]*$/; b--) ;
			if (part == "head") for (i = 1; i < b; i++) print l[i]
			else for (i = b + 1; i <= n; i++) print l[i]
		}'
}

strip_pr_suffix() { sed -E 's/[[:space:]]*\(#[0-9]+\)$//' <<<"$1"; }

# split_old <i>: sets BODY (the text below the subject, squash scaffolding
# dropped), AUTH (BODY minus trailers and ship's issue block), TRAIL (the
# trailer paragraph) and BLOCK (the block ship added, if any, with its issue
# in BLOCKN).
split_old() {
	local i=$1 s1 s2
	s1=$(strip_pr_suffix "${SUBJ[i]}")
	s2=$(sed -E ':a; s/[[:space:]]*\(#[0-9]+\)$//; ta' <<<"${SUBJ[i]}")
	# GitHub's squash separator is the last `---------` line, with only blank
	# lines and trailers after it; one anywhere else is the author's.
	BODY=$(tail -n +2 "$work/old.$i" | awk -v a="* ${PRT[i]}" -v b="* $s1" -v c="* $s2" '
		!done && ($0 == a || $0 == b || $0 == c) { done = 1; next }
		{ l[++n] = $0 }
		$0 == "---------" { sep = n }
		sep && n > sep && $0 !~ /^[ \t]*$/ && $0 !~ /^[A-Za-z0-9-]+: / { sep = 0 }
		END { for (i = 1; i <= n; i++) if (i != sep) print l[i] }')
	AUTH=$BODY TRAIL=""
	if [[ -n $(git interpret-trailers --parse --no-divider <<<"${SUBJ[i]}"$'\n\n'"$BODY") ]]; then
		AUTH=$(last_para head <<<"$BODY")
		TRAIL=$(last_para tail <<<"$BODY")
	fi
	# One pass, by ship.sh's rule: the block (first marker outside a fence to
	# the last end marker) to $work/block, the rest to stdout.
	AUTH=$(awk -v out="$work/block" '
		{ l[++n] = $0 }
		!s && /^ ? ? ?(```|~~~)/ { f = !f }
		!s && !f && /^<!-- ship:issue #[0-9]+ -->$/ { s = n }
		s && $0 == "<!-- /ship:issue -->" { e = n }
		END {
			if (!e) s = 0
			printf "" > out
			for (i = 1; i <= n; i++)
				if (s && i >= s && i <= e) print l[i] > out
				else print l[i]
		}' <<<"$AUTH")
	BLOCK=$(<"$work/block")
	BLOCKN=""
	[[ -z $BLOCK ]] || BLOCKN=$(issue_markers <<<"$BLOCK" | head -n1)
}

# sections <subject> <text>: the text with `## Plan` (and the subject) put
# first when it has no such line, and `## Test plan` added when it has none.
# The rule is check-commit-msg.sh's, so the check passes on what this writes.
sections() {
	local out=$2
	grep -qE '^## Plan[[:space:]]*$' <<<"$out" || out="## Plan"$'\n\n'"$1"$'\n\n'"$out"
	grep -qE '^## Test plan[[:space:]]*$' <<<"$out" || out+=$'\n\n'"## Test plan"$'\n\n'"$NOT_RECORDED"
	printf '%s' "$out"
}

conv_subject() {
	local i=$1 s paths type
	local bump_re='^Bump (lxc/incus-os submodule|the IncusOS base image)'
	local conv_re='^[a-z]+(\([^)]*\))?!?: '
	s=$(sed -E ':a; s/[[:space:]]*\(#[0-9]+\)$//; ta' <<<"${SUBJ[i]}")
	if [[ ${AE[i]} == *"dependabot[bot]@"* ]]; then
		local re='^Bumps \[([^]]+)\]\([^)]*\) from ([^ ]+) to ([^ ]+)$'
		local line
		line=$(grep -m1 -E '^Bumps \[' "$work/old.$i" || true)
		if [[ $line =~ $re ]]; then
			echo "build(deps): bump ${BASH_REMATCH[1]} from ${BASH_REMATCH[2]} to ${BASH_REMATCH[3]%.}"
			return
		fi
		s=${s#build(deps): }
		echo "build(deps): ${s,}"
		return
	fi
	if [[ $s =~ $bump_re ]]; then
		echo "build(incusos): ${s,}"
		return
	fi
	if [[ $s =~ $conv_re ]]; then
		echo "$s"
		return
	fi
	paths=$(g diff-tree --no-commit-id --name-only -r --root "${SHA[i]}")
	# The root commit adds everything, so its paths say nothing.
	if ((i == 0)); then
		type=chore
	elif grep -q '^\.github/' <<<"$paths"; then
		type=ci
	elif ! grep -qv '^docs/' <<<"$paths"; then
		type=docs
	elif grep -q '^\.devcontainer/' <<<"$paths"; then
		type="build(devcontainer)"
	else
		type=chore
	fi
	echo "$type: $s"
}

declare -a NEWSUBJ
blocks_total=0
for ((k = 0; k < ng; k++)); do
	f=${GFIRST[k]} l=${GLAST[k]}
	if [[ ${GKIND[k]} == convention ]]; then
		split_old "$f"
		NEWSUBJ[k]=$(conv_subject "$f")
		msg=${NEWSUBJ[k]}
		[[ -z $(tr -d '[:space:]' <<<"$BODY") ]] || msg+=$'\n\n'$BODY
	else
		text="" trailers="" existing="" have=" "
		for ((i = f; i <= l; i++)); do
			split_old "$i"
			if ((i == f)); then
				text=$AUTH
			else
				text+=$'\n\n'"${SUBJ[i]}"
				[[ -z $(tr -d '[:space:]' <<<"$AUTH") ]] || text+=$'\n\n'$AUTH
			fi
			[[ -z $TRAIL ]] || trailers+=$TRAIL$'\n'
			if [[ -n $BLOCK ]]; then
				existing+=$BLOCK$'\n\n'
				have+="$BLOCKN "
			fi
		done
		trailers=$(awk 'NF && !s[$0]++' <<<"$trailers")
		text=$(sections "${SUBJ[f]}" "$(git stripspace <<<"$text")")
		blocks=${existing%$'\n\n'}
		nb=$(issue_markers <<<"$existing" | wc -l)
		if [[ ${GKIND[k]} == issue ]]; then
			omit=() okey=""
			for x in $("$here/check-commit-msg.sh" --closes - <<<"$text"$'\n'"$trailers"); do
				omit+=(--omit-closes "$x")
				okey+="-$x"
			done
			first=""
			for x in ${GNUMS[k]}; do
				out=$(cached "block/$x$okey" "$here/issue-block.sh" "${omit[@]}" "$x") ||
					die "issue-block.sh #$x failed"
				[[ -n $first ]] || first=${out%%$'\n'*}
				[[ $have == *" $x "* ]] && continue
				[[ -z $blocks ]] || blocks+=$'\n\n'
				blocks+=${out#*$'\n'}
				nb=$((nb + 1))
			done
			NEWSUBJ[k]=$first
		else
			p=${GNUMS[k]}
			title=$(strip_pr_suffix "${PRT[f]}")
			NEWSUBJ[k]=$title
			blocks="<!-- ship:pr #$p -->"$'\n'"## PR #$p: $title"$'\n\n'"${PRU[f]}"$'\n'"<!-- /ship:pr -->"
		fi
		blocks_total=$((blocks_total + nb))
		msg=${NEWSUBJ[k]}$'\n\n'$text$'\n\n'$blocks
		[[ -z $trailers ]] || msg+=$'\n\n'$trailers
	fi
	git stripspace <<<"${msg//"$PERSONAL"/"$NOREPLY"}" >"$work/msg.$k"
	NEWSUBJ[k]=$(head -n1 "$work/msg.$k")
	# What GitHub may act on: --closes's rule, which is GitHub's, not the
	# classification's.
	[[ ${GKIND[k]} != issue ]] || "$here/check-commit-msg.sh" --closes - <"$work/msg.$k" >>"$work/closed"
	if [[ -n $msg_dir ]]; then
		mkdir -p "$msg_dir"
		cp "$work/msg.$k" "$msg_dir/$(printf '%03d' "$k")-${SHA[l]:0:7}.txt"
	fi
done

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------
report() {
	local i k fold kind ident na
	echo "# backfill-history report for $rev ($old_head)"
	echo "# old sha  unit  fold  old subject -> new subject  [identity]"
	for ((i = 0; i < N; i++)); do
		k=${GROUP[i]}
		fold=-
		((GFIRST[k] == GLAST[k])) || fold="fold:$k"
		case ${KIND[i]} in
		issue) kind="issue #${NUMS[i]// /,#}" ;;
		pr) kind="pr #${NUMS[i]}" ;;
		*) kind=convention ;;
		esac
		ident=""
		na=${NA[i]}
		[[ $na == "${AN[i]} <${AE[i]}>" ]] || ident+="author ${AN[i]} <${AE[i]}> -> $na; "
		[[ "${CN[i]} <${CE[i]}>" == "$NAME <$NOREPLY>" ]] || ident+="committer ${CN[i]} <${CE[i]}> -> $NAME <$NOREPLY>"
		ident=${ident%; }
		local newsubj=${NEWSUBJ[k]}
		((i == GLAST[k])) || newsubj="(folded into ${SHA[GLAST[k]]:0:7})"
		printf '%s  %s  %s  %s -> %s  [%s]\n' "${SHA[i]:0:7}" "$kind" "$fold" "${SUBJ[i]}" "$newsubj" "${ident:-unchanged}"
	done
	echo
	echo "## Totals"
	for kind in issue pr convention; do
		local nc=0 nu=0
		for ((i = 0; i < N; i++)); do [[ ${KIND[i]} == "$kind" ]] && nc=$((nc + 1)); done
		for ((k = 0; k < ng; k++)); do [[ ${GKIND[k]} == "$kind" ]] && nu=$((nu + 1)); done
		echo "$kind: $nc commits, $nu units"
	done
	echo "commits: $N old, $ng new"
	echo "folds: $((N - ng))"
	for ((k = 0; k < ng; k++)); do
		((GFIRST[k] == GLAST[k])) && continue
		local list=""
		for ((i = GFIRST[k]; i <= GLAST[k]; i++)); do list+="${SHA[i]:0:7} "; done
		echo "  fold:$k ${GKIND[k]} #${GNUMS[k]// /,#}: ${list% }"
	done
	echo "issue blocks: $blocks_total"
	echo
	# Read live, not cached: the state on the day of the push is what counts.
	echo "## Open issues the new messages close (GitHub may close them again on the push)"
	if [[ -s $work/closed ]]; then
		need_gh
		gh issue list --state open --limit 1000 --json number --jq '.[].number' |
			sort -u | comm -12 - <(sort -u "$work/closed") | sort -n | sed 's/^/  #/'
	fi
	echo
	echo "## Abbreviated SHAs in tracked files at $rev"
	g rev-list "$old_head" >"$work/all"
	g grep -nowE '[0-9a-f]{7,40}' "$old_head" -- . 2>/dev/null |
		awk -F: 'NR == FNR { for (n = 7; n <= 40; n++) p[substr($0, 1, n)] = 1; next }
			($NF in p) { print "  " $2 ":" $3 " (" $NF ")" }' "$work/all" - || true
}

report >"$work/report"

if [[ $mode == dry-run ]]; then
	cat "$work/report"
	exit 0
fi

# ---------------------------------------------------------------------------
# --apply
# ---------------------------------------------------------------------------
[[ ! -e $apply_dir ]] || die "$apply_dir exists; --apply makes a fresh mirror clone"
src=${BACKFILL_SOURCE:-$(git -C "$top" remote get-url origin)}
git clone -q --mirror "$src" "$apply_dir"
apply_dir=$(cd "$apply_dir" && pwd)
export GIT_DIR=$apply_dir
git config --unset remote.origin.mirror
[[ $(git rev-parse refs/heads/main) == "$old_head" ]] ||
	die "$src's main is $(git rev-parse --short refs/heads/main), not $rev ($old_head); fetch and re-run"

: >"$apply_dir/commit-map"
parent=""
export GIT_COMMITTER_NAME=$NAME GIT_COMMITTER_EMAIL=$NOREPLY
for ((k = 0; k < ng; k++)); do
	l=${GLAST[k]}
	c=${SHA[l]}
	# A fold takes its last commit's identity and dates, with its tree.
	na=${NA[l]}
	GIT_AUTHOR_NAME=${na% <*}
	GIT_AUTHOR_EMAIL=${na##*<}
	GIT_AUTHOR_EMAIL=${GIT_AUTHOR_EMAIL%>}
	GIT_AUTHOR_DATE=$(git log -1 --date=raw --format=%ad "$c")
	GIT_COMMITTER_DATE=$(git log -1 --date=raw --format=%cd "$c")
	export GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_AUTHOR_DATE GIT_COMMITTER_DATE
	parents=()
	[[ -z $parent ]] || parents=(-p "$parent")
	new=$(git commit-tree "$c^{tree}" "${parents[@]}" -F "$work/msg.$k")
	for ((i = GFIRST[k]; i <= l; i++)); do echo "${SHA[i]} $new" >>"$apply_dir/commit-map"; done
	parent=$new
done
git update-ref refs/heads/backup/main-pre-rewrite "$old_head" ""
git update-ref refs/heads/main "$parent" "$old_head"
cp "$work/report" "$apply_dir/report.txt"

cat <<EOF
Replayed $N commits as $ng into $apply_dir:
  refs/heads/main                      $parent (new)
  refs/heads/backup/main-pre-rewrite   $old_head (old)
  commit-map, report.txt
Next (runbook step 2):
  $0 --verify $old_head $parent --repo $apply_dir
EOF
