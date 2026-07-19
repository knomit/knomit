---
name: knomit-remember
description: Use immediately after discovering something non-obvious, being corrected by the user, or finding a hidden fact behind a problem — captures the discovery as a knomit fact so it isn't lost
---

# knomit-remember

## When to use — trigger phrases

Fire WHEN any of these occur:

- **You just discovered something** during exploration that isn't obvious from the surrounding material. Signal phrases in your own thinking: "oh, that's why…", "I didn't realize…", "the actual reason is…", "it turns out…".
- **The user corrected you on a fact.** Signal phrases from the user: "actually X", "no, it's Y", "wait, that's wrong", "the number is Z, not W".
- **Resolving an error or problem exposed a hidden rule** — the issue only happens when a non-obvious precondition is violated.
- **You're about to claim X exists** based on a memory or training-data guess — if you had to verify it, that's a discovery worth keeping.

For **predictions** (something that *might* be true but isn't observed yet): write as a fact with `type: hypothesis` instead of `observation`. Hypotheses carry uncertainty and trigger the confirm/retract lifecycle on dedup collisions.

DON'T fire for:

- Trivial restatements of something already self-evident from context
- Information that belongs in the user's own reference material or existing documentation
- One-off task details (in-progress state, current conversation context)

## How

1. Run `knomit_query` on the would-be title to surface similar or contradicting existing facts.
2. If a contradicting fact exists: ASK the user whether to `knomit-update`, `knomit-retract`, or merge — don't write a duplicate.
3. Otherwise call `knomit_learn` with: `topic`, `category`, `title`, `body`, `kind` (default epistemic), `type` (default observation; use `hypothesis` for predictions), `entities`, `refs`, `confidence` 0.85.

## Refs

Refs are optional and external: a URL, a document title/ID, a date, or another citable source. Leave `refs` empty when a fact isn't tied to anything externally checkable — that's normal, not an error.

## Linking to another fact

To reference another knomit fact, put its fact path (the `kb/<topic>/<category>/<uuid>.md` shown in query results) in `refs` — same field as external refs. There is NO inline link syntax: `[[name]]` and `[text](path)` are stored as literal body text and resolve to nothing.

(`[[name]]` is the *file-based auto-memory* convention — a different system. Don't carry it into knomit facts.)
