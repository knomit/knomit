---
name: knomit-harden
description: Use for periodic corpus hardening, or after an incident traced to a misapplied fact — adversarially audits facts that are TRUE but compressible into falsehoods, verifies their implied consequences against the current code, and repairs them (consequence-grained wording, named misreadings, enforcing-code refs)
---

# /knomit-harden [area]

## Why this exists

A fact can be entirely true and still cause failures, because consumers do not
act on facts — they act on the *slogan* they compress a fact into. A fact that
states a compound condition ("rows are keyed by (path, content_hash)")
compresses into a broader slogan ("rows are content-addressed"), and the
slogan yields convenient corollaries the original never claimed ("so renames
are free"). When such a fact sits in a high-retrieval path, every assisted
agent derives the *same* false corollary and ships it — shared knowledge turns
individual random errors into one systematic error.

**A fact's danger is not its falsehood but its most attractive false
corollary.** This skill hunts those, before consumers find them.

Authoring contract enforced by the repairs: state the operational consequence,
name the foreseeable misreading, reference the enforcing code.

## Scope and priority

1. If an area argument is given, audit that area. Otherwise sweep in
   blast-radius order: **invariants → architecture → conventions → decisions**.
   Never edit `kb/principles/` (designer-authored) — flag findings to the user.
2. Build the candidate list with `knomit_query` (path/domain filters,
   `sort=recent`). Prioritize facts that are:
   - type `principle`/`policy` or under `invariants/`, confidence ≥ 0.85
     (high trust × high retrieval = high blast radius);
   - ref'd ONLY to design docs / plans, or with no `src://` code ref at all
     (that alone is a finding — invariants must anchor to enforcing code);
   - pinned at old commits — check drift:
     `git log --oneline <pinned>..HEAD -- <file> | wc -l`;
   - worded in absolute slogans: NEVER, ALWAYS, immutable, content-addressed,
     "keyed by", "skips", "zero cost", "for free".

## The per-fact adversarial read (core loop)

For each candidate, in order:

1. **READ in full** — `knomit_explain` (body, refs, history).
2. **COMPRESS it deliberately** — write the one-sentence slogan a *hurried*
   consumer would carry away. You are simulating the careless reader, not the
   careful one.
3. **DERIVE consequences from the slogan, not from the fact** — 2–4 concrete
   operational inferences someone acting on the slogan would make ("so a
   rename costs nothing", "so I can update the row in place"). Always include
   the inference that would be most *convenient* for the consumer —
   convenience is what makes false corollaries attractive.
4. **VERIFY each inference against the current code** — open the enforcing
   code (the fact's refs first; if it has none, find the code). Verification
   means reading the code path, never re-reading the fact. Quote compound
   keys and conditions verbatim and check the role of EVERY component —
   compound conditions fail by silently dropped components.
5. **CLASSIFY and act:**
   - Every inference true → fact is safe. Add missing `src://` code refs if
     it had none. Move on.
   - Some inference false → **TRAP**. Repair via `/knomit-update`:
     (a) restate the condition uncompressibly (name every component),
     (b) state the operational consequence a consumer needs,
     (c) add a "WHAT THIS DOES NOT MEAN" section naming each false inference
     and why it is false, citing the code,
     (d) resend the full `refs` list with `src://<source>/<path>@<HEAD-commit>`
     anchors to the enforcing code added and stale anchors dropped.
   - The fact's own claim is false at HEAD → `/knomit-retract`, then
     `/knomit-remember` a replacement if warranted.
6. **TENSION check** — query for facts sharing this fact's entities or slogan
   terms. Fragments of two individually-true facts can compose into a false
   inference (one fact: "writes skip re-indexing on cache hits"; another:
   "the index is keyed by row id" — composed carelessly: "renames never
   re-index"). Where fragments compose into one of the false inferences from
   step 3, update BOTH facts to scope their claims explicitly and
   cross-reference each other (fact path in `refs`).

## Discipline

- Repair, don't bloat: add negative-space text only for inferences you PROVED
  false. A fact drowning in speculative caveats is a different failure mode.
- Refs REPLACE wholesale on update — dropping stale anchors is fine, but the
  list you send is the list the fact keeps: an omitted ref is a dropped ref
  (see /knomit-update).
- Every repair's `moment_name` names the audit; every added claim cites the
  code that proved it.
- This costs roughly one code excursion per fact. Work highest-blast-radius
  first, report progress as you go, and stop at a natural boundary with a
  coverage list rather than rushing the tail.

## Output

End with a table: `fact | slogan | false inference caught | action
(safe / updated / retracted / flagged)` — plus the unaudited backlog so the
next session can resume.
