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

Call `knomit_explain` with the fact path. The tool returns:

- The fact's own body
- Outgoing refs (what this fact references — both source files and other facts), classified as `local` (other fact paths) and `external` (URLs, src:// anchors)
- All resolved at the fact's anchor commit, NOT at HEAD

`knomit_explain` does NOT return incoming references (no backlink index). If you need to find facts that reference this one, use `knomit_query` with the fact path as a search term.

Walk the source-file refs:

- If the ref carries a blob (`@<commit>:<blob>`): compare it against HEAD with
  `git rev-parse HEAD:<path>`. Equal means the fact is still current; different
  means the file changed, and `git diff <blob> HEAD:<path>` shows exactly what.
  If `<path>` is gone at HEAD, `git log --find-object=<blob>` locates where it went.
- If the ref carries only a commit (the legacy form): `git show <commit>:<path>`,
  and note this fails outright if the file did not exist at that commit — one of
  the failure modes the blob form removes.
- For each linked fact: read it too; provenance often runs deep.

Cross-check that referenced files still exist at HEAD — if any are gone, **flag the fact as potentially stale and consider `/knomit-update` or `/knomit-retract`.**

## Interpreting refs in returned facts

- `src://<repo-id>/<path>@<commit>:<blob>` — source code, blob-anchored. `git cat-file blob <blob>` returns the exact bytes the fact was written about, even after a rename or delete.
- `src://<name>/<path>[@<commit>]` — legacy source form, still accepted. Resolve by hand against that repo's checkout.
- `https://…`, `http://…` — external URL
- Anything else (no scheme, no `://`) — a local knomit fact path

For a legacy `src://<name>/…` ref whose `<name>` you cannot map to a checkout, surface it as "in repo `<name>`" rather than trying to open the path locally. For the current form, `<repo-id>` is a root commit: `git rev-list --max-parents=0 HEAD | cut -c1-12` in a candidate checkout tells you whether it is the right repo.

## Important: knomit stores a HISTORICAL graph

All refs and edges resolve at the fact's commit point in time, **never at HEAD**. The "stale" check you're doing is exactly the diff between fact-time and HEAD-time. Don't expect knomit to do this comparison for you — that's why this skill exists.
