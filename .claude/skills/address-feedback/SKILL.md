---
name: address-feedback
description: Act on review comments, a /review-work verdict's gaps or failing checks on an open issue PR. Gathers unresolved threads, an unaddressed verdict, PR comments and failing checks (offering a /code-review pass first when a verdict's gaps have had none), amends the single commit, re-verifies, re-ships with `make ship`, syncs the PR title/body from the commit, replies on each thread and to the verdict, syncs the review-state labels, then stops before `make lgtm`. Use for "address the review", "fix the PR feedback", "respond to the review comments", "CI failed on the PR", "the checks are red on the PR".
---

# /address-feedback [<pr>]

The step after `/ship-issue`: an open PR has feedback, from the operator,
`/code-review --comment`, a `/review-work` verdict or CI. Under the one-commit rule the loop is the same
shape as first shipping: amend, re-verify, `make ship`. `CLAUDE.md` §
Conventions / § Worktrees, `docs/Development Conventions.md` § Branching &
issues / § PR body format, and `/ship-issue` hold the rules and checks. This
skill is the order to apply them in.

Stop at the first failing step and fix it (or report it) before going on.

1. **Find the PR and its feedback.** With no `<pr>`, use the current
   `eharvey/#<n>` branch's: `gh pr view --json number,url,state,headRefName`.
   It must be `OPEN`. Then pull only what's unaddressed, bounded:
   - Unresolved review threads (each thread's id, file, line and comments):

     ```sh
     gh api graphql -F n=<pr> -f query='query($n:Int!){repository(owner:"ehharvey",name:"homelab-ops"){pullRequest(number:$n){reviewThreads(first:100){nodes{id isResolved path line comments(first:20){nodes{author{login} body}}}}}}}' \
       --jq '.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved|not)'
     ```

   - Review summaries and PR comments:
     `gh pr view <pr> --json reviews,comments --jq '.reviews[], .comments[] | {author: .author.login, body}'`.
   - Failing checks: `gh pr checks <pr> --json name,state,link --jq '.[] | select(.state=="FAILURE")'`,
     then for each only the tail of its failed log. A check's `link` ends in
     its job id: `gh run view --job "${link##*/}" --log-failed | tail -n 80`.
     The `Review state` workflow's `sync` is advisory and runs on every review
     event, so only its latest run counts: an older failure the next run
     superseded isn't a work item, and a cancelled one was a burst's
     in-between run.
   - The review state: `scripts/review-state.sh <pr> --dry-run`. A `verdict`
     line marked `unaddressed` is a `/review-work` verdict whose gaps are
     work for this pass; each bullet under its `gaps:` is an item. Its body, by
     the id that line prints:
     `gh pr view <pr> --json comments --jq '.comments[] | select(.id == "<id>") | .body'`
     (an id starting `PRR_` is a verdict posted as a review before #536:
     `gh pr view <pr> --json reviews --jq '.reviews[] | select(.id == "<id>") | .body'`).
     `docs/Development Conventions.md` § Review state labels defines the
     labels, the markers and what counts as addressed.

   **Offer a `/code-review` pass first** when all three hold: the work
   includes an unaddressed verdict's gaps, the dry run's `since-verdict:` is
   0 (no `/code-review` pass since that verdict), and its `labels:` line has
   `review-passes-left-<n>` with `n` above 0. Then pause and ask the operator
   whether to run `/code-review --comment` before changing anything, and at
   which level, so this one pass handles its findings and the gaps together.
   - On yes, invoke the `code-review` skill with `<level> --comment <pr>`.
     If this session can't run it that way (no such skill, or it refuses to
     post), stop and print `/code-review <level> --comment <pr>` for the
     operator to run, then `/address-feedback <pr>` again; say in the PR's
     Test plan which of the two happened. A run that found nothing and posted
     nothing is still a pass: record it with
     `scripts/review-state.sh <pr> --clean-pass --head <sha>`, where `<sha>`
     is the PR's head as you read it before the run
     (`gh pr view <pr> --json headRefOid --jq .headRefOid`); if the head moved
     since, it refuses rather than record the wrong commit. Then gather again
     from the top of this step, so its threads join the list.
   - On no, carry on.

   List each item with what you'll do about it (take, partly take, decline)
   before changing anything.
