## File placement rules

- Plans, todos, changelogs, and any working/scratch files go in `.claude/plans/`
- NEVER create files in `docs/` unless explicitly asked to update documentation
- NEVER create `plan.md`, `todo.md`, `notes.md` or similar at the project root
- Commit messages and session logs go in `.claude/plans/`

<!-- knomit:integration -->
## Working with knomit memory

This project uses knomit as long-term memory. Six slash commands wrap
knomit's MCP tools. Use them in these moments:

**Before non-trivial work** — call `/recall <area>` before:

- Editing or writing files under `internal/store/` (resolver, vtab, refs/edges — see historical-graph invariant) or `internal/mcp/` (MCP profile boundaries)
- Picking where new code goes
- Implementing a pattern that may already exist
- Answering "why does X work this way?"

**After a discovery** — call `/remember` when:

- You found something non-obvious during exploration
- The user corrected you on a project fact
- A bug fix exposed a hidden invariant

**After a design choice** — call `/decided <slug>` when you and the user
resolved a tradeoff in conversation.

**When you doubt a fact** — call `/why <fact-path>` to walk the provenance
graph and verify against current code before relying on it.

**Philosophy** — knomit is your colleague's tribal knowledge. Invariants are
load-bearing; re-read before touching the area they cover. Facts can be
stale; `/why` is cheap. When uncertain, `/recall` — also cheap.
<!-- /knomit:integration -->