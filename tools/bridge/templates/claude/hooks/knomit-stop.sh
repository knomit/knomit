#!/usr/bin/env bash
# Stop hook: per-turn capture-worthy detection. Rate-limited to one
# nudge per N turns (default 5) to keep noise down.

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/_knomit-helpers.sh"

RATE_DIR="${TMPDIR:-/tmp}/knomit-stop-rate"
mkdir -p "$RATE_DIR"
COUNTER="$RATE_DIR/counter"
N=5

CUR=0
[[ -f "$COUNTER" ]] && CUR=$(cat "$COUNTER")
NEW=$((CUR + 1))
echo "$NEW" > "$COUNTER"
if (( NEW < N )); then
  exit 0
fi
echo 0 > "$COUNTER"

INPUT=$(cat)
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // empty')

BLOCKS=$(knomit_extract_recent_turns "$TRANSCRIPT" 6)
[[ "$BLOCKS" == "[]" ]] && exit 0

INTENTS='["correction","discovery","decision","fix-bug","gotcha"]'
RESPONSE=$(knomit_post_detect "$BLOCKS" "$INTENTS")

HITS=$(echo "$RESPONSE" | jq -r '
  .blocks[]
  | select(([.signals[].score] | max // 0) > 0.75)
  | "  - " + ([.signals[] | select(.score > 0.75)] | map(.intent) | join(","))
')

if [[ -z "$HITS" ]]; then
  exit 0
fi

NUDGE="This turn produced capture-worthy moments:
$HITS

Consider /knomit-remember before moving on."

knomit_inject_context "$NUDGE"
