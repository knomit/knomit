---
name: knomit-update
description: Use when an existing fact's body, confidence, or refs no longer match reality — update in place instead of writing a duplicate
---

# knomit-update

Use when:

- `knomit-why` exposed drift between a fact and its cited source
- Something you just learned makes part of an existing fact wrong
- New evidence raises or lowers the confidence of an existing claim
- A signal phrase from the user: "that fact is out of date", "update the X fact", "the number is now Y"

DON'T use for:

- A new, related observation — use `knomit-remember`
- A fact that is wholly wrong or supersedes — use `knomit-retract` (and then `knomit-remember` if a replacement is warranted)
- Renaming the topic/category — refs are append-only on update; retract + relearn instead

## How

Call `knomit_update` with:

- `file`: the fact path (e.g. `kb/decisions/finance/.../<uuid>.md`)
- `moment_name`: short label (e.g. `"corrected after 2026 review"`)
- `updates`: ONLY the changed fields (partial)

Common partial updates:

| Change | Field |
|---|---|
| Body text drifted | `body` |
| Add fresh source anchor | `refs` (note: APPENDED to existing — never replaces) |
| Evidence corroborated | `confidence` up, `sources` += new count |
| Evidence weakened | `confidence` down |
| Wrong topic/category | NOT possible via update — retract + relearn |

## Refs

Refs are optional and external: a URL, a document title/ID, a date, or another citable source. Leave `refs` empty when there's nothing externally checkable to cite.

## Refs are APPENDED, not replaced

Updating refs ADDS to the existing list. To remove a stale ref you can't update — you must retract and relearn. Plan accordingly: if a fact has many stale refs, retract + relearn is cleaner than several append-only updates.
