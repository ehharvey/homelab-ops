# jq defs shared by the skill scripts (#409), the jq counterpart of lib.sh.
# Include it with the skills directory on jq's library path:
#
#   skills=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
#   jq -L "$skills" 'include "lib"; …'
#
# What a bundle is and what counts as citing a decision live here, so /bundle,
# /run-chain, /next and audit.sh can't disagree about them.

# A /bundle parent's title, "[Task]: Bundle: …". An umbrella's isn't, and its
# sub-issues still run alone. Input: a title string.
def bundle_title: test("^\\[Task\\]: *Bundle:");

# A sub-issue of an open bundle: it ships with its parent, never alone. Input:
# an issue node with `parent { number state title }`. A closed parent (a
# reverted bundle) holds nothing back.
def bundled: .parent != null and .parent.state == "OPEN" and (.parent.title | bundle_title);

# The open children in a REST `issues/<n>/sub_issues` listing, the ones a
# bundle's commit closes (audit.sh, issue-block.sh). Input: that array.
def open_sub_issues: .[] | select(.state == "open");

# The decisions an issue's body cites: each `§<digit>…`, and `Decisions.md`
# when it mentions the file. Any one makes /run-chain's lane `full`.
def cites: (.body // "") | [scan("§[0-9]+"), (if test("Decisions\\.md") then "Decisions.md" else empty end)] | unique;

# The open issues this one blocks, which landing it unblocks. Input: an issue
# node with `blocking { nodes { number state } }`.
def unblocks: [.blocking.nodes[] | select(.state == "OPEN") | .number];

# Labelled `p0`: the operator wants it taken before other Ready issues (#392).
def is_p0: any(.labels.nodes[]; .name == "p0");

# The order /next lists Ready issues in and /run-chain runs them in (#392):
# sort issue nodes by $keys, a list of `p0` (labelled first), `unblocks` (most
# open dependents first) and `number` (ascending). The number is always the
# final tiebreaker, listed or not. lib.sh's order_json checks the keys and
# holds the default. Input: an array of issue nodes.
def ready_order($keys):
  sort_by([$keys[] as $k
    | if $k == "p0" then (if is_p0 then 0 else 1 end)
      elif $k == "unblocks" then -(unblocks | length)
      else .number end] + [.number]);