2. **Work in the issue's worktree**, never the main checkout (`CLAUDE.md` §
   Worktrees). `git worktree list` shows whether `eharvey/#<n>` already has
   one. If not, `make wt N=<n>` and work from the printed path.
3. **Change, then amend.** Make the changes and `git add` them, then amend,
   never a fixup commit: the branch stays exactly one commit. Update `## Plan`
   if the approach changed, and `## Test plan` to list what actually re-ran.
   Leave the subject and the `<!-- ship:issue -->` block alone: `make ship`
   regenerates both from the issue (#450), so an issue edit flows through on
   re-ship, and a hand edit there is overwritten:
   `git log -1 --format=%B > <scratchpad>/msg`, edit that file, then
   `git commit --amend -F <scratchpad>/msg` (Development Conventions § PR body
   format; anything else that opens the message strips the `## ` lines). If
   `origin/main` has moved, `git fetch -q origin main && git rebase FETCH_HEAD`.
4. **Re-verify** with `/ship-issue`'s **Checks**, **Validate** (through
   `validate-runner`) and **Fold fixes in** steps (5–7 as of #273; go by the
   names if the numbering has moved). Re-run the validate script when the
   change touches what it proves.
5. **Re-ship.** `make ship`. With the PR open it force-pushes, leased on the
   PR's head SHA, so it refuses if anything reached the branch since it read
   the PR. If it refuses, fetch the branch, fold the new work into the commit,
   and go back to step 4. Don't force past it.
6. **Sync the PR from the commit.** `--fill` copied the message only when the
   PR was created, so it's stale once the commit was amended:

   ```sh
   gh pr edit <pr> --title "$(git log -1 --format=%s)" --body-file <(git log -1 --format=%b)
   ```

   Use `%b`, not `%B`: `--fill` puts the subject in the title, not the body.
   Run it after `make ship`, so the title and body carry the issue block ship
   just regenerated.
7. **Reply on every thread you addressed.** Say what changed, or why not.
   Start each reply with the `<!-- address-feedback:reply -->` marker, on
   its own line: a thread counts as addressed only when it's resolved or its
   newest comment carries it (Development Conventions § Review state labels).
   ```sh
   gh api graphql -f id=<thread-id> -f body=$'<!-- address-feedback:reply -->\n<reply>' -f query='mutation($id:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$id,body:$body}){comment{url}}}'
   ```
   Resolve a thread (`resolveReviewThread(input:{threadId:$id})`) only when
   its feedback was fully taken. When you disagree, push back in a reply that
   gives the reason and leave the thread open for the operator. Never skip
   feedback silently. Answer plain PR comments with `gh pr comment <pr>`.

   **Reply to an unaddressed verdict** you worked on with one PR comment whose
   first line names it, then one line per gap: what changed, or why not.

   ```sh
   { echo '<!-- address-feedback:verdict-reply <verdict id> -->'; cat <scratchpad>/gaps.md; } >"<scratchpad>/reply.md"
   gh pr comment <pr> --body-file "<scratchpad>/reply.md"
   ```

   Then sync the labels: `scripts/review-state.sh <pr>`. Its `labels:` line
   goes in the report.
8. **Watch the checks.** Run `gh pr checks <pr> --watch` in the background
   (`CLAUDE.md` § Keeping context lean), then report: what was taken, what was
   declined and why, the review-state labels, and the checks' result.

**Stop there.** `make lgtm` is the operator's call after reading the new diff.
Don't run it, don't enable auto-merge another way, and don't offer to unless
asked.
