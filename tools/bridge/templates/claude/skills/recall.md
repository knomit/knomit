---
name: recall
description: Query knomit for top facts on a topic, grouped by kind
---

# /recall <topic-or-text>

Use BEFORE non-trivial work in a known-fact area:

- Picking where new code goes
- Editing/writing files under areas with known invariants
- Implementing a pattern that may already exist
- Answering "why does X work this way?"

How: call the knomit MCP tool `mcp__knomit__knomit_query` with the
user-supplied topic as `text`, plus any open file paths as `entities`.
Group the response by `kind` and show invariants first.
