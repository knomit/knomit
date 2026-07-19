---
name: knomit-decided
description: Use the moment a tradeoff is resolved — including answers to a multiple-choice question you posed the user — and BEFORE starting the work that decision authorized. Captures options, rationale, and choice so it isn't re-litigated.
---

# knomit-decided

## When to use — trigger phrases

Fire WHEN any of these just happened, and BEFORE you start the resulting work:

- **A multiple-choice question you posed the user was answered, where the options carried different tradeoffs.** The answer set IS the decision — the choice happened the moment the user selected. This is the single highest-signal trigger.
- Explicit verbal choice: "let's go with X", "we'll do A not B", "yes, that approach", "I think option 2 is right"
- Resolution of a tradeoff: pros/cons discussed, converged on one
- A rejected approach with a stated reason: "we won't do X because Y"
- An accepted constraint: "we have to use X because of Y"

Compare to `knomit-remember`: remember captures *what is*; decided captures *what we chose and why*. If options were weighed, it's a decision.

## Red flags — STOP, you're rationalizing

These thoughts mean fire the skill NOW, not later:

| Thought | Reality |
|---------|---------|
| "This is just a small/minor thing" | Decision size is independent of the size of the follow-up work. Options-considered-and-chosen IS the decision. |
| "I'll capture it after I'm done" | Momentum into the work means you'll forget or describe the choice post-hoc, losing the rationale. |
| "We can save it later if it matters" | Decisions don't get more capturable over time. The reasoning fades; the choice looks obvious in hindsight. |
| "It was an obvious call" | If options were presented with tradeoffs, it wasn't obvious — it was deliberated. |
| "The outcome speaks for itself" | The outcome shows WHAT was chosen. Decisions are WHY this approach beat the others. |

## DON'T fire for

- Mechanical default choices (no real alternative considered, e.g. picking a label)
- Decisions about the current conversation only (what to say next, how to format a response)
- Re-stating a decision already captured earlier in the same session

## How

Before acting on the decision, summarize into three parts:

1. **Options considered** — what was on the table (verbatim from the multiple-choice question, or paraphrased from prose)
2. **Rationale** — why the chosen option won, and why others lost if it's load-bearing
3. **The choice** — concrete decision

Then call `knomit_learn` with:

- `topic`: `decisions`
- `category`: `<area>/<slug>` (e.g. `finance/budgeting/annual-review-cadence`)
- `kind`: `epistemic`, `type`: `observation` (decisions are observed choices; the `decisions/` topic folder is what classifies them as decisions)
- `title`: short imperative summary of the choice
- `body`: the three parts above
- `entities`: people, places, concepts, or things affected
- `refs`: any URL, document, or citation backing the decision, if one exists
- `confidence`: 0.95

Only after the fact is committed should you start the work the decision authorized.

## Refs

Refs are optional and external: a URL, a document title/ID, a date, or another citable source. Leave `refs` empty when the decision isn't tied to any externally checkable source — that's normal, not an error.
