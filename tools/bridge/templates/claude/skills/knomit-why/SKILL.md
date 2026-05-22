---
name: knomit-why
description: Walk a fact's provenance graph to verify before relying on it
---

# /knomit-why <fact-path>

Use when:
- You doubt a stored fact and want to verify against current code
- User asks "why was this done this way?"

How: call `mcp__knomit__knomit_explain` with the fact path. Walk the refs
graph; surface source-file lineage at the anchor commit and linked facts.
Cross-check that referenced files still exist at HEAD — if any are gone,
flag the fact as potentially stale.
