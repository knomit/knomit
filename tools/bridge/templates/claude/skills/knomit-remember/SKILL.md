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
