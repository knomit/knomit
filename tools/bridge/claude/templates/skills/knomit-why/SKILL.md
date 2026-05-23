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
