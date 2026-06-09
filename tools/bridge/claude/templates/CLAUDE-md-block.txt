<!-- knomit:integration -->
## Working with knomit memory

This project uses knomit as long-term memory. Nine `/knomit-…` slash commands
wrap knomit's MCP tools. Use them in these moments:

**Before non-trivial work** — call `/knomit-recall <area>` before:
- Editing or writing files under <KNOWN_INVARIANT_PATHS>
- Picking where new code goes
- Implementing a pattern that may already exist
- Answering "why does X work this way?"

After recall returns, VERIFY load-bearing claims (3–5 facts your work
depends on) against HEAD before building on them. See the skill for the
verification handshake.

**After a discovery** — call `/knomit-remember` when:
- You found something non-obvious during exploration
- The user corrected you on a project fact
- A bug fix exposed a hidden invariant
- You want to record a falsifiable prediction (use type=hypothesis)

**After a design choice** — call `/knomit-decided <slug>` when you and the
user resolved a tradeoff in conversation. Captures options, rationale, and
the choice — not just what was chosen.

**When you doubt a fact** — call `/knomit-why <fact-path>` to walk the
provenance graph and verify against current code before relying on it.

**When a fact has drifted** — call `/knomit-update <fact-path>` if part of
the fact needs correcting (body, confidence, fresh ref); call
`/knomit-retract <fact-path>` if the fact is wholly wrong or its subject no
longer exists.

**On explicit user request only** — call `/knomit-hypothesize` to walk
synthesis facts and generate falsifiable predictions. Distinct from
`/knomit-review`; never invoke as an auto follow-up.

**After a burst of fact-writing or large refactor** — call `/knomit-review`
to run the work-stealing prune (dedup), distill (synthesis), and reflect
(methodology) pipeline. Session loop, not fire-and-forget.

**When an area has zero facts and you're about to work in it** — call
`/knomit-bootstrap <area>` to seed foundational facts (invariants,
architecture, conventions, decisions, gotchas). Strict trigger: only first
time, with a real upcoming work driver.

**Philosophy** — knomit is your colleague's tribal knowledge. Invariants are
load-bearing; re-read before touching the area they cover. Facts can be
stale; `/knomit-why` and `/knomit-update`/`/knomit-retract` are how you
keep the corpus from rotting. When uncertain, `/knomit-recall` — also cheap.
<!-- /knomit:integration -->
