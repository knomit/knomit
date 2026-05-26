---
name: knomit-hypothesize
description: Use ONLY when the user explicitly asks to generate predictions or extend synthesis facts forward — runs the work-stealing hypothesize pipeline, distinct from /knomit-review
---

# /knomit-hypothesize

Pipeline tool that walks synthesis facts on the agent branch and lets you decide, per-fact, whether to write a falsifiable prediction (a `type: hypothesis` fact).

Use ONLY when the user has explicitly asked:

- "hypothesize about X"
- "what predictions follow from these synth facts?"
- "extend the synthesis forward"
- "generate hypotheses for the current corpus"

DON'T invoke:

- As an auto follow-up to `/knomit-review` — these are distinct cognitive operations
- To record a single one-off prediction during design work — use `/knomit-remember` with `type: hypothesis` instead
- Without an explicit user trigger — the tool's own description warns against this

## How

Call `mcp__knomit__knomit_hypothesize` with no args to start a session. The tool returns one synthesis fact at a time with a `prompt` and `response_schema`.

For each item:

1. Read the synthesis fact.
2. Decide: is there a concrete, falsifiable prediction that follows? **Skipping is the EXPECTED outcome for most synth facts** — only write when you can articulate a real prediction.
3. Continue with `session_id` + `response` until the queue drains.

## What counts as a hypothesis

A hypothesis is a **falsifiable prediction** about future state, behavior under change, or unobserved consequence.

| ✅ Hypothesis | ❌ Not a hypothesis |
|---|---|
| "Removing the `kind=epistemic` filter in `dirtyFacts` will cause pragmatic facts to be rewritten as epistemic on next review" | "The pipeline will probably get refactored someday" |
| "If `evidence_weight` is computed after delete, weights will silently drop to 0 in 100% of cases" | "Evidence weight is important" |
| "Raising `defaultDedupThreshold` to 0.95 will reduce merges by ~40% based on current corpus similarity distribution" | "Higher threshold = fewer merges" |

Vagueness fails the falsifiability test. Time-bound and quantified predictions pass.

## Type-safety rules (load-bearing)

- Hypotheses must NEVER be evidence for other hypotheses.
- Distill must NEVER synthesize from hypotheses.
- On dedup collision with an existing observation, the hypothesis is retracted and linked via the observation's `refs` (canonical "hypothesis confirmed" lifecycle).
