---
name: knomit-review
description: Use when the corpus needs maintenance — duplicates suspected after a burst of /knomit-remember, drift accumulating after refactors, or as a periodic prune/distill/reflect pass
---

# /knomit-review

Work-stealing KB-maintenance pipeline. Three phases run in order over the dirty-fact window:

1. **Prune** — dedup near-identical facts (cosine ≥ 0.92); the winner inherits union of domains/entities and summed sources, hypothesis loses to observation regardless of confidence.
2. **Distill** — synthesize clusters of related facts into higher-level `type: synthesis` facts; RAPTOR runs up to depth 3 through the same queue.
3. **Reflect** — detect hypothesis transitions (confirmed/retracted) and optionally emit one new methodology (capped by `KNOMIT_REFLECT_PROPOSE_CAP`, novelty-gated by `KNOMIT_REFLECT_NOVELTY_THRESHOLD`).

Use when:

- You just landed a burst of `/knomit-remember` calls and want them deduped against the existing corpus
- Refactoring touched many files and corpus facts may now describe deleted code (stale src:// refs)
- Periodic maintenance: invoking once after a logical chunk of work integrates new facts into the corpus
- Signal phrases: "review knomit", "clean up the corpus", "synthesize", "look for duplicates"

DON'T invoke:

- Mid-task — let the work settle first
- To generate predictions — that's `/knomit-hypothesize` (distinct pipeline)
- On a clean session with no recent fact-writing — nothing to dedup

## How — it's a session loop, not fire-and-forget

`knomit_review` is a **work-stealing session**, not an async task. The tool returns ONE work item at a time. The calling model burns its own cycles processing each item until the queue drains.

1. First call: `mcp__knomit__knomit_review` with no args.
   - Returns `{session_id, work_item: {prompt, response_schema}}` for the first item, OR `{session_id, done: true}` if nothing dirty.
2. Process the work item:
   - Read the `prompt` and produce a JSON response matching `response_schema`.
   - For prune items: decide which facts to merge (and the merged content) vs keep distinct.
   - For distill items: decide whether to synthesize a higher-level fact from the cluster, and write its content.
   - For reflect items: emit hypothesis transitions and optionally one methodology.
3. Continue: `mcp__knomit__knomit_review` with `session_id` and `response`.
   - Server applies your decisions (writes new facts, retracts duplicates) and returns the next work item.
4. Loop until response includes `done: true` — that completes the session and advances the watermark.

## Priority ordering inside the queue

The server returns items in this order (highest priority first):

- Prune items — priority = cluster size (more candidates first)
- Distill depth 0 (initial seeds) — priority 0
- Distill depth 1 (RAPTOR roll-up) — priority −1
- Distill depth 2 — priority −2
- Distill depth 3 — priority −3
- Reflect item — priority −100 (singleton at the end)

## Stopping early

Abandoning a session (not calling Continue) does NOT advance the watermark — same dirty window will be re-clustered next time. But any decisions you already applied during the session DO stay committed. Safe to abort if results look wrong.

## Decisions are validated server-side

The server enforces type safety on every applied decision: hypothesis facts can't be evidence for hypothesis facts; distill can't synthesize from hypotheses; LLM-emitted fact paths are normalized to 8-char UUIDs and validated against `ontologyRoot`. Bad responses are rejected with a warning, not silently written.
