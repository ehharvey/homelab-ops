#!/usr/bin/env bash
# Rebase auto-merge PRs that fell behind main (#264). Run by
# .github/workflows/auto-rebase.yml; runnable locally with --dry-run.
#
# main's ruleset has strict_required_status_checks_policy on, so a PR must be
# up to date with main before it can merge. When one PR lands, every other
# queued one goes BEHIND and would sit there until updated by hand. Updating
# with GitHub's default "Update branch" adds a merge commit, which makes the
# branch two commits and fails one-commit, so this always rebases, which keeps
# the branch at one commit.
#
# Only PRs with auto-merge enabled are touched. Auto-merge is the operator's
# "I've read the diff" signal (`make lgtm`, #125), and force-pushing a branch
# someone is still working on would put their local copy out of sync. PRs not
# based on main (stacked) belong to `make restack` (#254); PRs from forks can't
# be pushed to.
#
# "Behind" comes from the compare API, not mergeStateStatus: GitHub computes
# merge state lazily, so right after a push to main it reads UNKNOWN for a few
# seconds, and that's exactly when this runs.
#
# A PR that conflicts gets one comment per head SHA (tracked by a marker in the
# comment body), never a force-push or a retry loop. Pushing a new head (the
# operator's local rebase) re-arms it.
#
# Environment:
#   GH_TOKEN      reads PRs and posts comments (GITHUB_TOKEN in CI)
#   REBASE_TOKEN  runs the rebase. Must NOT be GITHUB_TOKEN: pushes made with
#                 it don't trigger workflows, so the rebased PR would never get
#                 its required checks and would stay pending forever. Defaults
#                 to GH_TOKEN, which is fine for --dry-run.
#   REPO          owner/name (defaults to the current repo)
#
# Usage:
#   scripts/auto-rebase.sh [--dry-run]
set -euo pipefail

dry_run=0
[ "${1:-}" = "--dry-run" ] && dry_run=1

repo=${REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}
rebase_token=${REBASE_TOKEN:-${GH_TOKEN:-}}

list_candidates() {
	gh pr list --repo "$repo" --base main --state open --limit 100 \
		--json number,id,headRefOid,isCrossRepository,autoMergeRequest,mergeable \
		--jq '.[] | select(.autoMergeRequest != null and (.isCrossRepository | not))
		      | [.number, .id, .headRefOid, .mergeable] | @tsv'
}

# mergeable is computed lazily too; give GitHub a little time to settle it so
# a known conflict gets a comment instead of a doomed rebase attempt. Anything
# still UNKNOWN afterwards is attempted, and a conflict surfaces as a failed
# mutation instead.
candidates=$(list_candidates)
for _ in 1 2 3 4 5 6; do
	grep -q $'\tUNKNOWN$' <<<"$candidates" || break
	sleep 5
	candidates=$(list_candidates)
done

if [ -z "$candidates" ]; then
	echo "auto-rebase: no open auto-merge PRs on main."
	exit 0
fi

# Comments once per (PR, head SHA). $3 is the body below the marker.
comment_once() {
	local number=$1 head=$2 body=$3
	local marker="<!-- auto-rebase:conflict head=$head -->" existing
	# Captured, not piped into grep -q: under pipefail, grep exiting early can
	# SIGPIPE gh, which reads as "no marker" and posts a duplicate.
	existing=$(gh api "repos/$repo/issues/$number/comments" --paginate --jq '.[].body')
	if grep -qF "$marker" <<<"$existing"; then
		echo "  already commented for ${head:0:8}; not commenting again"
		return
	fi
	if [ "$dry_run" = 1 ]; then
		echo "  [dry-run] would comment"
		return
	fi
	gh pr comment "$number" --repo "$repo" --body "$marker
$body" >/dev/null
	echo "  commented"
}

# shellcheck disable=SC2016 # literal Markdown backticks
local_rebase_steps='```sh
git fetch origin main
git rebase origin/main      # resolve, then: git rebase --continue
git push --force-with-lease
```

Auto-merge stays on, so it merges once the checks pass. Don'"'"'t use the "Update branch" button: its default merge mode adds a merge commit, which fails `one-commit`.'

failed=0
while IFS=$'\t' read -r number id head mergeable; do
	behind=$(gh api "repos/$repo/compare/main...$head" --jq .behind_by)
	echo "#$number: head ${head:0:8}, behind main by $behind, mergeable=$mergeable"
	[ "$behind" -gt 0 ] || continue

	if [ "$mergeable" = CONFLICTING ]; then
		comment_once "$number" "$head" "**auto-rebase:** this PR is behind \`main\` and conflicts with it, so it can't be rebased automatically. Rebase it locally:

$local_rebase_steps"
		continue
	fi

	if [ "$dry_run" = 1 ]; then
		echo "  [dry-run] would rebase onto main"
		continue
	fi

	# expectedHeadOid makes this a no-op if someone pushed since we listed it,
	# rather than rebasing a head we never looked at.
	# shellcheck disable=SC2016 # $id/$head are GraphQL variables
	if out=$(GH_TOKEN=$rebase_token gh api graphql \
		-f query='mutation($id: ID!, $head: GitObjectID!) {
		  updatePullRequestBranch(input: {pullRequestId: $id, updateMethod: REBASE, expectedHeadOid: $head}) {
		    pullRequest { headRefOid }
		  }
		}' -f id="$id" -f head="$head" \
		--jq .data.updatePullRequestBranch.pullRequest.headRefOid 2>&1); then
		echo "  rebased: ${head:0:8} -> ${out:0:8}"
		continue
	fi

	echo "::warning::#$number: rebase failed: $out"
	failed=1
	comment_once "$number" "$head" "**auto-rebase:** couldn't rebase this PR onto \`main\`, most likely because of a conflict. Rebase it locally:

$local_rebase_steps

GitHub said:
\`\`\`
$out
\`\`\`"
done <<<"$candidates"

# A failed rebase that isn't a plain conflict (an expired token, say) should
# turn the run red, not just leave a PR comment.
exit "$failed"
