#!/usr/bin/env bash
# The mechanical audit of one issue branch (#272). /review-work, /run-chain §3
# and /ship-issue steps 1–4 all run it, so these checks live here and nowhere
# else; a copy in a SKILL.md would drift from this one.
#
#   audit.sh              # the current checkout (what /ship-issue runs)
#   audit.sh 281          # PR #281: its head against its base
#   audit.sh <path>       # the branch checked out in that worktree
#   audit.sh <branch>     # a local branch, else origin/<branch>
#
# The target is a PR when it is all digits (a leading `#` is allowed), a
# worktree when it is an existing directory, and a branch otherwise.
#
# The base is the PR's base branch. A branch or worktree uses the base of its
# open PR when it has one, and origin/main when it has none (it hasn't shipped,
# so it has no CI checks either; the output says so). Counting against the base
# rather than main keeps the check right for stacked PRs (#254).
#
# A merged or closed PR is refused: review is for work that hasn't landed. A
# branch whose PR already merged is refused the same way.
#
# Checks (a `FAIL` line each): the branch is eharvey/#<n>; the checkout is
# clean; exactly one commit beyond the base; the message has `## Plan`,
# `## Test plan` and `Closes #<n>`; no bootstrap-output/ or *.img committed.
# Then, for the calling skill to judge (nothing here decides whether the work
# is done): the facts its convention checks need, the commit message, the
# --stat, the issue's `### Done when` section and the Roadmap lines naming #<n>.
# Every section is bounded, and no diff is ever printed.
#
# Exit: 0 no mechanical gap; 1 a gap, or a refused PR; 2 usage, or no such target.
set -euo pipefail

