#!/usr/bin/env bash
# SessionEnd hook: digest of any capture-worthy moments not yet captured.

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/_knomit-helpers.sh"

INPUT=$(cat)
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // empty')

# Pull a wider window for the final pass
BLOCKS=$(knomit_extract_recent_turns "$TRANSCRIPT" 60)
[[ "$BLOCKS" == "[]" ]] && exit 0

INTENTS='["correction","discovery","decision","fix-bug","gotcha"]'
RESPONSE=$(knomit_post_detect "$BLOCKS" "$INTENTS")

HITS=$(echo "$RESPONSE" | jq -r '
  .blocks[]
  | select(([.signals[].score] | max // 0) > 0.7)
  | "  - " + ([.signals[] | select(.score > 0.7)] | map(.intent) | join(","))
')

if [[ -z "$HITS" ]]; then
  exit 0
fi

cat <<EOF
<system-reminder>
Session ending — these moments from this session look capture-worthy and
weren't written to knomit yet:
$HITS

Run \`/remember\` or \`/decided\` before exiting if you want them preserved.
</system-reminder>
EOF
