---
name: knomit-kickoff-area
description: Structured per-area seed pass for knomit
---

# /knomit-kickoff-area <area>

Use to bootstrap knomit knowledge for one subsystem.

Steps:
1. Read the area's main files and recent commits.
2. Draft foundational facts in each kind:
   - invariants (load-bearing rules)
   - architecture (what lives where)
   - conventions (how things are done)
   - any known decisions / gotchas / incidents
3. Present each draft to the user for review.
4. Write approved facts via `mcp__knomit__knomit_learn` at confidence 0.95.
