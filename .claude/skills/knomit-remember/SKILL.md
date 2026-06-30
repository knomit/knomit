---
name: knomit-remember
description: Use immediately after discovering something non-obvious, being corrected by the user, or a bug fix exposing a hidden invariant — captures the discovery as a knomit fact so it isn't lost
---

# /knomit-remember

## When to use — trigger phrases

Fire WHEN any of these occur:

- **You just discovered something** during exploration that isn't obvious from the code. Signal phrases in your own thinking: "oh, that's why…", "I didn't realize…", "the actual reason is…", "it turns out…".
- **The user corrected you on a project fact.** Signal phrases from the user: "actually X", "no, it's Y", "wait, that's wrong", "the threshold is Z, not W".
- **A bug fix exposed a hidden invariant** — the bug only happens when a non-obvious precondition is violated.
- **You're about to claim X exists** based on a memory or training-data guess — if you had to verify it, that's a discovery worth keeping.

For **predictions** (something that *might* be true but isn't observed yet): write as a fact with `type: hypothesis` instead of `observation`. Hypotheses carry uncertainty and trigger the confirm/retract lifecycle on dedup collisions.

DON'T fire for:

- Trivial restatements of code that's already self-evident
- Information that belongs in CLAUDE.md (project conventions, file placement)
- One-off task details (in-progress state, current conversation context)

## How

1. Run `mcp__knomit__knomit_query` on the would-be title to surface similar or contradicting existing facts.
2. If a contradicting fact exists: ASK the user whether to `/knomit-update`, `/knomit-retract`, or merge — don't write a duplicate.
3. Otherwise call `mcp__knomit__knomit_learn` with: `topic`, `category`, `title`, `body`, `kind` (default epistemic), `type` (default observation; use `hypothesis` for predictions), `entities`, `refs`, `confidence` 0.85.

## Ref format for source files (IMPORTANT)

Read your source slug from `.mcp.json` at `mcpServers.knomit.args` (the value right after `--source`).

If the project is in git, write source refs as `src://<source>/<path>@<commit>` (get commit via `git rev-parse HEAD`).

If not in git, omit the `@commit`: `src://<source>/<path>`.

Example: `src://knomit/internal/store/service.go@cfef409`

NEVER write bare paths like `internal/store/service.go` — knomit's ref resolver treats unscheme'd strings as local fact paths and lookups will fail or clash.

## Linking to another fact

To reference another knomit fact, put its fact path (the `kb/<topic>/<category>/<uuid>.md` shown in query results) in `refs` — same field as source refs, no scheme. There is NO inline link syntax: `[[name]]` and `[text](path)` are stored as literal body text and resolve to nothing.

(`[[name]]` is the *file-based auto-memory* convention — a different system. Don't carry it into knomit facts.)
