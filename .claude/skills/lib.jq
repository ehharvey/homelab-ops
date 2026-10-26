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

# The decisions an issue's body cites: each `§<digit>…`, and `Decisions.md`
# when it mentions the file. Any one makes /run-chain's lane `full`.
def cites: (.body // "") | [scan("§[0-9]+"), (if test("Decisions\\.md") then "Decisions.md" else empty end)] | unique;
