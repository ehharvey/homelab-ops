# The review-state labels and the merge gate (#536), written once for both
# readers: scripts/review-state.sh, which sets the labels, and
# .claude/skills/next/next.sh, which reports which PRs meet the gate. Both
# load it with `jq -L scripts 'include "review-gate"; …'`. The rules are
# docs/Development Conventions.md § Review state labels.

# Whether a label name is one review-state.sh owns.
def review_label: . == "reviewed-work" or . == "review-unaddressed" or startswith("review-passes-left-");

# Whether an array of label names meets the merge gate: a verdict posted, no
# /code-review passes left, and nothing unaddressed.
def meets_merge_gate:
  any(.[]; . == "reviewed-work") and any(.[]; . == "review-passes-left-0")
  and (any(.[]; . == "review-unaddressed") | not);
