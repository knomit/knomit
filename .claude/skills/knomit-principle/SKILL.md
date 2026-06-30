---
name: knomit-principle
description: Author a designer principle in knomit. Use ONLY when the user explicitly runs /knomit-principle — agents must NOT invoke this on their own (the validation rules will reject any agent-authored attempt).
---

# /knomit-principle <slug>

## When to use

Fire ONLY when the user runs `/knomit-principle` explicitly. Never invoke proactively. If during work you notice a candidate principle, surface it to the user in chat — *they* run the command if they want it captured.

## Flow

1. Ask the user which **bucket** the principle belongs to:
   - `mission` — what knomit exists to solve, who it's for
   - `philosophy` — mental-model rules that span subsystems
   - `anti-patterns` — choices the designer rejects
   - `ux` — interaction taste, voice, pacing
2. Ask the user for the **domain** (scope):
   - `global` — surfaces every SessionStart. Confirm: "this loads every session — sure?"
   - An area path (e.g. `store/resolver`) — surfaces only when `/knomit-recall` touches that area or any parent.
3. Ask the user for the **title** (short, imperative — the principle as a statement).
4. Ask the user for the **body** (rationale, scope of applicability, examples of what NOT to do).
5. Call `mcp__knomit__knomit_learn` with:
   - `topic`: `principles`
   - `category`: `<bucket>/<slug>` (where `<slug>` is the argument the user passed to `/knomit-principle`)
   - `title`: the user's title
   - `body`: the user's body
   - `kind`: `pragmatic`
   - `type`: `policy`
   - `domain`: `[<global or area>]`
   - `entities`: `[designer]`
   - `confidence`: `1.0`

## Why `entities: [designer]` is required

The code ontology declares validation rules that reject any write to `kb/principles/*` without `designer` in entities. This skill is the only sanctioned authoring path because the user runs it. Agent-driven flows (`/knomit-remember`) do NOT set this entity and will be rejected server-side.

## On failure

If the server returns a validation error (e.g. `must-have-designer-entity`, `domain-mutually-exclusive`, `must-be-pragmatic-policy`, `domain-non-empty`), surface the message verbatim and ask the user to clarify the input. Do NOT silently retry or auto-fix.
