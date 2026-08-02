---
name: knomit-recall
description: Use BEFORE brainstorming sessions, implementation requests, or any non-trivial work in an area — surfaces invariants, design decisions, and anti-patterns from prior knowledge so they inform the work from the start
---

# /knomit-recall <topic-or-text>

## When to use — trigger phrases

Fire BEFORE acting on any of these user signals:

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

DON'T fire for:

- Trivial edits in files you're actively iterating on
- Questions answerable from the current file alone (lint fixes, typos)
- After you've already recalled in this session for the SAME topic

## How

Call `mcp__knomit__knomit_query` with:

- `text`: the user-supplied topic (or your own one-line summary of the area)
- `entities`: any file paths currently open or about to be edited
- `applies_to`: the area path the work targets (e.g. `store/resolver`). Derive from an explicit user-supplied path, OR from the dominant directory among open files. Omit if uncertain; text/entities matching still works.

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

## Interpreting refs in returned facts

- `src://<repo-id>/<path>@<commit>:<blob>` — source code. `<repo-id>` is the first 12 hex of that repo's root commit; `<commit>` and `<blob>` are full 40-hex. Retrieve the exact bytes with `git cat-file blob <blob>` — this works even if the file was later renamed or deleted.
- `src://<name>/<path>[@<commit>]` — legacy source form, still accepted and still resolvable by hand. Not written for new facts.
- `https://…` / `http://…` — external URL.
- No scheme — local knomit fact path.

If `<source>` doesn't match your session, surface as "in repo `<source>`" rather than trying to open locally.
