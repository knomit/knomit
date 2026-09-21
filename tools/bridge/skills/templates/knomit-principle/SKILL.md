---
name: knomit-principle
description: Author a designer principle in knomit. Use ONLY when the user explicitly runs /knomit-principle — agents must NOT invoke this on their own (the validation rules will reject any agent-authored attempt).
---

# /knomit-principle <slug>

## When to use

Fire ONLY when the user runs `/knomit-principle` explicitly. Never invoke proactively. If during work you notice a candidate principle, surface it to the user in chat — *they* run the command if they want it captured.

## Before you start: this base may have no `principles` topic

The topic is not universal. Every knowledge base declares its own ontology, and
a write to a topic the base does not have is REFUSED — `validate path: unknown
topic "principles"`.

**Check first, before asking the user anything.** The knomit server
instructions for this connection list what the base has, under "Available
topics". That list is the WRITE repo's own ontology, which is the one a write
is validated against. If `principles` is on it, run the flow below.

If the list is not in front of you, you have no pre-check and must not invent
one: an unscoped connection computes its instructions before you bind, so it
shows "(no ontology loaded)", and an empty `knomit_query` under a path prefix
cannot tell a missing topic from an empty one. Run the flow and read the
refusal — there, the refusal IS the check.

**If `principles` is absent, REFUSE and say so.** One line to the user: this
knowledge base has no `principles` topic, its topics are `<the actual list>`,
so the principle was not recorded. Then stop — and stop BEFORE collecting the
bucket, domain, title and body, because asking four questions and then throwing
the answers away is the rudest way to discover this.

**Never substitute another topic.** Not `meta`, not `conventions`, not the
nearest-looking one. A principle is designer intent and is READ AS SUCH: it
outranks the tactical rules around it and is the first thing an agent reads
about an area. Filed anywhere else it loses that standing and quietly
misreports what the substitute topic holds. A missing topic is a statement
about what this base chose to keep — hand the choice back to the user, who can
add it to the ontology if they want principles recorded here.

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
5. Call `knomit_learn` with:
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
