## File placement rules

- Plans, todos, changelogs, and any working/scratch files go in `.claude/plans/`
- NEVER create files in `docs/` unless explicitly asked to update documentation
- NEVER create `plan.md`, `todo.md`, `notes.md` or similar at the project root
- Commit messages and session logs go in `.claude/plans/`

<!-- knomit:integration v3 -->
## Working with knomit memory

This project uses knomit as long-term memory. Eleven `/knomit-…` slash commands
wrap knomit's MCP tools. Use them in these moments:

**Before non-trivial work** — call `/knomit-recall <what you are about to
work on>` before:
- Editing or writing code in an area you have not already recalled for this session
- Picking where new code goes
- Implementing a pattern that may already exist
- Answering "why does X work this way?"
- Committing to an explanation of a failure — before you name the cause,
  not after. Reproducing a bug tells you THAT it happens; the corpus may
  already say WHY, and may say your reproduction points at the wrong cause.
- Briefing a subagent on an area — recall for them and put it in the brief,
  or tell them to recall.

The argument is free text, not a taxonomy term — `/knomit-recall lens write-repo
resolution` is a valid call. Name a KIND of knowledge as well when you want one:
`/knomit-recall invariants for internal/fact` scopes the query to the
`invariants` topic. The kinds are this repo's ontology topics, listed in the
knomit MCP server instructions; the skill turns the name you type into the
path filter.

Recall is a habit, not a phase: two recalls at the start of a session do
not cover the debugging you do an hour later.

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

**Periodically, or after an incident traced to a misapplied fact** — call
`/knomit-harden [area]` to adversarially audit high-blast-radius facts:
compress each to the slogan a hurried reader would take away, derive the
consequences a consumer would act on, verify those against the code, and
repair the traps (consequences, named misreadings, enforcing-code refs).

**When an area has zero facts and you're about to work in it** — call
`/knomit-bootstrap <area>` to seed foundational facts (invariants,
architecture, conventions, decisions, gotchas). Strict trigger: only first
time, with a real upcoming work driver.

**Philosophy** — knomit is your colleague's tribal knowledge. Invariants are
load-bearing; re-read before touching the area they cover. Facts can be
stale; `/knomit-why` and `/knomit-update`/`/knomit-retract` are how you
keep the corpus from rotting. When uncertain, `/knomit-recall` — also cheap.
When you have just measured your way to a confident cause, recall then too:
that is when the felt gap is gone but the ignorance isn't.
<!-- /knomit:integration -->
