#!/usr/bin/env bash
# PreCompact hook: scan the to-be-compressed transcript window for
# capture-worthy moments via /detect, nudge if any score above threshold.

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/_knomit-helpers.sh"

INPUT=$(cat)
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // empty')

BLOCKS=$(knomit_extract_recent_turns "$TRANSCRIPT" 24)
[[ "$BLOCKS" == "[]" ]] && exit 0

INTENTS='["correction","discovery","decision","fix-bug","gotcha"]'
RESPONSE=$(knomit_post_detect "$BLOCKS" "$INTENTS")

# Find blocks where max signal score > threshold (0.7, matching the YAML)
HITS=$(echo "$RESPONSE" | jq -r '
  .blocks[]
  | select(([.signals[].score] | max // 0) > 0.7)
  | "  - " + ([.signals[] | select(.score > 0.7)] | map(.intent) | join(",")) + ": " + (.index | tostring)
')

if [[ -z "$HITS" ]]; then
  exit 0
fi

cat <<EOF
<system-reminder>
Before compaction, these recent moments look capture-worthy:
$HITS

Run \`/remember\` or \`/decided\` if you want any of them preserved.
</system-reminder>
EOF
