---
name: knomit-decided
description: Use immediately after you and the user resolved a tradeoff in conversation — captures the options, rationale, and choice so the decision isn't re-litigated later
---

# /knomit-decided <slug>

## When to use — trigger phrases

Fire WHEN the conversation just produced any of these:

- Explicit choice between alternatives: "let's go with X", "we'll do A not B", "yes, that approach", "I think option 2 is right"
- Resolution of a tradeoff: discussed pros/cons, converged on one
- A rejected approach with a stated reason: "we won't do X because Y"
- An accepted constraint: "we have to use X because of Y"

Compare to `/knomit-remember`: remember captures *what is*; decided captures *what we chose and why*. If options were weighed, it's a decision.

DON'T fire for:

- Mechanical default choices (no real alternative considered)
- Decisions about the current conversation only (what to say next, how to format)
- Re-stating a decision already captured earlier in the same session

## How

Summarize the conversation into three parts:

1. **Options considered** — what was on the table
2. **Rationale** — why the chosen option won (and why others lost, if it's load-bearing)
3. **The choice** — concrete decision

Then call `mcp__knomit__knomit_learn` with:

- `topic`: `decisions`
- `category`: `<area>/<slug>` (e.g. `synthesize/sumproductnorm-default`)
- `kind`: `epistemic`, `type`: `observation` (decisions are observed choices; the `decisions/` topic folder is what classifies them as decisions)
- `title`: short imperative summary of the choice
- `body`: the three parts above
- `entities`: files/symbols affected
- `refs`: source files touched + URL to the conversation if available
- `confidence`: 0.95

## Ref format for source files (IMPORTANT)

Read your source slug from `.mcp.json` at `mcpServers.knomit.args` (the value right after `--source`).

If the project is in git: `src://<source>/<path>@<commit>` (commit via `git rev-parse HEAD`).

If not in git: `src://<source>/<path>`.

Example: `src://knomit/internal/store/service.go@cfef409`

NEVER write bare paths — knomit's ref resolver treats unscheme'd strings as local fact paths and lookups will fail or clash.
