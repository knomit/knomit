---
name: knomit-update
description: Use when an existing fact's body, confidence, or refs no longer match reality — update in place instead of writing a duplicate
---

# /knomit-update <fact-path>

Use when:

- `/knomit-why` exposed drift between a fact and HEAD
- Code you just edited makes part of an existing fact wrong
- New evidence raises or lowers the confidence of an existing claim
- A signal phrase from the user: "that fact is out of date", "update the X fact", "the threshold is now Y"

DON'T use for:

- A new, related observation — use `/knomit-remember`
- A fact that is wholly wrong or supersedes — use `/knomit-retract` (and then `/knomit-remember` if a replacement is warranted)
- Renaming the topic/category — the fact path is fixed at creation; retract + relearn instead

## How

Call `mcp__knomit__knomit_update` with:

- `file`: the fact path (e.g. `kb/decisions/synthesize/.../<uuid>.md`)
- `moment_name`: short label (e.g. `"post-rename dirtyFacts → dirty"`)
- `updates`: ONLY the changed fields (partial)

Common partial updates:

| Change | Field |
|---|---|
| Body text drifted | `body` |
| Add, refresh, or drop source anchors | `refs` (REPLACES the whole list — send every ref the fact should keep) |
| Evidence corroborated | `confidence` up, `sources` += new count |
| Evidence weakened | `confidence` down |
| Wrong topic/category | NOT possible via update — retract + relearn |

## Body updates replace the WHOLE body — preserve hardening

`updates.body` replaces the entire body, not a section of it. When the existing fact carries a "WHAT THIS DOES NOT MEAN" section or fully-enumerated compound conditions (typically added by /knomit-harden), carry them into the new body unchanged unless your evidence specifically disproves them. A routine drift fix that drops these sections silently un-hardens the fact.

## Ref format reminder

Source refs must be `src://<source>/<path>@<commit>`. Get `<source>` from `.mcp.json` (`--source` arg) and `<commit>` from `git rev-parse HEAD`. Never write bare paths — knomit's resolver treats unscheme'd strings as local fact paths.

## Refs REPLACE the whole list

`updates.refs` replaces the entire list — any existing ref you leave out is DROPPED. Read the fact's current refs first (`knomit_explain` or query output), then resend the full merged list: every ref to keep, plus fresh anchors, minus stale ones. Omit the `refs` field entirely to leave refs untouched.

Dropping a ref only affects this and future revisions — prior revisions keep their refs in git history and their DERIVED_FROM provenance edges, so replacing refs never erases past provenance.
