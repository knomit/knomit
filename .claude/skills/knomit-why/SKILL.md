---
name: knomit-why
description: Use when about to rely on a specific fact's claims for a decision or implementation — walks the provenance graph and flags stale anchors before you build on it
---

# /knomit-why <fact-path>

Use when:

- `/knomit-recall` surfaced a fact whose specific claims (thresholds, ordering, paths, signatures) your work will depend on, AND the fact is non-trivially old or load-bearing
- User asks "why was this done this way?" referring to current behavior the fact explains
- You suspect a fact is stale and want to verify before update/retract

DON'T fire for:

- Facts you're only skimming for context (not relying on specific claims)
- Facts you just wrote yourself this session
- The whole result set of a recall — pick the 3–5 load-bearing ones and verify those (see the knomit-recall skill's step 2)

## How

Call `mcp__knomit__knomit_explain` with the fact path. The tool returns:

- The fact's own body
- Outgoing refs (what this fact references — both source files and other facts), classified as `local` (other fact paths) and `external` (URLs, src:// anchors)
- All resolved at the fact's anchor commit, NOT at HEAD

`knomit_explain` does NOT return incoming references (no backlink index). If you need to find facts that reference this one, use `mcp__knomit__knomit_query` with the fact path as a search term.

Walk the source-file refs:

- For each `src://<source>/<path>@<commit>` where `<source>` matches your session: run `git show <commit>:<path>` to see what the fact was anchored to. Compare against HEAD; if anything load-bearing has drifted, the fact may be stale.
- For each linked fact: read it too; provenance often runs deep.

Cross-check that referenced files still exist at HEAD — if any are gone, **flag the fact as potentially stale and consider `/knomit-update` or `/knomit-retract`.**

## Interpreting refs in returned facts

- `src://<source>/<path>@<commit>` — source file in source repo `<source>` at a specific commit. If `<source>` matches your session's source (read `--source` from `.mcp.json`), the file may have changed since `<commit>` — use `git show <commit>:<path>` to see the version the fact was anchored to.
- `src://<source>/<path>` — source file with no git anchor. If `<source>` matches your session, read the file directly; the fact was captured without commit-pinning.
- `https://…`, `http://…` — external URL
- Anything else (no scheme, no `://`) — a local knomit fact path

If `<source>` doesn't match your session's source, surface it as "in repo `<source>`" rather than trying to open the path locally.

## Important: knomit stores a HISTORICAL graph

All refs and edges resolve at the fact's commit point in time, **never at HEAD**. The "stale" check you're doing is exactly the diff between fact-time and HEAD-time. Don't expect knomit to do this comparison for you — that's why this skill exists.
