# Fact Synthesis Design

## Goal

Automated knowledge base maintenance — prune stale and duplicate facts, distill many into fewer higher-order facts, and derive new insights by cross-referencing facts across domains and entities.

## Architecture

Synthesis is a CLI subcommand (`knomit synthesize`) that loads recipe files from the knomit repo, queries facts, sends them to an LLM, and executes the LLM's decisions using existing git primitives (commit, tag, delete). All changes happen on a dedicated branch. The user controls whether changes merge automatically or require review.

## Recipe Model

Recipes live in the knomit repo at `.knomit/synthesize/<name>.yml`. Version-controlled, synced across machines, self-contained alongside the knowledge base.

```
<knomit-repo>/
├── worlds/
│   └── ...
└── .knomit/
    ├── synthesize/
    │   ├── cve-review.yml
    │   └── security-insights.yml
    ├── merge/          ← future job types
    └── ...
```

### Recipe format

```yaml
name: security-review
prompt: "Focus on vulnerability lifecycle and vendor patterns"
scope:
  domain: [security]
  entities: []
  search: []
  path: ""
auto_merge: false

steps:
  - mode: prune
    model: gemini-2.0-flash
    prompt: "Identify CVEs that have been patched, facts that are outdated, redundant, or duplicates that should be unified"
  - mode: distill
    model: claude-sonnet-4-6
    prompt: "What patterns emerge? Which vendors have recurring issues? What are the trends?"
```

### Fields

| Field | Description |
|-------|-------------|
| `name` | Recipe identifier, used for branch name and tags |
| `prompt` | Recipe-level context shared with all steps |
| `scope` | Fact selection filters (all optional, combined with AND) |
| `scope.domain` | Match facts with any of these domain tags |
| `scope.entities` | Match facts with any of these entity tags |
| `scope.search` | Text/embedding search queries for additional fact discovery |
| `scope.path` | Ontology path prefix (optional narrowing filter) |
| `auto_merge` | `true`: merge into current branch, delete synthesis branch. `false`: push branch for review |
| `steps` | Ordered pipeline of operations |
| `steps[].mode` | `prune` or `distill` |
| `steps[].model` | LLM model override for this step (falls back to env default) |
| `steps[].prompt` | Step-specific instructions (combined with recipe-level prompt) |

### Scope philosophy

The ontology (directory tree) is for organizing facts, not for bounding synthesis. Synthesis thinks across the entire knowledge base, filtered by domains and entities. The `path` filter is an optional narrowing mechanism, not the primary scoping lens.

### Auto-discovery mode

When a recipe omits the `scope` field entirely, synthesis auto-discovers what needs processing by detecting facts that changed since the last run. This is the default mode for recurring/scheduled synthesis.

**How it works:**

1. Look up the last synthesis commit for this recipe in the `synthesis_log` table
2. If no previous run exists, process all facts (full initial run)
3. Otherwise, `git diff --name-only <last_commit>..HEAD -- worlds/` to find added/modified/deleted fact files
4. Use the changed facts as the input set for the pipeline steps

This means a recipe like:

```yaml
name: daily-review
prompt: "Review recent changes for patterns and staleness"
auto_merge: true
steps:
  - mode: prune
    model: gemini-2.0-flash
  - mode: distill
    model: claude-sonnet-4-6
```

...automatically targets only facts that changed since the last `daily-review` run. No explicit scope needed.

**Explicit scope still works.** If `scope` is provided, it's used as-is regardless of what changed. Auto-discovery and explicit scope are mutually exclusive — the recipe author chooses one or the other.

## Synthesis Log

A SQLite table in the cache DB tracking when each recipe last ran:

```sql
CREATE TABLE IF NOT EXISTS synthesis_log (
  recipe TEXT NOT NULL,
  last_commit TEXT NOT NULL,
  run_at TEXT NOT NULL,
  facts_processed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (recipe)
);
```

| Column | Description |
|--------|-------------|
| `recipe` | Recipe name (matches `.knomit/synthesize/<name>.yml`) |
| `last_commit` | Git commit SHA at the time of the run |
| `run_at` | ISO 8601 timestamp |
| `facts_processed` | Number of facts sent to the LLM |

Updated after each successful synthesis run. Used by auto-discovery mode to determine the delta.

## CLI

```bash
knomit synthesize                     # default: prune+distill on changes since last run
knomit synthesize --recipe <name>     # run one recipe
knomit synthesize --all               # run all recipes in .knomit/synthesize/
```

**Default mode (no flags):** Runs a built-in two-step pipeline (prune → distill) using auto-discovery — only facts that changed since the last `knomit synthesize` run. Uses the default LLM model from env. Auto-merges results. This is the "just run it" mode for cron/CI.

Resolves `<name>` to `<repo>/.knomit/synthesize/<name>.yml`.

## Modes

### Prune

Review facts for staleness, redundancy, obsolescence, and duplication. Prune handles both removing stale facts and merging duplicates — facts that say the same thing are unified into a single fact with `refs` pointing to the originals.

LLM response:

