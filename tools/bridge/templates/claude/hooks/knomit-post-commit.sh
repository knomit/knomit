#!/usr/bin/env bash
# PostToolUse hook for Bash: when CC runs `git commit`, nudge for capture.

set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/_knomit-helpers.sh"

# CC passes the tool input/output as JSON on stdin
INPUT=$(cat)
CMD=$(echo "$INPUT" | jq -r '.tool_input.command // empty')

# Only act on git commit
if ! echo "$CMD" | grep -q '^git commit'; then
  exit 0
fi

STDOUT=$(echo "$INPUT" | jq -r '.tool_output.stdout // empty')
EXIT_CODE=$(echo "$INPUT" | jq -r '.tool_output.exit_code // 0')
LEN=${#STDOUT}

# Heuristic: substantive if stdout > 60 chars OR has markers
if (( LEN < 60 )) && ! echo "$STDOUT" | grep -qE 'fix:|refactor:|decided:|invariant:|gotcha:'; then
  exit 0
fi

NUDGE="This commit looks substantive. Run /knomit-remember to capture as a fact, or /knomit-decided if the commit codifies a design choice.

Commit subject:
$STDOUT"

knomit_inject_context "$NUDGE"
