---
name: knomit-recall
description: Use BEFORE brainstorming sessions, planning requests, or any non-trivial work in an area — surfaces invariants, decisions, and prior knowledge so they inform the work from the start
---

# knomit-recall

## When to use — trigger phrases

Fire BEFORE acting on any of these signals:

**Brainstorming / exploration** — recall runs first so the discussion is informed by what already exists:

- About to start open-ended exploration or a creative session
- "let's brainstorm X", "how should we approach Y", "what's the best way to Z", "what are our options for W"
- "plan X", "should we go with A or B for Y", "what would it take to do Z"

**Action requests** — explicit and softer phrasings both count:

- "do X", "start Y", "set up Z", "put together W"
- "change how we handle Y", "revisit our approach to Z"

**Diagnostic / explanatory:**

- About to start substantial work in an area you haven't touched yet this session
- "why do we do X this way?" — rationale question about existing practice or knowledge
- About to decide where new knowledge or work fits relative to what's already known

DON'T fire for:

- Trivial follow-ups within a topic you're actively iterating on
- Questions answerable from what's already in front of you
- After you've already recalled in this session for the SAME topic

## How

Call `knomit_query` with:

- `text`: the user-supplied topic (or your own one-line summary of the area)
- `entities`: any people, places, concepts, or things directly involved
- `applies_to`: the area path the work targets (e.g. `finance/budgeting`). Derive from an explicit user-supplied area, OR from the dominant topic under discussion. Omit if uncertain; text/entities matching still works.

**Empty result?** Note "no prior facts in this area — proceeding" and continue. Empty results are common in unfamiliar areas; not a blocker. When `applies_to` is set, missing matches mean no principle applies at this scope — proceed with text/entity results as today.

When the query returns facts, do BOTH steps below. Skipping step 2 means you're trusting facts that may be stale — corpus facts can lag reality.

### Step 1 — Read in priority order

1. **Principles first** (`kb/principles/`) — stated intent. Scoped principles are the *first* thing to read in an area; they trump tactical rules. Global-domain principles (`domain` contains `global`) apply everywhere — read them too unless you've already internalized them earlier this session.
2. **Invariants** (`kb/invariants/`) — load-bearing rules. Violating one breaks something important; if your plan needs to, STOP and confirm with the user.
3. **Decisions** (`kb/decisions/`) — the *why* behind the current approach.
4. **Conventions** — established practice for the area.
5. **Scan all bodies for "anti-pattern:"** — cheapest constraint you'll find.

### Step 2 — Verify the load-bearing claims

Pick the 3–5 facts whose specific claims (numbers, ordering, names, relationships) your work will depend on. For each:

- If it has a ref (URL, document title/ID, or dated source): re-check that source. If anything load-bearing has drifted or the source no longer supports the claim, run `knomit-update` or `knomit-retract` BEFORE building on the fact.
- If it has no ref at all: lower your trust accordingly; corroborate before relying on it.

## Interpreting refs in returned facts

Refs are whatever lets you re-check a claim: URLs, document titles/IDs, dates, citations, or other external sources. A fact may have no ref at all — that just means it isn't externally checkable; weigh it accordingly rather than treating the absence as an error.
