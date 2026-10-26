# The Ready criterion "Done when names a validate script or real-run check"
# (/file-task § Labels and milestone, #410), as one check every caller shares:
# .claude/skills/next/next.sh (inconsistencies), .claude/skills/run-chain/queue.sh
# (skip), and #385's `make ready` when it lands.
#
#   jq -L "$repo/.claude/skills" 'include "file-task/done-when"; .body | done_when_ok'
#
# Input is an issue body (a string; null reads as empty). An item of its
# Done-when section qualifies when it names `scripts/validate/<name>.sh` or
# starts `Real-run check` (bold allowed). The check is textual: a script the
# issue will create counts as much as one that exists already, and a proof
# delegated to another issue ("proven by #N's script") doesn't count until the
# item names that script or says `Real-run check:`.

# The section's items, each with its continuation lines joined. An item is a
# list entry (`- [ ]`, `- [x]`, a plain `-`/`*`/`+` bullet, or `1.`/`1)`), or,
# under the heading, a prose paragraph. The section is a `### Done when`
# heading (the task template) running to the next heading, or a `Done when:`
# line (a bug's Expected behavior, which has no such heading) running to the
# first paragraph after its list. Code fences are tracked, so a `# comment` in
# a fenced block isn't a heading; a fenced line joins the item it's under.
def done_when_items:
  def add($s): if .item then .item += " " + $s else .item = $s end;
  [ ((. // "") | gsub("\r"; "") | split("\n")) + [null]
    | foreach .[] as $l ({in: null, item: null, fence: false};
        .out = null
        | if $l == null then .out = .item
          elif ($l | test("^\\s*(```|~~~)")) then
            .fence = (.fence | not)
            | if .in then add($l | sub("^\\s+"; "")) else . end
          elif .fence then
            if .in then add($l | sub("^\\s+"; "")) else . end
          elif ($l | test("^#{1,6}\\s")) then
            .out = .item | .item = null
            | .in = (if ($l | test("^###\\s+Done when\\s*$"; "i")) then "heading" else null end)
          elif ($l | gsub("[*_\\s]"; "") | test("^donewhen:?$"; "i")) then
            .out = .item | .item = null | .in = "line"
          elif .in == null then .
          elif ($l | test("^\\s*([-*+]|[0-9]+[.)])\\s")) then
            .out = .item
            | .item = ($l | sub("^\\s*([-*+]|[0-9]+[.)])\\s+(\\[[ xX]\\]\\s+)?"; ""))
          elif ($l | test("^\\s*$")) then .out = .item | .item = null
          elif .item then add($l | sub("^\\s+"; ""))
          elif .in == "heading" then .item = ($l | sub("^\\s+"; ""))
          else .in = null end;
        .out // empty) ];

# An item qualifies when it names a validate script, or leads with the phrase
# "Real-run check". Issues filed before #410 write "Real-run check (no
# pipeline):" or "Real-run check, recorded in …", so the colon isn't required.
def done_when_item_qualifies:
  test("scripts/validate/[A-Za-z0-9._-]+\\.sh")
  or test("^[*_]{0,2}real-run check\\b"; "i");

def done_when_ok: any(done_when_items[]; done_when_item_qualifies);

# The reason callers print when done_when_ok is false.
def done_when_gap: "Done-when names no validate script or real-run check";
