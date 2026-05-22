---
name: review
description: Run a knomit maintenance pass (synthesis, contradictions, stale facts)
---

# /review

On-demand KB maintenance. Calls `mcp__knomit__knomit_review` (async MCP
task). knomit walks the KB to:

- Cluster facts; suggest synthesis (distill duplicates)
- Flag contradictions
- Surface stale facts (refs to deleted files)

Present suggestions to the user; on approval, write synthesis facts and
retract stale ones.
