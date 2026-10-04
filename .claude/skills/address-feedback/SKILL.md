---
name: address-feedback
description: Act on review comments or failing checks on an open issue PR. Gathers unresolved threads, PR comments and failing checks, amends the single commit, re-verifies, re-ships with `make ship`, syncs the PR title/body from the commit, replies on each thread, then stops before `make lgtm`. Use for "address the review", "fix the PR feedback", "respond to the review comments", "CI failed on the PR", "the checks are red on the PR".
---

# /address-feedback [<pr>]

The step after `/ship-issue`: an open PR has feedback, from the operator,
`/code-review --comment` or CI. Under the one-commit rule the loop is the same
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

   List each item with what you'll do about it (take, partly take, decline)
   before changing anything.
2. **Work in the issue's worktree**, never the main checkout (`CLAUDE.md` §
   Worktrees). `git worktree list` shows whether `eharvey/#<n>` already has
   one. If not, `make wt N=<n>` and work from the printed path.
3. **Change, then amend.** Make the changes, then `git commit --amend`, never a
   fixup commit: the branch stays exactly one commit. Update `## Plan` if the
   approach changed, and `## Test plan` to list what actually re-ran. If
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
7. **Reply on every thread you addressed.** Say what changed, or why not:
   ```sh
   gh api graphql -f id=<thread-id> -f body='<reply>' -f query='mutation($id:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$id,body:$body}){comment{url}}}'
   ```
   Resolve a thread (`resolveReviewThread(input:{threadId:$id})`) only when
   its feedback was fully taken. When you disagree, push back in a reply that
   gives the reason and leave the thread open for the operator. Never skip
   feedback silently. Answer plain PR comments with `gh pr comment <pr>`.
8. **Watch the checks.** Run `gh pr checks <pr> --watch` in the background
   (`CLAUDE.md` § Keeping context lean), then report: what was taken, what was
   declined and why, and the checks' result.

**Stop there.** `make lgtm` is the operator's call after reading the new diff.
Don't run it, don't enable auto-merge another way, and don't offer to unless
asked.
