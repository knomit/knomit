---
name: decided
description: Capture a design decision made during the session
---

# /decided <slug>

Use after a tradeoff was resolved in conversation. The skill summarizes:

1. Options considered (what was discussed)
2. Rationale (why the chosen option won)
3. The choice (concrete decision)

Then writes a fact via `mcp__knomit__knomit_learn`:

- kind: `decision`
- topic path: `decisions/accepted/<YYYY-MM>-<slug>`
- refs: include any files touched + URL to the conversation if available
- confidence: 0.95