usage() {
	echo "usage: audit.sh [<pr> | <worktree path> | <branch>]" >&2
	exit 2
}
[[ $# -le 1 ]] || usage
target=${1:-.}

gaps=0
ok() { printf 'ok    %s\n' "$*"; }
gap() {
	printf 'FAIL  %s\n' "$*"
	gaps=$((gaps + 1))
}
refuse() {
	printf 'REFUSED  %s\n' "$*"
	exit 1
}
# At most $1 lines of stdin, then a note of how many were cut.
bound() { awk -v max="$1" 'NR <= max { print } END { if (NR > max) printf "  … %d more lines\n", NR - max }'; }

# The worktree that has refs/heads/$1 checked out, if any.
worktree_of() {
	git worktree list --porcelain | awk -v ref="refs/heads/$1" '
		/^worktree / { wt = substr($0, 10) }
		$0 == "branch " ref { print wt; exit }'
}

pr="" state="" branch="" base_ref="" head="" wt=""

if [[ $target =~ ^#?[0-9]+$ ]]; then
	pr=${target#\#}
	info=$(gh pr view "$pr" --json state,headRefName,baseRefName \
		--jq '[.state, .headRefName, .baseRefName] | @tsv' 2>/dev/null) ||
		{
			echo "audit: no PR #$pr" >&2
			exit 2
		}
	IFS=$'\t' read -r state branch base_ref <<<"$info"
	[[ $state == OPEN ]] || refuse "PR #$pr is $state: review is for open PRs only"
	cd "$(git rev-parse --show-toplevel)"
	git fetch -q origin "pull/$pr/head"
	head=$(git rev-parse FETCH_HEAD)
	wt=$(worktree_of "$branch")
elif [[ -d $target ]]; then
	wt=$(git -C "$target" rev-parse --show-toplevel 2>/dev/null) ||
		{
			echo "audit: $target is not inside a git checkout" >&2
			exit 2
		}
	cd "$wt"
	branch=$(git branch --show-current)
	head=$(git rev-parse HEAD)
else
	branch=$target
	cd "$(git rev-parse --show-toplevel)"
	if head=$(git rev-parse -q --verify "refs/heads/$branch^{commit}"); then
		wt=$(worktree_of "$branch")
	elif git fetch -q origin "refs/heads/$branch" 2>/dev/null; then
		head=$(git rev-parse FETCH_HEAD)
	else
		# Merging deletes the branch, so ask GitHub before calling it unknown.
		merged=$(gh pr list --head "$branch" --state merged --limit 1 --json number --jq '.[0].number // empty')
		[[ -z $merged ]] || refuse "$branch already merged as PR #$merged"
		echo "audit: no PR, directory or branch named $target" >&2
		exit 2
	fi
fi

# A branch or worktree: find its PR. An open one sets the base; a merged one
# means there's nothing left to review.
if [[ -z $pr && -n $branch ]]; then
	info=$(gh pr list --head "$branch" --state all --limit 20 --json number,state,baseRefName \
		--jq '(map(select(.state == "OPEN")) + map(select(.state == "MERGED")))[0]
			| select(. != null) | [.number, .state, .baseRefName] | @tsv')
	if [[ -n $info ]]; then
		IFS=$'\t' read -r pr state base_ref <<<"$info"
		[[ $state == OPEN ]] || refuse "$branch already merged as PR #$pr"
	fi
fi

base_ref=${base_ref:-main}
git fetch -q origin "refs/heads/$base_ref"
base=$(git rev-parse FETCH_HEAD)

n=""
if [[ $branch =~ ^eharvey/#([0-9]+)$ ]]; then n=${BASH_REMATCH[1]}; fi

echo "== Target =="
if [[ -n $pr ]]; then
	echo "PR:       #$pr (open), base $base_ref"
else
	echo "PR:       none, so no CI checks; base origin/main"
fi
echo "branch:   ${branch:-(detached HEAD)}"
echo "issue:    ${n:+#$n}"
echo "head:     $(git rev-parse --short "$head")"
echo "base:     origin/$base_ref @ $(git rev-parse --short "$base")"
echo "checkout: ${wt:-none}"

echo
echo "== Checks =="
if [[ -n $n ]]; then
	ok "branch is $branch"
else
	gap "branch is '${branch:-(detached HEAD)}', not eharvey/#<n>"
fi

if [[ -z $wt ]]; then
	echo "n/a   clean tree: no local checkout of $branch"
else
	dirty=$(git -C "$wt" status --porcelain)
	if [[ -z $dirty ]]; then
		ok "clean tree in $wt"
	else
		gap "uncommitted changes in $wt:"
		bound 20 <<<"$dirty" | sed 's/^/        /'
	fi
	local_head=$(git -C "$wt" rev-parse HEAD)
	if [[ -n $pr && $local_head != "$head" ]]; then
		gap "$wt is at $(git rev-parse --short "$local_head") but PR #$pr's head is $(git rev-parse --short "$head"): push or pull first"
	fi
fi

count=$(git rev-list --count "$base..$head")
if [[ $count -eq 1 ]]; then
	ok "one commit beyond origin/$base_ref"
else
	gap "$count commits beyond origin/$base_ref; it takes exactly 1"
	if [[ $count -gt 1 ]]; then
		echo "        squash: git fetch origin $base_ref && git reset --soft FETCH_HEAD && git commit -c HEAD@{1}"
	fi
fi

# With no commit beyond the base, HEAD's message is the base's: don't judge it.
msg=""
if [[ $count -gt 0 ]]; then
	msg=$(git log -1 --format=%B "$head")
	missing=()
	grep -qE '^## Plan[[:space:]]*$' <<<"$msg" || missing+=("## Plan")
	grep -qE '^## Test plan[[:space:]]*$' <<<"$msg" || missing+=("## Test plan")
	if [[ -n $n ]]; then
		grep -qE "(^|[^[:alnum:]])Closes #$n([^0-9]|$)" <<<"$msg" || missing+=("Closes #$n")
	elif ! grep -qE '(^|[^[:alnum:]])Closes #[0-9]+' <<<"$msg"; then
		missing+=("Closes #<n>")
	fi
	if [[ ${#missing[@]} -eq 0 ]]; then
		ok "message has ## Plan, ## Test plan and Closes #$n"
	else
		gap "message lacks: $(printf '%s, ' "${missing[@]}" | sed 's/, $//')"
	fi
fi

changed=$(git diff --name-only --diff-filter=d "$base...$head")
generated=$(grep -E '(^|/)bootstrap-output/|\.img$' <<<"$changed" || true)
if [[ -z $generated ]]; then
	ok "no bootstrap-output/ or *.img committed"
else
	gap "generated artifacts committed:"
	bound 10 <<<"$generated" | sed 's/^/        /'
fi

echo
echo "== Facts for the skill to judge =="
yn() { if grep -qE "$1" <<<"$changed"; then echo yes; else echo no; fi; }
echo "changed files: $(git diff --name-only "$base...$head" | wc -l)"
echo "  Go: $(yn '\.go$|(^|/)go\.(mod|sum)$')   docs/: $(yn '^docs/')   docs/Roadmap.md: $(yn '^docs/Roadmap\.md$')"
echo "  internal/config/: $(yn '^internal/config/')   docs/Config Schema.md: $(yn '^docs/Config Schema\.md$')"
echo "  scripts/validate/: $(yn '^scripts/validate/')   docs/Decisions.md: $(yn '^docs/Decision')"
# A directive can be moved rather than dropped, so both sides are shown.
gosec=$(git diff -U0 "$base...$head" | awk '/^(---|\+\+\+) / { next } /^[-+].*\/\/nolint:[^ ]*gosec/')
removed=$(grep '^-' <<<"$gosec" || true)
if [[ -z $removed ]]; then
	echo "//nolint:gosec lines removed: none"
else
	echo "//nolint:gosec lines removed: $(wc -l <<<"$removed"), added: $(grep -c '^+' <<<"$gosec" || true)." \
		"Judge each: gone with its code, moved, or stripped"
	bound 10 <<<"$removed" | sed 's/^/  /'
fi
# Candidates for decisions-scout. Not every §N is a Decisions.md section (an
# issue may say "/run-chain §3"), and Decisions.md's own headings are left out.
cited_issue=""
if [[ -n $n ]]; then
	cited_issue=$(gh issue view "$n" --json body --jq .body | grep -oE '§[0-9]+' | sort -uV | paste -sd' ' || true)
fi
cited_files=""
mapfile -t files < <(grep -v '^docs/Decision' <<<"$changed" | grep . || true)
if [[ ${#files[@]} -gt 0 ]]; then
	cited_files=$(git grep -hoE '§[0-9]+' "$head" -- "${files[@]}" | sort -uV | paste -sd' ' || true)
fi
echo "§ cited by the issue:         ${cited_issue:-none}"
echo "§ cited in the changed files: ${cited_files:-none}"

echo
echo "== Commit message =="
if [[ -n $msg ]]; then bound 120 <<<"$msg"; else echo "(no commit beyond the base)"; fi

echo
echo "== Stat =="
# The first 60 files, then the summary line.
git diff --stat=100 "$base...$head" | awk '{ l[NR] = $0 } END {
	for (i = 1; i < NR && i <= 60; i++) print l[i]
	if (NR > 61) print " … " NR - 61 " more files"
	if (NR > 0) print l[NR] }'

if [[ -n $n ]]; then
	echo
	echo "== Issue #$n: Done when =="
	gh issue view "$n" --json title,state --jq '"\(.title) [\(.state)]"'
	gh issue view "$n" --json body --jq .body |
		awk '/^### Done when/ { p = 1; next } p && /^### / { exit } p' | bound 60

	echo
	echo "== docs/Roadmap.md lines naming #$n (at head) =="
	lines=$(git show "$head:docs/Roadmap.md" 2>/dev/null | grep -nE "#$n([^0-9]|$)" || true)
	if [[ -n $lines ]]; then bound 20 <<<"$lines"; else echo "(none: #$n isn't on the Roadmap)"; fi
fi

echo
if [[ $gaps -eq 0 ]]; then
	echo "audit: no mechanical gap"
else
	echo "audit: $gaps mechanical gap(s)"
	exit 1
fi
