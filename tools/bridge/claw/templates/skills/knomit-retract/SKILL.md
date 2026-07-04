---
name: knomit-retract
description: Use when a fact is wrong, superseded, or describes something that no longer exists or is no longer true — fully removes it from the active corpus
---

# knomit-retract

Use when:

- The claim is wrong (verified via `knomit-why` or a direct check of the source)
- The fact describes something — a plan, a state of affairs, a rule — that no longer exists or is no longer true
- A new fact supersedes it entirely (write the replacement FIRST, with a ref to the to-be-retracted path so provenance survives)
- Signal phrases from the user: "that's no longer true", "retract X", "delete that fact", "we got rid of Y"

DON'T use for:

- Partial correction — use `knomit-update`
- A stale ref only — `knomit-update` to add a fresh ref (note: refs are append-only, so old ones linger)
- Hypothesis that turned out to be wrong AND has a confirming-observation companion — keep the hypothesis retracted *via the dedup-collision lifecycle in knomit_learn*, not by hand

## How

Call `knomit_retract` with:

- `file`: the fact path (e.g. `kb/decisions/.../<uuid>.md`)
- `moment_name`: short label explaining why (e.g. `"superseded after 2026 policy change"`)

The fact's index entry is marked retracted. Retractions are reversible by relearning — knomit keeps the underlying content in its own internal history. Other facts that reference the retracted one via `refs` are NOT touched; if many do, consider whether they need updates too (`knomit-recall` for downstream impact).

## Supersession pattern

When replacing rather than just deleting:

1. `knomit-remember` (or `knomit-decided`) — write the replacement first, including a `refs` entry pointing at the old path so provenance survives.
2. `knomit-retract` — retract the old one, citing the replacement in `moment_name`.

Order matters: write-then-retract keeps the corpus consistent. Retract-then-write briefly leaves a gap.
