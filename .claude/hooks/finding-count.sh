#! /bin/bash
# SessionStart hook (#314): one context line when the findings log
# (scripts/finding.sh) holds unfiled findings, so they get filed rather than
# forgotten. Silent otherwise, and it never fails the session start.
set -uo pipefail

cat >/dev/null 2>&1 || true # the hook input; nothing in it is needed

finding="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/finding.sh"
n=$("$finding" count --unfiled 2>/dev/null) || exit 0
case "$n" in
'' | *[!0-9]* | 0) exit 0 ;;
1) echo "1 unfiled finding; \`/file-findings\` to file it" ;;
*) echo "$n unfiled findings; \`/file-findings\` to file them" ;;
esac
exit 0
