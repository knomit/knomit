---
name: knomit-why
description: Use when about to rely on a specific fact's claims for a decision — walks the provenance graph and flags stale anchors before you build on it
---

# knomit-why

Use when:

- `knomit-recall` surfaced a fact whose specific claims (numbers, ordering, names, relationships) your work will depend on, AND the fact is non-trivially old or load-bearing
- User asks "why was this done this way?" referring to something the fact explains
- You suspect a fact is stale and want to verify before update/retract

DON'T fire for:

- Facts you're only skimming for context (not relying on specific claims)
- Facts you just wrote yourself this session
- The whole result set of a recall — pick the 3–5 load-bearing ones and verify those (see the knomit-recall skill's step 2)

## How

Call `knomit_explain` with the fact path. The tool returns:

- The fact's own body
- Outgoing refs (what this fact references — both external sources and other facts), classified as `local` (other fact paths) and `external` (URLs, documents, other sources)
- All resolved as-of the fact's own version — a knomit-internal detail of how the versioned provenance graph is stored, not something you need to manage

`knomit_explain` does NOT return incoming references (no backlink index). If you need to find facts that reference this one, use `knomit_query` with the fact path as a search term.

Walk the refs:

- For each external ref (URL, document, dataset, etc.): re-examine that source. Does it still support the claim? If a load-bearing source has changed or vanished, the fact may be stale.
- For each linked fact: read it too; provenance often runs deep.

If a source can no longer be confirmed or has visibly changed, **flag the fact as potentially stale and consider `knomit-update` or `knomit-retract`.**

## Verifying against ground truth

The check you're doing is: does the cited source still say what the fact claims? knomit doesn't do this comparison for you — that's why this skill exists. A fact with no ref at all can't be re-verified this way; treat it as lower-confidence and corroborate independently if it's load-bearing.
