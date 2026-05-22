#!/usr/bin/env bash
# SessionStart hook: inject invariants + recently-updated facts as a
# system-reminder preamble before CC reads the user's first prompt.

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/_knomit-helpers.sh"

REPO=$(knomit_repo_name)
BRANCH=$(knomit_agent_branch)
BASE=$(knomit_base_url)

INVARIANTS=$(curl -sS "$BASE/api/v1/repos/$REPO/branches/$BRANCH/search?path=invariants/" \
  | jq -r '._embedded.facts // [] | map("  - \(.title)\n    \(.body)") | join("\n")')

RECENT=$(curl -sS "$BASE/api/v1/repos/$REPO/branches/$BRANCH/activity?limit=5" \
  | jq -r '._embedded.facts // [] | map("  - \(.path): \(.title)") | join("\n")')

if [[ -n "$INVARIANTS" || -n "$RECENT" ]]; then
  cat <<EOF
<system-reminder>
Known facts from knomit for this codebase:

LOAD-BEARING INVARIANTS:
$INVARIANTS

Recent work in this repo:
$RECENT
</system-reminder>
EOF
fi
