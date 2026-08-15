---
name: knomit-recall
description: Use BEFORE brainstorming sessions, implementation requests, non-trivial work in an area, or committing to an explanation of why something fails — surfaces invariants, design decisions, and anti-patterns from prior knowledge so they inform the work from the start
---

# /knomit-recall <topic-or-text>

## When to use — trigger phrases

Recall is a HABIT, not a phase. Two recalls at the top of a session do not
cover the debugging you do an hour later. Most of the triggers below are user
signals; the last group is not, and it is the one that gets missed.

**Brainstorming / design exploration** — recall runs first so the brainstorm is informed by what already exists:

- About to invoke the `brainstorming` skill for creative work
- "let's brainstorm X", "how should we approach Y", "what's the best way to Z", "what are our options for W"
- "design X", "should we use A or B for Y", "what would it take to support Z"

**Implementation requests** — explicit and softer phrasings both count:

- "implement X", "add support for Y", "build a new Z", "create X"
- "make X work", "set up Y", "get Z working", "wire up W", "add a way to do X", "change behavior of Y"
- "redesign", "refactor", "rework"

**Diagnostic / explanatory:**

- "fix the bug in <area>" — when the area isn't one you've routinely touched this session
- "why does X work this way?" — existing-code rationale question
- About to pick where new code goes

**Your own conclusions — no user signal required.** These fire mid-task, when
nobody has asked you anything:

- About to state WHY something fails or misbehaves — "this test is flaky
  because…", "the 500 comes from…", "that number is wrong because…"
- About to act on a belief about a cause: writing the fix, reverting, changing
  the retry strategy, declaring a failure pre-existing
- About to brief a subagent on an area (see *Dispatching subagents* below)

Why this group is the one you skip: recall fires when you notice a gap.
Measuring your way to a confident answer removes the felt gap without removing
the ignorance. The moment you have empirically settled something is precisely
the moment you are least likely to ask whether it was already known — and
therefore the moment a documented answer is most likely to be sitting unread.
Reproducing a behaviour tells you THAT it happens; the corpus may already say
WHY, and may say your reproduction supports the wrong cause.

DON'T fire for:

- Trivial edits in files you're actively iterating on
- Questions answerable from the current file alone (lint fixes, typos)
- Re-running a test to see whether it passes. Concluding what the failure
  MEANS is a different act, and it does need recall.
- After you've already recalled in this session for the SAME question. Same
  *topic* is not enough: a design-time recall on an area does not cover a
  later "why is this failing?" in that area — different question, different
  facts.

## How

Call `mcp__knomit__knomit_query` with:

- `text`: the user-supplied topic (or your own one-line summary of the area)
- `entities`: any file paths currently open or about to be edited
- `applies_to`: the area path the work targets (e.g. `store/resolver`). Derive from an explicit user-supplied path, OR from the dominant directory among open files. Omit if uncertain; text/entities matching still works.
- `path`: an ontology-path prefix, when the request names a KIND of knowledge rather than only a subject. `applies_to` and `path` are DIFFERENT AXES and combine freely:
    - `applies_to` = **subject matter**, matched against each fact's `domain:` tags (`store`, `mcp`, `repos`, `build`).
    - `path` = **kind of knowledge**, the `kb/<topic>/` folder. Valid topics: `architecture`, `conventions`, `decisions`, `gotchas`, `incidents`, `invariants`, `meta`, `principles`. A bare topic name maps to `kb/<topic>/`.

  So "invariants about the lens write repo" is `text: "lens write repo"` + `path: "kb/invariants/"`, and "everything scoped to store" is `applies_to: ["store"]`. Confusing the two is the common error — `applies_to: ["invariants"]` half-works by accident, because a few facts carry `invariant` as a domain tag, and returns the wrong thing convincingly.

  **Use `path` whenever the question is about a kind.** Step 1 below tells you to read principles and invariants first; without `path` that is a hope about ranking, and with it the priority order is actually reachable.

**Empty result?** Note "no prior facts in this area — proceeding" and continue. Empty results are common in unfamiliar areas; not a blocker. When `applies_to` is set, missing matches mean no designer principle applies at this scope — proceed with text/entity results as today.

When the query returns facts, do BOTH steps below. Skipping step 2 means you're trusting facts that may be stale — corpus facts can lag HEAD.

### Step 1 — Read in priority order

1. **Principles first** (`kb/principles/`) — designer intent. Scoped principles are the *first* thing to read in an area; they trump tactical rules. Skip any fact whose `domain` contains `global` — those are already in SessionStart context.
2. **Invariants** (`kb/invariants/`) — load-bearing rules. Violating one breaks the system; if your design needs to, STOP and confirm with the user.
3. **Decisions** (`kb/decisions/`) — the *why* behind current shape.
4. **Conventions** — house style for the area.
5. **Scan all bodies for "anti-pattern:"** — cheapest design constraint you'll find.

### Step 2 — Verify the load-bearing claims

Pick the 3–5 facts whose specific claims (thresholds, ordering, struct shapes, file paths, function signatures) your work will depend on. For each:

- If the ref carries a blob (`@<commit>:<blob>`): compare it against HEAD with
  `git rev-parse HEAD:<path>`. Equal means the fact is still current; different
  means the file changed, and `git diff <blob> HEAD:<path>` shows exactly what.
  If `<path>` is gone at HEAD, `git log --find-object=<blob>` locates where it went.
- If the ref carries only a commit (the legacy form): `git show <commit>:<path>`,
  and note this fails outright if the file did not exist at that commit — one of
  the failure modes the blob form removes.
- If it has only external (`https://`) refs: sanity-check via the actual source file before relying.
- If it has no refs at all: lower your trust accordingly; prefer reading the relevant code directly.

## Dispatching subagents

A controller that stops recalling gives its subagents no reason to recall
either, so a whole fan-out can re-derive what one fact already records. Before
dispatching implementers, reviewers, or explorers into an area, do one of:

- Recall on their behalf and put the findings in the brief — preferred when the
  area has invariants they could violate without knowing it.
- Tell them explicitly to run `/knomit-recall <area>` before concluding
  anything.

Their reports are subject to the same rule: a subagent that hands back a cause
("the failure is a parallelism bug") has asserted a WHY, and that assertion is
worth checking against the corpus before you act on it.

## Interpreting refs in returned facts

- `src://<repo-id>/<path>@<commit>:<blob>` — source code. `<repo-id>` is the first 12 hex of that repo's root commit; `<commit>` and `<blob>` are full 40-hex. Retrieve the exact bytes with `git cat-file blob <blob>` — this works even if the file was later renamed or deleted.
- `src://<name>/<path>[@<commit>]` — legacy source form, still accepted and still resolvable by hand. Not written for new facts.
- `https://…` / `http://…` — external URL.
- No scheme — local knomit fact path.

For a legacy `src://<name>/…` ref whose `<name>` you cannot map to a checkout, surface it as "in repo `<name>`" rather than trying to open locally. For the current form, `<repo-id>` is a root commit: `git rev-list --max-parents=0 HEAD | cut -c1-12` in a candidate checkout tells you whether it is the right repo.
