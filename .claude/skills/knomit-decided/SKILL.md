---
name: knomit-decided
description: Use the moment a tradeoff is resolved — including answers to a multiple-choice question you put to the user — and BEFORE starting the work that decision authorized. Captures options, rationale, and choice so it isn't re-litigated.
---

# /knomit-decided <slug>

## When to use — trigger phrases

Fire WHEN any of these just happened, and BEFORE you start the resulting work:

- **You put options to the user and they picked one, where the options carried different tradeoffs.** The answer set IS the decision — the choice happened the moment the user selected. This is the single highest-signal trigger.
- Explicit verbal choice: "let's go with X", "we'll do A not B", "yes, that approach", "I think option 2 is right"
- Resolution of a tradeoff: pros/cons discussed, converged on one
- A rejected approach with a stated reason: "we won't do X because Y"
- An accepted constraint: "we have to use X because of Y"

Compare to `/knomit-remember`: remember captures *what is*; decided captures *what we chose and why*. If options were weighed, it's a decision.

## Red flags — STOP, you're rationalizing

These thoughts mean fire the skill NOW, not later:

| Thought | Reality |
|---------|---------|
| "This is just a small/mechanical edit" | Decision size is independent of edit size. Options-considered-and-chosen IS the decision. |
| "I'll capture it after I'm done" | Momentum into the edit means you'll forget or describe the choice post-hoc, losing the rationale. |
| "We can save it later if it matters" | Decisions don't get more capturable over time. The reasoning fades; the choice looks obvious in hindsight. |
| "It was an obvious call" | If options were presented with tradeoffs, it wasn't obvious — it was deliberated. |
| "The answer is in the diff" | Diffs show WHAT changed. Decisions are WHY this approach beat the others. |

## DON'T fire for

- Mechanical default choices (no real alternative considered, e.g. picking a variable name)
- Decisions about the current conversation only (what to say next, how to format a response)
- Re-stating a decision already captured earlier in the same session

## How

Before writing the code/edit the decision authorized, summarize into three parts (plus a fourth when foreseeable):

1. **Options considered** — what was on the table (verbatim from the options you offered, or paraphrased from prose)
2. **Rationale** — why the chosen option won, and why others lost if it's load-bearing
3. **The choice** — concrete decision
4. **Non-scope** (when foreseeable) — one line on what the decision does NOT authorize. Decisions compress into "we always do X" slogans; if the choice was conditional on context, name the boundary so it isn't stretched later.

Then call `knomit_learn` with:

- `topic`: `decisions`
- `category`: `<area>/<slug>` (e.g. `synthesize/sumproductnorm-default`)
- `kind`: `epistemic`, `type`: `observation` (decisions are observed choices; the `decisions/` topic folder is what classifies them as decisions)
- `title`: short imperative summary of the choice
- `body`: the three parts above
- `entities`: files/symbols affected
- `refs`: source files touched + URL to the conversation if available
- `confidence`: 0.95

Only after the fact is committed should you start the implementing edit.

## Ref format for source files (IMPORTANT)

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

The older `src://<name>/<path>@<commit>` form is still accepted everywhere and
is never rewritten — don't "fix" existing refs in that form.

A `kb/…` ref must resolve when the call lands, or knomit REJECTS the whole call.
All facts in ONE knomit_learn call are committed together, so facts written in
the same call may cite each other in any order — including circularly. Only
citations ACROSS calls must point at facts that already exist. When writing a
set of interlinked facts, write them in one call.

Example: `src://knomit/internal/store/service.go@cfef409`

NEVER write bare paths — knomit's ref resolver treats unscheme'd strings as local fact paths and lookups will fail or clash.
