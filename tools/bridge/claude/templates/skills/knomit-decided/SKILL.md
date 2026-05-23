---
name: knomit-decided
description: Capture a design decision made during the session
---

# /knomit-decided <slug>

Use after a tradeoff was resolved in conversation. The skill summarizes:

1. Options considered (what was discussed)
2. Rationale (why the chosen option won)
3. The choice (concrete decision)

Then writes a fact via `mcp__knomit__knomit_learn`:

- kind: `decision`
- topic path: `decisions/accepted/<YYYY-MM>-<slug>`
- refs: include any files touched + URL to the conversation if available
- confidence: 0.95

**Ref format for source files (IMPORTANT):**

Read your source slug from `.mcp.json` at `mcpServers.knomit.args` (the value
right after `--source`).

If the project is a git repository, write source-file refs as:

    src://<source>/<path>@<commit>

Get the commit with `git rev-parse HEAD`. Example:

    src://knomit/internal/store/service.go@cfef409

If the project is NOT in git, omit the @commit suffix:

    src://<source>/<path>

Example:

    src://my-scripts/run.sh

NEVER write bare paths like `internal/store/service.go` — knomit's ref
resolver treats unscheme'd strings as local fact paths and lookups will
fail or clash with actual facts.
