#!/usr/bin/env bash
# Shared helpers for knomit hooks. Sourced, not executed directly.

# knomit_base_url: print the knomit HTTP base URL.
# Set KNOMIT_BASE_URL if knomit runs on a non-default port; otherwise the
# default works for the standard local install.
knomit_base_url() {
  if [[ -n "$KNOMIT_BASE_URL" ]]; then
    echo "$KNOMIT_BASE_URL"
    return
  fi
  echo "http://localhost:19278"
}

# knomit_repo_name: print the configured repo name from .mcp.json,
# falling back to the directory basename.
knomit_repo_name() {
  local mcp="$CLAUDE_PROJECT_DIR/.mcp.json"
  if [[ -f "$mcp" ]]; then
    jq -r '.mcpServers.knomit.args | (index("--repo") + 1) as $i | .[$i] // empty' "$mcp" 2>/dev/null \
      || basename "$CLAUDE_PROJECT_DIR"
  else
    basename "$CLAUDE_PROJECT_DIR"
  fi
}

# knomit_agent_branch: print the agent branch for the current repo.
# Resolved by querying knomit at startup.
knomit_agent_branch() {
  local repo
  repo=$(knomit_repo_name)
  curl -sS "$(knomit_base_url)/api/v1/repos/$repo" 2>/dev/null \
    | jq -r '.agent_branch // empty'
}

# knomit_post_detect <blocks-json> <intents-json>: POST to /detect, print JSON response.
knomit_post_detect() {
  local blocks="$1"
  local intents="$2"
  local repo branch
  repo=$(knomit_repo_name)
  branch=$(knomit_agent_branch)
  jq -n \
    --argjson blocks "$blocks" \
    --argjson intents "$intents" \
    --arg repo "$repo" \
    --arg branch "$branch" \
    '{blocks: $blocks, intents: $intents, novelty_context: {repo: $repo, branch: $branch}}' \
    | curl -sS -X POST "$(knomit_base_url)/api/v1/profiles/code/detect" \
        -H 'Content-Type: application/json' --data-binary @-
}

# knomit_extract_recent_turns <transcript-path> <n>: extract last n
# user+assistant turns as a JSON array of {role, text} blocks.
knomit_extract_recent_turns() {
  local transcript="$1"
  local n="${2:-12}"
  if [[ ! -f "$transcript" ]]; then
    echo "[]"
    return
  fi
  tail -n "$n" "$transcript" \
    | jq -s --slurpfile _ /dev/null '
      map(select(.type == "user" or .type == "assistant"))
      | map({role: .type, text: (.message.content | tostring)})
    '
}

# knomit_inject_context: print a JSON object that injects $1 as a system
# reminder via CC's hookSpecificOutput.additionalContext mechanism.
# Use this from PreCompact, Stop, PostToolUse, etc. (any event other than
# the few that auto-wrap plain stdout: SessionStart, UserPromptSubmit, ...).
knomit_inject_context() {
  jq -nc --arg ctx "$1" '{hookSpecificOutput: {additionalContext: $ctx}}'
}
