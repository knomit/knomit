---
name: knomit-remember
description: Capture a discovery as a knomit fact (with contradiction check)
---

# /knomit-remember

Use AFTER:
- Discovering something non-obvious during exploration
- A user correction on a project fact
- A bug fix that exposed a hidden invariant

Steps:
1. Run `mcp__knomit__knomit_query` on the would-be title to surface
   similar/contradicting existing facts.
2. If a contradicting fact exists, ask the user whether to update,
   retract, or merge instead of writing a new one.
3. Otherwise call `mcp__knomit__knomit_learn` with: kind, title, body,
   entities (file paths/symbols), refs (file@commit + PRs), confidence 0.85.

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
