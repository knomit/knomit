---
name: knomit-recall
description: Query knomit for top facts on a topic, grouped by kind
---

# /knomit-recall <topic-or-text>

Use BEFORE non-trivial work in a known-fact area:

- Picking where new code goes
- Editing/writing files under areas with known invariants
- Implementing a pattern that may already exist
- Answering "why does X work this way?"

How: call the knomit MCP tool `mcp__knomit__knomit_query` with the
user-supplied topic as `text`, plus any open file paths as `entities`.
Group the response by `kind` and show invariants first.

**Interpreting refs in returned facts:**

When knomit returns a fact's `refs`, you may see:

- `src://<source>/<path>@<commit>` — source file in source repo `<source>`
  at a specific commit. If `<source>` matches your session's source (read
  `--source` from `.mcp.json`), the file may have changed since `<commit>` —
  use `git show <commit>:<path>` to see the version the fact was anchored to.
- `src://<source>/<path>` — source file with no git anchor. If `<source>`
  matches your session, read the file directly; the fact was captured
  without commit-pinning.
- `https://…`, `http://…` — external URL
- Anything else (no scheme, no `://`) — a local knomit fact path

If `<source>` doesn't match your session's source, surface it as "in repo
`<source>`" rather than trying to open the path locally.
