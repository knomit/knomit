# Knomit as Claude Code's Long-Term Memory for Source Code — Design

**Date:** 2026-05-20
**Status:** Design, awaiting plan
**Shape:** B (balanced — hooks for the obvious, skills for the subtle, no PreToolUse auto-injection)

## Goal

Turn knomit into the persistent memory layer for Claude Code (CC) when working
on source code. Across sessions and across machines, CC should:

- Enter every session pre-warmed on load-bearing facts about the codebase
- Recall relevant prior knowledge before non-trivial work
- Capture discoveries, decisions, and gotchas as it learns
- Verify provenance before relying on stored beliefs
- Be backed by a safety net of capture-side hooks that catch what CC forgets to write

The integration is **generic**: any project can opt in by dropping config files
and running an `init` flow. Knomit itself is the first user, but the design is
codebase-agnostic.

## Pain points addressed

All four categories of knowledge are in scope:

1. **Invariants & gotchas** — load-bearing rules CC must never violate (the
   "I keep forgetting" class, e.g. knomit's historical-graph invariant)
2. **Architecture & structure** — what lives where, what depends on what
3. **Conventions & idioms** — how the project does things (testing, logging,
   error handling, naming)
4. **Decisions & rationale** — design choices with their context

## Approach

Shape B: hybrid trigger model.

- **Hooks** automate the moments where forgetting would be costly: session
  start (read invariants), commits (capture summary), pre-compaction /
  per-turn / session-end (capture safety net for discoveries CC didn't write
  down).
- **Skills** (slash commands) handle deliberate moments: recalling before
  non-trivial work, remembering after a discovery, capturing a decision,
  verifying provenance, structured kickoff.
- **CLAUDE.md** carries the philosophy and when-to-use guidance.

Bootstrap: structured per-area kickoff (high-quality seed), then organic
accumulation thereafter.

---

## 1. Foundation

### 1.1 Topology — per-project `.mcp.json`, one bridge per repo

`knomit-bridge` is dual-purpose:

- **Primary role:** stdio MCP proxy spawned by CC at session start (existing
  behavior in `tools/bridge/main.go`).
- **Secondary role:** one-shot scaffolding tool, invoked as
  `knomit-bridge init` to drop the integration files into a codebase
  (see §4.2).

Each codebase that opts into knomit-as-memory carries its own `.mcp.json` at
the repo root declaring `knomit-bridge` as an MCP server, scoped to that repo:

```json
{
  "mcpServers": {
    "knomit": {
      "command": "knomit-bridge",
      "args": ["--repo", "<repo-name>", "--profile", "code"]
    }
  }
}
```

CC reads `.mcp.json` at session start, spawns the bridge as a subprocess, and
sees `mcp__knomit__*` tools throughout the session. When CC exits, the bridge
exits with it. Stateless and scoped.

**Why this works:**

- Aligns with CC's documented model (one `.mcp.json` per project root)
- Aligns with knomit-bridge's stateless-by-design contract
- Multi-codebase = multiple parallel CC sessions, one per repo (the
  documented and conventional pattern)

**Mode 2 (one session, multiple repos) is not designed for.** A user who
needs it can configure a parent-folder workspace with multiple bridges in
its `.mcp.json` and a `knomit-repos.json` map; the hooks below already
support this as an opt-in (they fall back to a cwd-prefix map if present,
otherwise call `mcp__knomit__*` directly).

### 1.2 Ontology — embedded `ontology_code.yaml`

Add a second embedded ontology to `internal/fact/` alongside the existing
`ontology_default.yaml` (Wikipedia-style). The new file becomes the canonical
preset for source-code repos.

**File:** `internal/fact/ontology_code.yaml`

```yaml
id: source-code
name: Source Code Knowledge
description: Knowledge categories for AI agents working in a codebase.
topics:
  invariants:
    description: Load-bearing rules that must never be violated
    children:
      architecture: { description: Layering, dependency, ownership constraints }
      data:         { description: Invariants on data models, schemas, indexes }
      protocol:     { description: API contracts, wire-format guarantees }
      concurrency:  { description: Thread, lock, ordering rules }
  architecture:
    description: Structural facts — what lives where, how it connects
    children:
      modules:      { description: Package/module purpose and boundaries }
      flows:        { description: Request/data flow paths }
      integrations: { description: External services, libraries, MCP servers }
  conventions:
    description: How we do things here — idioms, patterns, style
    children:
      testing:  { description: Mocks, fixtures, structure }
      logging:  { description: Library, levels, structured fields }
      errors:   { description: Wrapping, surfacing, fallbacks }
      naming:   { description: File, type, function naming patterns }
      git:      { description: Commit messages, branch model, PR norms }
  decisions:
    description: Design choices with rationale
    children:
      accepted:   { description: Decisions in force }
      superseded: { description: Previously accepted, now replaced }
      rejected:   { description: Alternatives considered and rejected }
  gotchas:
    description: Sharp edges and surprises
    children:
      tools:     { description: Build, test, dev-tool gotchas }
      libraries: { description: Third-party library quirks }
      runtime:   { description: Language/runtime traps }
  incidents:
    description: Past bugs and their fixes (regression-test anchored)
    children:
      bugs:        { description: Bugs that landed }
      near-misses: { description: Caught before merging }
  meta:
    description: Knowledge about the knowledge
    children:
      reasoning: { description: Methodology facts from knomit_review/hypothesize }
      ontology:  { description: Notes about how this ontology is used }
```

**Selection at init time:**

```
knomit init --name <repo> --ontology-preset code
```

A new `--ontology-preset <name>` flag is added to `cmd/init.go` alongside the
existing `--ontology <path>`. The preset name selects from the embedded
ontologies (`default`, `code`); paths still work as today.

`meta/reasoning` is preserved from the default ontology because
`knomit_review` and `knomit_hypothesize` write methodology facts there.

**Fact paths (validated by `Ontology.ValidatePath`):**

- `invariants/architecture/historical-graph`
- `architecture/modules/store-resolver`
- `conventions/testing/use-mockgen`
- `decisions/accepted/2026-04-introduce-vtable`
- `gotchas/tools/sqlite-vtable-stub-removed`
- `incidents/bugs/2026-03-resolver-walks-past-root`
- `meta/reasoning/<knomit-generated>`

All path segments are kebab-case-or-numeric per `validKeyRe`. Below the
defined children, the path is freeform.

### 1.3 Fact field conventions

Every fact captured via the skills/hooks below carries:

| Field | Convention |
|---|---|
| `title`, `body` | Human-readable; body should be self-contained — readable without the title |
| `kind` | One of `invariant`, `architecture`, `convention`, `decision`, `gotcha`, `incident`, `methodology`. Drives SessionStart preamble formatting |
| `domain` | Free-form tags: `go`, `sqlite`, `mcp`, `frontend`, `testing`, etc. |
| `entities` | File paths (relative to repo root), symbol names, type names. Used by `/recall` for matching |
| `refs` | `src://<source>/<path>[@<commit>]` source-file lineage, PR URLs (`https://…`), linked local fact paths. Used by `/why` |
| `confidence` | 0.95 for `/kickoff-area`, 0.85 for deliberate `/remember`, 0.6 for hook-driven auto-capture |

### 1.4 Ref scheme conventions

Refs in `knomit_learn` calls follow these conventions:

| Form | Meaning | Example |
|---|---|---|
| `src://<source>/<path>[@<commit>]` | Source file in registered source repo `<source>`, optionally git-anchored | `src://knomit/internal/store/service.go@cfef409` |
| `https://`, `http://` | External URL | `https://github.com/knomit/knomit/pull/65` |
| `file:///abs/path` | Filesystem reference outside any source repo | `file:///tmp/scratch.txt` |
| *no scheme* | Local fact path within this KB | `invariants/architecture/historical-graph` |

The `<source>` slug is supplied per-session by the bridge's `--source`
flag (required). It identifies WHICH source-code repo the file lives in,
so a single knomit KB can hold facts about multiple source repos
unambiguously. Knomit stores the ref opaquely — no source registry, no
validation.

---

## 2. Mechanics

### 2.1 Skills (six slash commands)

All commands use the `knomit-` prefix to avoid clashing with built-in CC skills.
Skills live under `.claude/skills/<name>/SKILL.md` (CC's required per-skill-directory layout).

| Command | Wraps | Purpose |
| --- | --- | --- |
| `/knomit-recall <topic-or-text>` | `knomit_query` | Top facts for the query, grouped by `kind` (invariants first). Builds the query from free text + entity hints (currently-open files) + optional path prefix |
| `/knomit-remember` | `knomit_query` + `knomit_learn` | Capture a discovery. Runs similarity query first; surfaces contradictions/near-duplicates so CC can update / retract / merge instead of dup-writing. Otherwise writes fact (confidence 0.85) |
| `/knomit-why <fact-path>` | `knomit_explain` | Walks refs graph; shows source-file lineage at the anchor commit + linked facts. Used to verify before relying on a fact |
| `/knomit-decided <slug>` | `knomit_learn` | Summarizes the current conversation's decision (options, rationale, choice). Writes under `decisions/accepted/<YYYY-MM>-<slug>` |
| `/knomit-kickoff-area <area>` | `knomit_query` + `knomit_learn` | Structured per-area seed. CC reads the area, drafts foundational facts in each kind, user reviews, CC writes at confidence 0.95 |
| `/knomit-review` | `knomit_review` | Async maintenance pass: cluster facts, suggest synthesis, flag contradictions, surface stale facts (refs to deleted files) |

### 2.2 Hooks (capture safety net is the core value)

Four hooks, all on by default. Additional hook ideas (`UserPromptSubmit`
correction detection, `Bash` failure surprise capture, `TodoWrite`
completion nudges) are listed in §5 as opt-in extensions.

All hooks except `SessionStart` use CC's `hookSpecificOutput.additionalContext`
JSON protocol to inject context (output `{"hookSpecificOutput":{"additionalContext":"…"}}`).
`SessionStart` auto-wraps plain stdout as a system reminder, so it emits plain text.

SessionEnd was considered but dropped: CC's SessionEnd hook is fire-and-forget
(exit code ignored, output discarded), so context injection isn't possible from
that event. Its intended "session digest" role is partially covered by the
rate-limited Stop hook.

**A. `SessionStart`** — `.claude/hooks/knomit-session-start.sh`

Fires once at session start. Steps:

1. Detect `cwd` → resolve MCP server name. Default: `knomit`. Falls back to a
   `knomit-repos.json` cwd-prefix map if present (Pattern X / Mode 2 support).
2. Call `knomit_query` for all facts under `invariants/*` (no result cap —
   invariants are load-bearing).
3. Call `knomit_query` for the top 5 most-recently-updated facts in this repo
   (any kind, last 14 days).
4. Emit plain text (no wrapper tags); CC wraps it as a system reminder automatically.

CC sees the preamble before reading the user's first prompt.

**B. `PostToolUse` on `Bash` for `git commit`** — `.claude/hooks/knomit-post-commit.sh`

Fires after CC commits. Steps:

1. Read commit stdout from `tool_output.stdout` (exit code from `tool_output.exit_code`).
2. Decide if the commit is substantive (heuristic): stdout > 60 chars, OR
   contains markers (`fix:`, `refactor:`, `decided:`, `invariant:`,
   `gotcha:`).
3. If substantive, output `hookSpecificOutput.additionalContext` JSON nudging
   to run `/knomit-remember` or `/knomit-decided`.

Safety net for things CC discovered mid-flow but forgot to capture.

**C. `PreCompact`** — `.claude/hooks/knomit-pre-compact.sh`

Fires before CC compacts conversation context. Steps:

1. Scan the recent transcript window (the part about to be compressed).
2. POST to the knomit `/detect` endpoint for capture-worthy signals
   (see §2.3).
3. If any blocks score above threshold, output `hookSpecificOutput.additionalContext`
   nudging to run `/knomit-remember` or `/knomit-decided` before compaction.

This is the most important capture-side hook — last chance before
discoveries get summarized into oblivion.

**D. `Stop`** — `.claude/hooks/knomit-stop.sh`

Fires at the end of every assistant turn. Steps:

1. Examine the just-completed turn (transcript + tool calls).
2. POST the turn's blocks to `/detect` (same endpoint as PreCompact;
   see §2.3).
3. If any block scores above threshold, output `hookSpecificOutput.additionalContext`
   nudging to run `/knomit-remember` before moving on.

Rate-limited: at most one Stop-nudge per N turns (configurable; default 5)
to keep noise down. Stop fires every turn so this rate limit is the primary
de-noising mechanism. Whether the YAML thresholds should also be tighter
for Stop than PreCompact is an open question (§7 q5).

### 2.3 Capture-worthy signal detection (via knomit `/detect`)

The PreCompact / Stop hooks do not pattern-match in shell.
They POST a window of recent transcript blocks to a new knomit HTTP
endpoint that scores each block for capture-worthiness using the existing
embedder + search index. All canonical phrases and thresholds live in a
YAML file — no patterns hardcoded in Go source.

#### Endpoint

```text
POST /api/v1/profiles/{profile}/detect
```

For source-code repos, `{profile}` is always `code`. The endpoint is
profile-scoped because what counts as a capture-worthy signal in a
source-code conversation is different from a chat or generic-knowledge
conversation; promoting `profile` to a path segment also gives a clean
home for future profile-specific endpoints. Profiles that don't have a
configured intents file return 404.

**Request:**

```json
{
  "blocks": [
    { "role": "user",      "text": "no actually I think we should use vtab here" },
    { "role": "assistant", "text": "Ah — the resolver was walking past root" }
  ],
  "intents": ["correction", "discovery", "decision", "fix-bug", "gotcha"],
  "novelty_context": {
    "repo": "knomit",
    "branch": "machine/host"
  }
}
```

`novelty_context` is optional. Hooks always pass it (they know their
repo/branch from environment); other callers can omit it to get
intent-scoring only.

**Response:**

```json
{
  "blocks": [
    {
      "index": 0,
      "signals":      [{ "intent": "correction", "score": 0.87 }],
      "novelty":      0.91,
      "similar_facts": []
    },
    {
      "index": 1,
      "signals":      [{ "intent": "discovery",  "score": 0.78 }],
      "novelty":      0.74,
      "similar_facts": [{ "path": "architecture/store/resolver", "similarity": 0.31 }]
    }
  ]
}
```

Two per-block signals:

- **Intent score** — max cosine similarity between the block's embedding
  and any canonical phrase for that intent. Robust to phrasing variation
  in a way regex isn't. The response carries a score for *every* requested
  intent (the example above trims to the top one for readability); the
  caller applies thresholds.
- **Novelty** — `1 - max_similarity_to_any_existing_fact` in the
  novelty-context repo. High novelty + any plausible intent = strong
  capture signal. Low novelty = the moment is already known; the
  `similar_facts` list lets CC update an existing fact instead of writing
  a duplicate.

Intents are conversation **signals**, not fact **kinds**. A `correction`
intent may lead to a `convention` or `invariant` fact; the intent is what
tripped the detector, not what gets written.

#### Intents and thresholds — `internal/detect/intents_code.yaml`

Canonical phrases and thresholds live in a YAML file embedded via
`//go:embed`, parsed and embedded into vector space at service startup,
cached in memory. Override via a `--intents-code <path>` flag on the
knomit service (same shape as the existing `--ontology <path>` override).

```yaml
intents:
  correction:
    description: User pushing back on a prior assumption
    canonical_phrases:
      - "no actually, that's not right"
      - "you're missing the point"
      - "let me correct you"
      - "wait, that's wrong"
      - "I think you misunderstood"
  discovery:
    description: CC realizing something not previously understood
    canonical_phrases:
      - "ah, I see — the resolver actually walks the parent chain"
      - "turns out the vtab handles this differently"
      - "I was wrong about how this works"
  decision:
    description: A design tradeoff resolved in conversation
    canonical_phrases:
      - "let's go with the vtab approach"
      - "do it the second way"
      - "yes that's the right call"
  fix-bug:
    description: Root cause identified and fix applied
    canonical_phrases:
      - "the root cause was the missing vtab registration"
      - "this fixes the panic in the resolver"
  gotcha:
    description: Unexpected behavior worth recording
    canonical_phrases:
      - "this only works if X"
      - "be careful, this silently fails when Y"

thresholds:
  intent_score: 0.7
  novelty_score: 0.8
  combined_low_intent: 0.5
```

Adding a new profile later is just dropping `intents_<profile>.yaml` next
to this one and embedding it; the handler resolves the file from the
`{profile}` path segment.

#### How hooks consume the response

A small shell helper inside each capture hook:

1. Gather the relevant transcript window (last N turns for Stop; the
   to-be-compacted window for PreCompact; the full session digest for
   SessionEnd).
2. POST to `/api/v1/profiles/code/detect` with blocks + novelty context.
3. For each block where `max(intent_score) > thresholds.intent_score` OR
   (`novelty > thresholds.novelty_score` AND any `intent_score >
   thresholds.combined_low_intent`), include it in the nudge.
4. Emit a `<system-reminder>` listing the capture-worthy moments plus any
   `similar_facts` so CC knows what's already known and can update vs
   create.

**Nudge vs auto-write.** The hooks still nudge CC rather than writing
facts directly. CC has the conversation context to compose a coherent fact
body; the detect endpoint just tells the hook *which* moments to surface.
Auto-write via subagent (Shape D+) is listed in §5.

**Why HTTP, not MCP.** The detect endpoint is server-side machinery the
hooks need; it doesn't belong in CC's tool surface. Exposing
`knomit_detect` as an MCP tool is listed in §5 as an opt-in extension for
the case where CC itself wants to score arbitrary text.

### 2.4 CLAUDE.md template block

Users drop this block into their project's `CLAUDE.md`:

```markdown
## Working with knomit memory

This project uses knomit as long-term memory. Six `/knomit-…` slash commands
wrap knomit's MCP tools. Use them in these moments:

**Before non-trivial work** — call `/knomit-recall <area>` before:
- Editing or writing files under [list known-invariant paths]
- Picking where new code goes
- Implementing a pattern that may already exist
- Answering "why does X work this way?"

**After a discovery** — call `/knomit-remember` when:
- You found something non-obvious during exploration
- The user corrected you on a project fact
- A bug fix exposed a hidden invariant

**After a design choice** — call `/knomit-decided <slug>` when you and the
user resolved a tradeoff in conversation.

**When you doubt a fact** — call `/knomit-why <fact-path>` to walk the
provenance graph and verify against current code before relying on it.

**Philosophy** — knomit is your colleague's tribal knowledge. Invariants are
load-bearing; re-read before touching the area they cover. Facts can be
stale; `/knomit-why` is cheap. When uncertain, `/knomit-recall` — also cheap.
```

The `[list known-invariant paths]` placeholder is filled per-project. For
knomit itself, it would list `internal/store/`, `internal/mcp/`, and any
area touching refs/edges/resolver.

---

## 3. Maintenance

### 3.1 On-demand review via `/knomit-review`

CC invokes `/knomit-review` (wrapping `knomit_review`'s async MCP task). knomit
walks the KB to:

- Cluster related facts and suggest synthesis (distill near-duplicates into
  one sharper fact)
- Flag contradictions across facts
- Surface retraction candidates (facts whose source files no longer exist,
  or whose claims contradict current code)

CC presents suggestions to the user; user approves; CC writes the
synthesis / retractions.

### 3.2 Contradiction detection at write time

Already part of `/knomit-remember`: a similarity query precedes any write. If a
contradicting fact exists, the skill surfaces the conflict so the user and
CC decide update / retract / merge. Catches inconsistencies as they're
introduced.

### 3.3 Staleness via refs

Each fact's `refs` carry `file@commit` lineage. `/review` checks whether
each referenced file still exists at the current `HEAD`. Missing references
are strong staleness signals. `/why` shows the same check on individual
lookups.

### 3.4 Optional: scheduled review

Layer on later via CC's `/schedule` command if drift becomes noticeable.
Not part of the initial build.

---

## 4. Setup

### 4.1 Per-machine (once)

1. Install knomit and `knomit-bridge`; ensure `knomit-bridge` is on `PATH`.
2. Start the knomit service.

No global CC config required. Pattern A is per-project.

### 4.2 Per-codebase (once)

Two commands, in order:

```bash
# (a) Create the knomit-side repo (existing knomit CLI)
knomit init --name <repo> --ontology-preset code

# (b) Drop CC-side integration files into the project
cd <repo>
knomit-bridge init [--repo <name>]
```

What `knomit-bridge init` does — strictly CC-side scaffolding, nothing
touching the knomit repo:

1. **CC-side scaffolding** (files embedded in the bridge binary via
   `//go:embed`). Layout dropped into the current directory:

   ```
   <repo>/
   ├── .mcp.json                          ← declares knomit-bridge
   ├── .claude/
   │   ├── settings.json                  ← hooks registered here
   │   ├── hooks/
   │   │   ├── _knomit-helpers.sh
   │   │   ├── knomit-session-start.sh
   │   │   ├── knomit-post-commit.sh
   │   │   ├── knomit-pre-compact.sh
   │   │   └── knomit-stop.sh
   │   └── skills/                        ← per-skill-directory (CC requirement)
   │       ├── knomit-recall/SKILL.md
   │       ├── knomit-remember/SKILL.md
   │       ├── knomit-why/SKILL.md
   │       ├── knomit-decided/SKILL.md
   │       ├── knomit-kickoff-area/SKILL.md
   │       └── knomit-review/SKILL.md
   └── CLAUDE.md                          ← contains the integration block
   ```

2. **Conflict handling.** Per-file semantics:

   - **Owned by integration** (`.claude/hooks/*`, `.claude/skills/**`):
     always written/overwritten. Re-running `knomit-bridge init` after
     deleting any of these restores them.
   - **Merge-required** (`.mcp.json`, `.claude/settings.json`, `CLAUDE.md`):
     if the destination exists, a companion is dropped instead.

   | Existing file | Companion dropped | What user does |
   | --- | --- | --- |
   | `.mcp.json` | `.mcp.json.knomit` | Merge `mcpServers.knomit` into existing |
   | `.claude/settings.json` | `.claude/settings.json.knomit` | Merge `hooks` entries into existing |
   | `CLAUDE.md` | `CLAUDE.md.knomit-block` | Append the block to existing |

3. **Summary output.** The bridge prints what was created vs what needs
   manual merging:

   ```text
   ✓ Created .mcp.json, .claude/settings.json, .claude/hooks/*, .claude/skills/*
   ⚠ CLAUDE.md exists — merge content from CLAUDE.md.knomit-block manually
   ```

All scaffolded files are intended to be checked into git so contributors
inherit the integration.

### 4.3 Seeding & ongoing use

After scaffolding:

1. **Seed the KB via kickoff** — one `/kickoff-area <area>` per major
   subsystem. Each session produces ~5–15 foundational facts at confidence
   0.95.

2. **Organic from then on** — facts accumulate via `/knomit-remember`, `/knomit-decided`,
   the post-commit hook, and the capture-safety-net hooks.

---

## 5. Opt-in extensions (Shape C+, layered on later)

None of these are part of the initial build. Listed here so the design's
shape doesn't preclude them.

| Extension | What it adds | Add when |
|---|---|---|
| **PreToolUse fact injection** | Hook fires before Edit/Write; queries facts referencing the target file; injects matches as a reminder | Shape B feels too quiet — CC misses facts you know exist |
| **`UserPromptSubmit` correction detection** | Score each incoming user prompt against the `correction` intent via `/detect`; nudge if above threshold | Per-turn correction detection has high value (vs only catching it later at PreCompact) |
| **`knomit_detect` MCP tool** | Expose `/detect` through the `code` MCP profile so CC can score arbitrary text itself | CC wants to self-assess "should I `/remember` this?" without a hook firing |
| **`Bash` failure surprise capture** | `PostToolUse` on non-zero exit; nudge to capture as a gotcha | Surprising-failure pattern is happening enough to be worth a hook |
| **`TodoWrite` completion nudges** | When CC marks a substantive todo complete, nudge for capture | Todo milestones consistently mark capture-worthy work |
| **`/scope <feature>` + auto decision archives** | Conversation tagging; on session end, CC writes a decision-trail fact with refs to the transcript | You want decisions auto-captured without `/decided` |
| **`/hypothesize`** | Wraps `knomit_hypothesize`; generates design hypotheses from synthesis facts | You want knomit pulling its weight as a thinking partner |
| **Bug-fix lineage** | Fixing a bug auto-prompts `/remember kind=incident` with refs to failing test + fix commit | You want a regression-incident graph |
| **Onboarding mode** | `/explain-this-codebase <area>` walks knomit and produces a curated narrative for new contributors | New people / agents joining frequently |
| **Cross-machine sync surface** | SessionStart preamble adds "what others learned since you last saw this" | You work across multiple machines |
| **Auto-write via subagent** | Hooks dispatch a subagent that composes and writes facts directly (vs nudging CC) | Nudges are missing too many real captures |
| **Hook config switchboard** | A single `.claude/knomit-hooks.json` toggles each hook (including the on-by-default ones); each script exits early if its flag is off | More than two opt-in hooks accumulate, or users want to disable a default hook without editing `settings.json` |

---

## 6. Out of scope

- **Embedding model tuning.** The `/detect` endpoint (§2.3) uses the
  existing embedder for similarity scoring; we don't fine-tune or swap
  the embedding model itself.
- **Auto-merging across machines.** Already handled by knomit's
  machine-branch + consensus model; the integration reads what's there.
- **A separate UI.** The existing knomit web UI in `internal/web/` covers
  human browsing.

---

## 7. Open questions to resolve during implementation

1. **CC hook event names.** ~~CLOSED~~ Verified: `SessionStart`, `PostToolUse`,
   `PreCompact`, `Stop` are live events and support context injection via
   `hookSpecificOutput.additionalContext`. `SessionEnd` exists but is
   fire-and-forget (exit code ignored, output discarded) — dropped from
   this integration (see §2.2). `UserPromptSubmit` is live for opt-in use.

2. **Tool namespacing under Pattern X.** Confirm whether CC namespaces
   tools as `mcp__<server>__<tool>` consistently. If yes, hooks under
   Pattern X target by full namespaced name. If CC has a different
   namespacing scheme, adjust the hook's lookup logic.

3. **CC-side scaffolding lives in `knomit-bridge init`.** Resolved
   (§4.2): the bridge owns CC-side scaffolding only via embedded
   templates. Knomit-side repo creation stays in `knomit init`. Open
   sub-question: exact form of the integration marker in CLAUDE.md
   (`<!-- knomit:integration -->` proposed) and in `.mcp.json`
   (presence of `mcpServers.knomit` key is sufficient).

4. **`--ontology-preset` flag implementation.** New flag on
   `cmd/init.go`. Selects from a small registry of embedded ontologies
   (`default`, `code`). Existing `--ontology <path>` keeps working for
   custom ontologies. Both flags should be mutually exclusive.

5. **Intent canonical phrases and thresholds (§2.3).** The initial set
   of intents (`correction`, `discovery`, `decision`, `fix-bug`,
   `gotcha`) and the canonical phrases per intent in
   `internal/detect/intents_code.yaml` are starting points. They will
   need tuning based on real session noise. Plan: ship conservative
   thresholds, allow override via `--intents-code <path>`, iterate.
   Also TBD: whether thresholds should be per-hook (PreCompact wants
   broader recall; Stop wants higher precision to reduce noise) or
   uniform with per-hook rate limits doing the de-noising.

6. **`/detect` for missing profile.** A `POST /api/v1/profiles/{profile}/detect`
   where no `intents_<profile>.yaml` exists should return 404 (honest:
   profile has no detect support) rather than an empty response (which
   could be misread as "no signals found"). Confirm during implementation.

7. **Hook → knomit URL discovery.** Hooks need to know the knomit HTTP
   base URL to POST to `/detect`. Options: read the same lockfile
   `knomit-bridge` reads (`readLockfileBaseURL` in `tools/bridge/main.go`),
   or a `KNOMIT_BASE_URL` env var set by the scaffolded settings.json, or
   a `.claude/knomit-env` file. Resolve during implementation; the
   lockfile option is most consistent with the bridge.

8. **Where do skills live.** ~~CLOSED~~ Project-scoped skills in `.claude/skills/<name>/SKILL.md`
   (per-skill-directory layout required by CC). Prefix `knomit-` on all skill
   names to avoid clashing with built-in CC skills (`review`, etc.).

9. **Confidence calibration.** The default values (kickoff 0.95, deliberate
   0.85, hook-driven 0.6) are guesses. Review after some real usage and
   adjust.

10. **knomit-repos.json schema for Pattern X.** Spec the format precisely
    (longest-prefix match, fallback behavior, where the file lives). Defer
    until Mode 2 is actually needed.
