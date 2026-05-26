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
- Renaming the topic/category — refs are append-only on update; retract + relearn instead

## How

Call `mcp__knomit__knomit_update` with:

- `file`: the fact path (e.g. `kb/decisions/synthesize/.../<uuid>.md`)
- `moment_name`: short label (e.g. `"post-rename dirtyFacts → dirty"`)
- `updates`: ONLY the changed fields (partial)

Common partial updates:

| Change | Field |
|---|---|
| Body text drifted | `body` |
| Add fresh source anchor | `refs` (note: APPENDED to existing — never replaces) |
| Evidence corroborated | `confidence` up, `sources` += new count |
| Evidence weakened | `confidence` down |
| Wrong topic/category | NOT possible via update — retract + relearn |

## Ref format reminder

Source refs must be `src://<source>/<path>@<commit>`. Get `<source>` from `.mcp.json` (`--source` arg) and `<commit>` from `git rev-parse HEAD`. Never write bare paths — knomit's resolver treats unscheme'd strings as local fact paths.

## Refs are APPENDED, not replaced

Updating refs ADDS to the existing list. To remove a stale ref you can't update — you must retract and relearn. Plan accordingly: if a fact has many stale source anchors, retract + relearn is cleaner than 5 append-only updates.
