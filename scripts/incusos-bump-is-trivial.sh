#!/usr/bin/env bash
# Decides whether an incus-os submodule bump is provenance-only, so
# .github/workflows/bump-incusos.yml can enable auto-merge on its PR without a
# human `make lgtm` (#287). Run by that workflow after scripts/vendor-incusos.sh
# and before the PR is opened, so it reads the uncommitted working tree:
# tracked changes against HEAD, plus untracked files.
#
# Provenance-only means ALL of:
#   (a) the only changed paths are the submodule pointer (third_party/incus-os)
#       and files under internal/third_party/incusos/;
#   (b) no vendored file is added, deleted or renamed, and none changes mode
#       or type;
#   (c) every changed line under internal/third_party/incusos/ is the stamp
#       vendor-incusos.sh writes, `// Vendored from github.com/lxc/incus-os @
#       <40-hex sha>`.
# Anything else (a struct field, a new or removed upstream file, a go.mod pin
# moved by vendor-incusos.sh) needs a human to read the diff, because
# `go build`/`go test` catch breakage but not silent field-mapping drift in
# internal/seed.
#
# Output: a Markdown report on stdout, for the PR body. On the review path it
# lists what changed beyond the stamps.
#
# Exit status:
#   0  provenance-only: auto-merge is safe
#   1  needs review
#   2  error (not in a git work tree)
#
# Tested by scripts/incusos_bump_test.go (`make test`), against throwaway git
# repos built for each case.
#
# Usage:
#   scripts/incusos-bump-is-trivial.sh
set -euo pipefail

SUBMODULE="third_party/incus-os"
VENDORED="internal/third_party/incusos/"
STAMP_RE='^[-+]// Vendored from github\.com/lxc/incus-os @ [0-9a-f]{40}$'
EXCERPT_LINES=200

top=$(git rev-parse --show-toplevel 2>/dev/null) || {
	echo "error: not inside a git work tree" >&2
	exit 2
}
cd "$top"

outside=()     # (a) changed paths outside the two allowed ones
structural=()  # (b) vendored files added, deleted or changed in mode/type
stamp_only=()  # vendored files whose only changes are stamp lines
content=()     # (c) vendored files with changes beyond the stamp, "path (+N -M)"
content_paths=()
submodule_moved=0

# Tracked changes, working tree against HEAD. --no-renames reports a rename as
# a delete plus an add, so (b) sees both halves. --ignore-submodules=dirty
# counts a moved submodule pointer but not stray files inside the checkout.
# Each record is ":srcmode dstmode srcsha dstsha status" NUL path NUL.
while IFS= read -r -d '' meta && IFS= read -r -d '' path; do
	read -r srcmode dstmode _ _ status <<<"${meta#:}"
	if [ "$path" = "$SUBMODULE" ]; then
		if [ "$status" = M ] && [ "$srcmode" = 160000 ] && [ "$dstmode" = 160000 ]; then
			submodule_moved=1
		else
			structural+=("$path (status $status, mode $srcmode -> $dstmode)")
		fi
		continue
	fi
	case "$path" in
	"$VENDORED"*) ;;
	*)
		outside+=("$path")
		continue
		;;
	esac
	case "$status" in
	A) structural+=("$path (added)") ;;
	D) structural+=("$path (deleted)") ;;
	M)
		if [ "$srcmode" != "$dstmode" ]; then
			structural+=("$path (mode $srcmode -> $dstmode)")
			continue
		fi
		# Changed lines only: skip each file's header (diff --git, index,
		# ---/+++) by counting only lines inside a hunk.
		# The stamp match is grep -E, not awk: mawk (Ubuntu's awk) has had
		# no {n} intervals for most of its life.
		changed=$(git diff --no-color --text --unified=0 HEAD -- "$path" |
			awk '/^diff --git / { inhunk = 0; next }
			     /^@@/ { inhunk = 1; next }
			     inhunk && /^[-+]/')
		add=0 del=0
		if [ -n "$changed" ]; then
			nonstamp=$(grep -vE "$STAMP_RE" <<<"$changed" || true)
			add=$(grep -c '^+' <<<"$nonstamp" || true)
			del=$(grep -c '^-' <<<"$nonstamp" || true)
		fi
		if [ "$add" -eq 0 ] && [ "$del" -eq 0 ]; then
			stamp_only+=("$path")
		else
			content+=("$path (+$add -$del)")
			content_paths+=("$path")
		fi
		;;
	*) structural+=("$path (status $status)") ;;
	esac
done < <(git diff --raw -z --no-renames --ignore-submodules=dirty HEAD)

# Untracked files are what the PR would add (create-pull-request commits them
# all); ignored ones never reach the PR.
while IFS= read -r -d '' path; do
	case "$path" in
	"$VENDORED"*) structural+=("$path (added)") ;;
	*) outside+=("$path (added)") ;;
	esac
done < <(git ls-files -z --others --exclude-standard)

list() {
	local item
	for item in "$@"; do
		# shellcheck disable=SC2016 # the backticks are Markdown, not a command
		printf -- '- `%s`\n' "$item"
	done
}

if [ ${#outside[@]} -eq 0 ] && [ ${#structural[@]} -eq 0 ] && [ ${#content[@]} -eq 0 ] &&
	{ [ "$submodule_moved" -eq 1 ] || [ ${#stamp_only[@]} -gt 0 ]; }; then
	echo "**Provenance-only bump: auto-merge enabled.** The diff moves the \`$SUBMODULE\`"
	echo "submodule pointer and rewrites only the \`// Vendored from github.com/lxc/incus-os @ <sha>\`"
	echo "stamp in ${#stamp_only[@]} vendored file(s) under \`$VENDORED\`. No struct, file"
	echo "or dependency changed, so this merges once the required checks pass, with no \`make lgtm\`."
	exit 0
fi

echo "**Needs review: auto-merge not enabled.** This bump changes more than the"
echo "vendored-from stamps, so it waits for a human to read the diff and run \`make lgtm\`."
echo "\`go build\`/\`go test\` passed in this workflow, but that only catches breakage,"
echo "not silent field-mapping drift in \`internal/seed\`."
if [ ${#outside[@]} -eq 0 ] && [ ${#structural[@]} -eq 0 ] && [ ${#content[@]} -eq 0 ]; then
	echo
	echo "No change was found at all, which a real bump never produces."
fi
if [ ${#content[@]} -gt 0 ]; then
	echo
	echo "Vendored files with changes beyond the stamp (non-stamp lines added/removed):"
	echo
	list "${content[@]}"
fi
if [ ${#structural[@]} -gt 0 ]; then
	echo
	echo "Vendored files added, deleted or changed in mode or type:"
	echo
	list "${structural[@]}"
fi
if [ ${#outside[@]} -gt 0 ]; then
	echo
	echo "Changed paths outside \`$SUBMODULE\` and \`$VENDORED\`:"
	echo
	list "${outside[@]}"
fi
if [ ${#content_paths[@]} -gt 0 ]; then
	excerpt=$(git diff --no-color --text HEAD -- "${content_paths[@]}")
	total=$(wc -l <<<"$excerpt")
	echo
	echo "<details><summary>Diff of the vendored files above</summary>"
	echo
	echo '```diff'
	head -n "$EXCERPT_LINES" <<<"$excerpt"
	echo '```'
	if [ "$total" -gt "$EXCERPT_LINES" ]; then
		echo
		echo "(first $EXCERPT_LINES of $total lines; see the PR diff for the rest)"
	fi
	echo
	echo "</details>"
fi
exit 1
