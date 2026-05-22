#!/usr/bin/env bash
# PostToolUse hook for Bash: when CC runs `git commit`, nudge for capture.

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/_knomit-helpers.sh"

# CC passes the tool input as JSON on stdin
INPUT=$(cat)
CMD=$(echo "$INPUT" | jq -r '.tool_input.command // empty')

# Only act on git commit
if ! echo "$CMD" | grep -q '^git commit'; then
  exit 0
fi

MSG=$(echo "$INPUT" | jq -r '.tool_output // empty')
LEN=${#MSG}

# Heuristic: substantive if message > 60 chars OR has markers
if (( LEN < 60 )) && ! echo "$MSG" | grep -qE 'fix:|refactor:|decided:|invariant:|gotcha:'; then
  exit 0
fi

cat <<EOF
<system-reminder>
This commit looks substantive. Run \`/remember\` to capture as a fact, or
\`/decided\` if the commit codifies a design choice. Commit subject:
$MSG
</system-reminder>
EOF
