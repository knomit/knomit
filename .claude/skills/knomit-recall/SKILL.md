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

- If it has a `src://<source>/<path>@<commit>` ref AND `<source>` matches this session: run `git show <commit>:<path>` and diff mentally against HEAD. If anything load-bearing has drifted, run `/knomit-update` or `/knomit-retract` BEFORE building on the fact.
- If it has only external (`https://`) refs: sanity-check via the actual source file before relying.
- If it has no refs at all: lower your trust accordingly; prefer reading the relevant code directly.

**Under a lens, first identify the fact's source repo.** A lens federates several repos; a returned fact's `file` path tells you which one it came from:

- A **`kb://<repo-id>/…`-qualified path** is a fact from a READ MOUNT, not the write repo. Call `knomit_repos` once per session to get the mount table — repo name ↔ 12-hex `<repo-id>` ↔ branch ↔ `src://` source slug. Map the fact's `<repo-id>` to its mount to learn which repo and which `src://<source>` its refs are anchored in, then verify the fact's `src://<source>/<path>@<commit>` refs against **that** repo's checkout — NOT the session's own `--source`. The matching `<source>` is the mount's slug, which may be any of the lens's read repos.
- A **bare (unqualified) path** is the lens's write repo — verify it exactly as in the single-repo bullets above.
- **A read-mount fact is read-only through the lens.** If verification shows a read-mount fact has drifted, you cannot `/knomit-update` or `/knomit-retract` it here — the write tools reject a `kb://<read-mount-id>/…` path. Connect to that repo's own endpoint (or a lens whose write repo is that repo) to correct it; otherwise just lower your trust for this session and note the drift.

## Interpreting refs in returned facts

- `src://<source>/<path>@<commit>` — source file in repo `<source>` at a specific commit. It is locally verifiable when `<source>` matches a repo you can check out: in a single-repo session that is your `--source` (read `.mcp.json`); **under a lens it is ANY mount's source slug** — map the fact's `kb://<repo-id>/…` path to its mount via `knomit_repos` to find which `<source>` applies. When it matches, the file may have drifted since `<commit>`; verify via `git show <commit>:<path>` against that repo's checkout.
- `src://<source>/<path>` — source file, no commit pin. Read the current file directly (in the matching repo).
- `kb://<repo-id>/<path>` — a fact in ANOTHER knomit repo mounted in this lens (a cross-repo pointer). Resolve `<repo-id>` to its repo via the `knomit_repos` mount table; `knomit_explain` with the qualified path verbatim reads it.
- `https://…` / `http://…` — external URL.
- No scheme — a local knomit fact path in the current repo/lens.

If a `src://<source>` slug matches no mount in this session, surface it as "in repo `<source>`" rather than trying to open locally. In a lens, "this session" means any of the mounts `knomit_repos` lists — check the mount table before concluding a source is unreachable.