```json
{
  "decisions": [
    { "file": "worlds/security/cve-2024-1234.md", "action": "forget", "reason": "Patched in vendor advisory VA-2024-5678" },
    { "file": "worlds/security/cve-2024-5555.md", "action": "update", "confidence": 0.3, "reason": "Likely fixed but no confirmation" },
    { "file": "worlds/security/cve-2024-9999.md", "action": "keep", "reason": "Still active, no patch available" }
  ],
  "merges": [
    {
      "sources": ["worlds/people/alice/dark-mode-1.md", "worlds/people/alice/dark-mode-2.md"],
      "merged": {
        "path": "worlds/people/alice/prefers-dark-mode",
        "title": "Alice prefers dark mode",
        "body": "Consistently stated across multiple sessions...",
        "domain": ["preferences"],
        "confidence": 0.95,
        "entities": ["alice"],
        "refs": ["worlds/people/alice/dark-mode-1.md", "worlds/people/alice/dark-mode-2.md"]
      }
    }
  ],
  "summary": "Pruned 12 resolved CVEs, merged 6 redundant facts into 3"
}
```

### Distill

Find patterns across many facts and produce higher-order insights. Optionally forget originals that are fully subsumed.

LLM response:

```json
{
  "synthesize": [
    {
      "path": "worlds/vendors/acme/security-posture",
      "title": "Acme Corp has recurring authentication vulnerabilities",
      "body": "Across 15 CVEs from 2023-2025, Acme Corp shows a pattern of...",
      "domain": ["security", "vendors"],
      "confidence": 0.8,
      "entities": ["acme-corp"],
      "refs": ["worlds/security/cve-2024-1234.md", "worlds/security/cve-2024-5678.md"]
    }
  ],
  "forget": ["worlds/security/acme-cve-summary-old.md"],
  "summary": "Synthesized 15 Acme CVEs into 2 pattern facts, removed 1 outdated summary"
}
```

## Pipeline Execution

### 1. Create branch

Create `synthesize/{recipe-name}` from current branch.

### 2. For each step

**a. Query facts** — Re-query from git using scope filters via SearchIndex. Each step sees the current state of the repo, including changes from previous steps. Git is the source of truth.

**b. Chunk for context limits** — If the fact set exceeds the LLM's context budget, split into size-based batches.

**c. Call LLM per chunk** — Send facts + recipe prompt + step prompt. The step's `model` override selects the LLM (falls back to env default).

**d. Cross-chunk pass** — If multiple chunks produced results, do a final LLM call with the chunk outputs to find cross-cutting patterns.

**e. Execute plan** — Apply the LLM's decisions using core fact operations:
- **learn**: Create new facts (merged duplicates, distilled insights). Commits as `learn: <title>`, tags as `learn/synthesize-{recipe}`.
- **forget**: Delete pruned/subsumed/merged-source facts. Commits as `forget(synthesize-{recipe}): <file>`, tags as `forget/synthesize-{recipe}`.
- **update**: Adjust confidence/metadata. Commits as `update: <title>`, tags as `learn/synthesize-{recipe}`.

### 3. Finalize

- **`auto_merge: true`** — Merge `synthesize/{recipe}` into current branch, delete synthesis branch.
- **`auto_merge: false`** — Push `synthesize/{recipe}` to remote. Print a summary of proposed changes.

## LLM Adapter

Standalone module (`src/llm.ts`) with a clean interface:

```ts
interface LLMAdapter {
  complete(system: string, messages: Message[]): Promise<string>;
}
```

Three implementations via raw `fetch`, no SDK dependencies:
- **Anthropic**: `POST https://api.anthropic.com/v1/messages`
- **Gemini**: `POST https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent`
- **AWS Bedrock**: `POST https://bedrock-runtime.{region}.amazonaws.com/model/{model}/invoke` (uses AWS Signature V4 signing)

### Configuration

Environment variables (defaults when step doesn't specify a model):

```
KNOMIT_LLM_PROVIDER=anthropic    # "anthropic", "gemini", or "bedrock"
KNOMIT_LLM_MODEL=claude-sonnet-4-6
ANTHROPIC_API_KEY=sk-ant-...
GOOGLE_AI_API_KEY=AI...
AWS_REGION=us-east-1
AWS_ACCESS_KEY_ID=AKIA...
AWS_SECRET_ACCESS_KEY=...
```

The adapter is reusable for future features. Adding providers is a one-file change.

## Git Integration

Synthesis uses the same learning moment system as the rest of knomit:

- Every learn/forget/update creates a proper commit with a descriptive message
- Moments are tagged with `learn/synthesize-{recipe}` or `forget/synthesize-{recipe}`
- `knomit_why` shows that a fact was created or modified by a synthesis run
- The TUI timeline displays synthesis moments like any other learning episode

### Refactoring needed

The existing tool handlers (`learnHandler`, `forgetHandler`, `updateHandler`) call `sync()` and `push()` internally. Synthesis needs the core logic (commit + tag + index) without sync/push, since it works on a separate branch and batches changes.

Extract core functions from handlers:
- `commitFact(repo, factData)` — write file, commit, index
- `deleteFact(repo, file)` — delete file, commit, remove from index
- `updateFact(repo, file, updates)` — read, merge, write, commit, index
- `tagMoment(repo, momentName)` — create tag

MCP tool registrations continue calling the full handlers. Synthesis calls the core functions directly.

## Scaling

At thousands of facts:
- Facts are chunked into size-based batches for LLM context limits
- Each chunk gets its own LLM call
- A cross-chunk pass finds patterns across chunks
- Each step re-queries from git, so pipeline steps see pruned/updated state

## Not in Scope

- **Scheduling** — external (cron, GitHub Actions, etc.). Auto-discovery mode makes this easy: just run `knomit synthesize --all` on a schedule and each recipe processes only what changed
- **Multi-machine merge** — separate feature (`knomit merge`)
- **Review manifests in frontmatter** — recipe prompts handle staleness rules
- **Priority scoring formulas** — recipe author decides what to review
- **External ingestion** — separate feature; synthesis only works on existing facts
