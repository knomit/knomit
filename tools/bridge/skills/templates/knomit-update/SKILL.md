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

Call `knomit_update` with:

- `file`: the fact path (e.g. `kb/decisions/synthesize/.../<uuid>.md`)
- `moment_name`: short label (e.g. `"post-rename dirtyFacts → dirty"`)
- `updates`: ONLY the changed fields (partial). Any key not in the schema fails
  the whole call: body edits go in `ops`, and the path and origin cannot change
- `ops`: edits to the body, in place of `updates.body` (see below)
- `if_commit` (optional): the `commit` knomit_explain returned when you read the fact

Common partial updates:

| Change | Field |
|---|---|
| A sentence or section drifted | `ops` (`str_replace`) |
| New material at the end | `ops` (`append`) |
| Body rewritten top to bottom | `body` |
| Add, refresh, or drop source anchors | `refs` (REPLACES the whole list — send every ref the fact should keep) |
| Evidence corroborated | `confidence` up, `sources` += new count |
| Evidence weakened | `confidence` down |
| Wrong topic/category | NOT possible via update — retract + relearn |

## Edit the body with ops, not a resent body

`ops` edits the body in place, so you send only what changes. Use it for any
edit to a large fact. Send `ops` or `updates.body`, never both; `ops` combines
with every other `updates` field.

```json
"ops": [
  {"op": "str_replace", "old_str": "the gate runs twice", "new_str": "the gate runs once"},
  {"op": "append", "text": "A closing paragraph."}
]
```

- `str_replace`: `old_str` must occur EXACTLY ONCE in the body, byte for byte.
  `new_str: ""` deletes it.
- `append`: adds `text` as a new paragraph at the end of the body.
- Ops apply in order, each to the body the one before it produced. They land
  as ONE revision or not at all; an error names the failing op by index.
- On 0 matches, the error gives the longest prefix of `old_str` that does
  occur, its byte offset, and the first differing character on each side as
  `U+XXXX`. Fix `old_str` from that: smart quotes, em dash vs `--`,
  non-breaking space and trailing whitespace are the usual causes. On 2 or
  more matches, widen `old_str` with surrounding text until it is unique.
- Copy `old_str` from the body `knomit_explain` returned, not from memory.

Pass `if_commit` with the `commit` from that same explain call. If the fact
changed since you read it, nothing is written and the error gives
`current_commit`. Read the fact again at that commit and rebuild your ops.

An edit that leaves an unclosed ```` ``` ```` fence in a body whose fences were
balanced is refused, on both paths.

## Body updates replace the WHOLE body — preserve hardening

`updates.body` replaces the entire body, not a section of it. When the existing fact carries a "WHAT THIS DOES NOT MEAN" section or fully-enumerated compound conditions (typically added by /knomit-harden), carry them into the new body unchanged unless your evidence specifically disproves them. A routine drift fix that drops these sections silently un-hardens the fact.

## Ref format reminder

Source refs are `src://<repo-id>/<path>@<commit>:<blob>`. Produce the three
components from the repo you are citing:

```bash
git rev-list --max-parents=0 HEAD | cut -c1-12   # <repo-id>
git rev-parse HEAD                                # <commit>, full 40 hex
git rev-parse <commit>:<path>                     # <blob>, full 40 hex
```

**Which id?** `<repo-id>` in a `src://` ref is the **source repo's** root commit,
computed by running git in the checkout you are citing — it is NOT a knomit repo
id, and it is the only id you ever supply.

For `kb://` refs you never build an id at all. Cite a fact in this repo by its
bare path; knomit rewrites it to the canonical `kb://<repo-id>/<path>` form on
write. Cite a fact in another repo by copying the `kb://<id>/…` path verbatim
from the query or explain result that gave it to you.

That last command FAILING is the check: it means the file did not exist at that
commit. **Never cite source that does not exist in the repo's history** — knomit
holds fact blobs only, never source, so it cannot verify a src ref for you.

Add `#L<start>-L<end>` when the fact is about specific lines rather than a whole
file. The blob is what makes the citation durable: `git cat-file blob <blob>`
returns the exact bytes even after the file is renamed or deleted, which a
commit-only ref cannot.

A `src://` ref you add is `src://<repo-id>/<path>@<commit>:<blob>`, all three
values computed with the commands above, never typed or abbreviated. Leave refs
already on a fact as they are.

A `kb/…` ref must resolve when the call lands, or knomit REJECTS the whole call.
All facts in ONE knomit_learn call are committed together, so facts written in
the same call may cite each other in any order — including circularly. Only
citations ACROSS calls must point at facts that already exist. When writing a
set of interlinked facts, write them in one call.

## Refs REPLACE the whole list

`updates.refs` replaces the entire list — any existing ref you leave out is DROPPED. Read the fact's current refs first (`knomit_explain` or query output), then resend the full merged list: every ref to keep, plus fresh anchors, minus stale ones. Omit the `refs` field entirely to leave refs untouched.

Dropping a ref only affects this and future revisions — prior revisions keep their refs in git history and their DERIVED_FROM provenance edges, so replacing refs never erases past provenance.
