---
name: knomit-bootstrap
description: Use ONLY when an area has zero existing knomit facts and needs first-time seeding — bootstraps the corpus for a subsystem you're about to work in for the first time
---

# /knomit-bootstrap <area>

First-time corpus bootstrap for an area knomit has no facts about yet. Distinct from `/knomit-remember` (one fact at a time, ongoing) and `/knomit-review` (maintenance over existing corpus). This is a recipe for going from empty → enough-to-recall.

## When to use — strict trigger

Fire ONLY when:

- `/knomit-recall <area>` returned zero or near-zero facts
- AND you're about to do substantial work in the area (not just one quick edit)
- AND no other agent is concurrently bootstrapping the same area

DON'T fire for:

- Areas with existing facts — use `/knomit-remember` to add incrementally, or `/knomit-review` to consolidate
- Trivial areas (single file, one concept) — `/knomit-remember` is sufficient
- "Just in case" speculation — bootstrap when you have a real upcoming work driver

## How

### Step 1 — Read the area's surface

- Top-level files in the area (e.g. `internal/synthesize/*.go` if area is "synthesis")
- Recent commits touching the area (`git log --oneline -20 -- <path>`)
- Open documentation in or near the area (READMEs, design docs)

### Step 2 — Draft facts grouped by topic (NOT kind)

Knomit's ontology has fixed top-level **topics** (architecture, conventions, decisions, gotchas, incidents, invariants, meta), and freeform **categories** inside each. The `kind` field is separate (epistemic | pragmatic) and orthogonal to topic.

Draft 3–10 foundational facts covering:

| Topic | What to seed |
|---|---|
| `invariants` | Load-bearing rules that MUST hold (violation breaks the system) |
| `architecture` | Where things live; what struct/function owns what responsibility |
| `conventions` | House style for the area — naming, idioms, file layout |
| `decisions` | Notable design choices you can infer from the code (with rationale) |
| `gotchas` | Surprising or non-obvious behavior worth flagging |

Choose `category` paths 2–4 segments deep that create a navigable tree (e.g. `synthesize/dedup/threshold`, not just `dedup`). See the ontology docs in `internal/fact/ontology_code.yaml`.

### Step 3 — Present drafts to the user before writing

Bootstrapping is high-leverage and high-blast-radius. Show drafts as a list (title + 1-line body summary + topic/category) and ask for approval before writing. **Don't write unilaterally.** A bad bootstrap pollutes the corpus and biases all future recall in the area.

### Step 4 — Write approved facts via `mcp__knomit__knomit_learn`

Single batched `knomit_learn` call with all approved facts. Suggested defaults:

- `confidence`: 0.85 for derived/inferred facts; 0.95 only for facts the user explicitly confirmed
- `kind`: `epistemic` for all (default) unless seeding a policy/heuristic
- `refs`: include `src://<source>/<file>@<commit>` anchors for everything you read
- `entities`: file paths, key symbols, struct/function names
- `moment_name`: `"bootstrap <area>"`

### Step 5 — Verify by running `/knomit-recall <area>` again

After writing, recall and confirm the new facts appear and group sensibly. If facts didn't land where expected, the category paths likely need adjustment — `/knomit-update` to fix individual facts before they're cemented by `/knomit-review`.

## Anti-pattern: bootstrapping speculatively

Don't bootstrap because "we might work in this area someday." Bootstrap when there's a concrete upcoming task that needs the corpus. Otherwise the seed facts grow stale before anyone uses them, and `/knomit-recall` returns noise.
