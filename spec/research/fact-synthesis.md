# Research: Fact Synthesis (`knomit_synthesize`)

## Problem

Knowledge bases grow. Facts become stale (a CVE gets fixed), redundant (15 facts about Alice's music taste that could be 3), or contradictory. Without active maintenance, the repo accumulates noise that degrades query quality and wastes context window tokens.

Two distinct synthesis needs:

1. **Prune** — review existing facts, forget the obsolete
2. **Distill** — review many facts, synthesize fewer higher-order facts

Both share a core loop but differ in prompt intent and output shape.

## The Core Loop

```
1. Select a scope (path, domain, entity, time range)
2. Read all facts in scope
3. Present them to an LLM with a synthesis prompt
4. LLM returns a plan: { learn: [...], update: [...], forget: [...] }
5. Execute the plan using existing tools
```

Synthesis is a compound operation on top of existing primitives:

```
knomit_synthesize
  ├── knomit_query   (read the scope)
  ├── LLM call       (reason about the facts)
  ├── knomit_learn   (create new synthesized facts)
  ├── knomit_update  (adjust confidence on existing)
  └── knomit_forget  (remove subsumed/obsolete facts)
```

No new Git operations needed.

## Mode 1: Prune

**Use case:** CVE-2024-1234 was fixed. A security fact about a dependency being vulnerable is now stale.

- **Selection:** All facts in a path/domain/entity set, filtered by age or low confidence
- **Prompt intent:** "Which of these facts are no longer true? Which are redundant? Which have been superseded?"
- **Output:** Mostly `forget` and `update` (lower confidence). Rarely `learn`.

**Trigger options:**
- Manual: `knomit_synthesize` with `mode: "prune"`
- Automatic: on schedule (cron / startup), or when fact count in a subtree exceeds a threshold
- Reactive: when an external signal arrives (e.g., CVE database says "fixed")

## Mode 2: Distill

**Use case:** 15 individual facts about Alice's music listening → 2-3 higher-order facts about her patterns.

- **Selection:** All facts under a path or matching an entity, especially where `sources` is low and facts are numerous
- **Prompt intent:** "What patterns do you see across these facts? Produce higher-order facts that capture the essence. Identify which originals can be forgotten because the synthesis subsumes them."
- **Output:** `learn` (new synthesized facts with `refs` pointing to originals) + `forget` (originals fully captured by the synthesis)

This is the Weaver Pattern from the MBEKG spec (Section 6), but automated rather than agent-initiated.

## Where Does the LLM Call Live?

| Option | Description | Tradeoff |
|--------|-------------|----------|
| A. Inside Knomit | `knomit_synthesize` calls an LLM internally | Knomit becomes an agent, not just storage |
| B. Outside Knomit | Calling agent reads via query, reasons, calls learn/update/forget | Knomit stays dumb, but caller must be sophisticated |
| C. Hybrid | `knomit_review` returns a "review packet" with staleness signals; agent decides | Intelligence at the edges, but two-step workflow |

**Decision: A for convenience, C for control.**

- **Dumb callers** get `knomit_synthesize(scope, budget)` — one call, fully autonomous
- **Smart callers** get `knomit_review` + manual control over the loop

## Scaling to Thousands of Facts

At 1000+ facts, a single LLM call is impossible — context limits, cost, latency. The ontology (directory tree) provides natural decomposition.

### Bottom-Up Hierarchical Review

```
Pass 1: Leaf directories (most specific)
  worlds/security/dependencies/   → review 8 facts → prune 3, flag 2
  worlds/security/policies/       → review 5 facts → all fine
  worlds/people/alice/music/      → review 15 facts → distill to 3

Pass 2: Parent directories (roll up)
  worlds/security/                → review remaining + summaries from pass 1
  worlds/people/alice/            → review remaining + summaries from pass 1

Pass 3: Top-level
  worlds/                         → cross-cutting patterns only
```

Each pass operates on a small, bounded set — facts directly in that directory, plus compressed summaries from children (not the full child facts).

### What `knomit_review` Returns at Scale

A review plan — a queue of scoped review tasks, not one giant packet:

```ts
{
  strategy: "bottom_up",
  review_units: [
    {
      path: "worlds/security/dependencies/",
      fact_count: 8,
      priority: "high",
      reason: "6/8 facts older than stale_after_days (90)",
    },
    {
      path: "worlds/people/alice/music/",
      fact_count: 15,
      priority: "medium",
      reason: "fact density 15 > max_facts 10",
    },
    {
      path: "worlds/projects/knomit/",
      fact_count: 4,
      priority: "low",
      reason: "routine review, all facts < 30 days old",
    },
  ],
  stats: {
    total_facts: 1200,
    review_units: 34,
    high_priority: 5,
    estimated_tokens_per_unit: "2k-8k",
  }
}
```

The agent works through units by priority, calling `query` per unit, reasoning, then calling `learn/forget/update`.

### Priority Scoring

Each directory gets a priority score:

```
priority = w1 * (stale_fact_ratio)
         + w2 * (fact_density / max_facts)
         + w3 * (orphaned_ref_count)
         + w4 * (avg_age / stale_after_days)
```

High-priority units are reviewed first. Low-priority ones can be skipped if the agent has a time/cost budget.

### Summary Propagation

After reviewing leaves, parents need context. Each leaf review produces a 1-2 sentence summary passed upward:

```
Children summaries:
- dependencies/: pruned 3/8 (CVEs resolved), 5 remain
- policies/: no changes, 5 facts healthy
- audits/: distilled 12 → 4 quarterly summaries

Direct facts in security/:
- [2 facts, both current]

Question: any cross-cutting action needed?
```

### Cost Envelope

For a 1200-fact repo:

| Metric | Estimate |
|--------|----------|
| Review units | ~30-50 directories |
| Tokens per unit | 2k-8k (facts) + 1k (prompt) |
| Total input tokens | ~150k-300k |
| Total output tokens | ~30k-50k (decisions) |
| Cost (Haiku triage, Sonnet decisions) | ~$0.10-0.30 per full review |
| Wall time (parallel) | ~30-60 seconds |

## Staleness Signals

Domain-aware heuristics without Knomit needing domain knowledge:

| Signal | Heuristic |
|--------|-----------|
| Age in volatile domain | `security`, `market`, `versions` domains → flag if > 90 days untouched |
| Low confidence + old | `confidence < 0.5` and `age > 180 days` → candidate for pruning |
| High fact density | > 10 facts under one path → candidate for distillation |
| Redundant entities | Multiple facts sharing 80%+ of the same entities → candidate for merge |
| Orphaned refs | `refs` point to commits/files that no longer exist → flag |

### Configurable via Manifests

Domain owners declare what "stale" means for their subtree by adding a `review` section to manifest frontmatter:

```yaml
# worlds/security.md
---
domain: [security]
confidence: 1.0
sources: 1
entities: [security]
refs: []
review:
  stale_after_days: 90
  max_facts: 20
---
```

## Tool Interface

```ts
knomit_synthesize({
  scope: "worlds/security/",       // path prefix, or "worlds/" for full repo
  mode: "prune" | "distill" | "full",
  budget: {
    max_units: 10,                  // stop after N directories
    max_tokens: 500_000,            // total token budget
    priority_threshold: "medium",   // skip low-priority units
  },
  llm: {
    provider: "anthropic" | "openai" | "google",
    model: "claude-sonnet-4-6" | "gpt-4o" | "gemini-2.0-flash",
    api_key: "sk-...",             // or read from env
  }
})
```

## Model-Agnostic LLM Adapter

Thin wrapper — all three providers have near-identical chat completion APIs. No SDKs needed — just `fetch`. Zero dependencies. Works in Bun natively.

```ts
interface LLMAdapter {
  complete(messages: Message[]): Promise<string>;
}

function createAdapter(config: LLMConfig): LLMAdapter {
  switch (config.provider) {
    case "anthropic":
      // POST https://api.anthropic.com/v1/messages
    case "openai":
      // POST https://api.openai.com/v1/chat/completions
    case "google":
      // POST https://generativelanguage.googleapis.com/v1beta/models/...
  }
}
```

### API Key Resolution

Keys via env vars (Bun auto-loads `.env`):

```env
KNOMIT_LLM_PROVIDER=anthropic
KNOMIT_LLM_MODEL=claude-sonnet-4-6
ANTHROPIC_API_KEY=sk-ant-...
# or OPENAI_API_KEY=sk-...
# or GOOGLE_AI_API_KEY=AI...
```

`knomit_synthesize` defaults to env vars when `llm` isn't passed explicitly.

## Synthesis Prompts

### Prune Prompt

```
You are reviewing facts in a knowledge base for staleness and redundancy.

Facts in scope:
{facts_json}

Domain review rules (from manifest):
- stale_after_days: {stale_after_days}
- max_facts: {max_facts}

For each fact, decide:
- KEEP: fact is current and valuable
- FORGET: fact is obsolete, superseded, or no longer true
- UPDATE: fact needs confidence adjusted (provide new value and reason)

Respond as JSON:
{
  "decisions": [
    { "file": "...", "action": "keep|forget|update", "confidence"?: 0.X, "reason": "..." }
  ],
  "summary": "one sentence summary of what changed"
}
```

### Distill Prompt

```
You are synthesizing facts in a knowledge base.

Facts in scope:
{facts_json}

Identify patterns across these facts. Produce:
1. New higher-order facts that capture patterns (with refs to source facts)
2. Which original facts are fully subsumed and can be forgotten

Respond as JSON:
{
  "synthesize": [
    { "path": "...", "title": "...", "body": "...", "domain": [...],
      "confidence": 0.X, "entities": [...], "refs": [...] }
  ],
  "forget": ["file1.md", "file2.md"],
  "summary": "..."
}
```

## Summary

| Aspect | Detail |
|--------|--------|
| What | Automated fact maintenance — prune stale facts, distill many into few |
| How | Bottom-up hierarchical review using the ontology tree |
| Intelligence | Pluggable LLM (Anthropic, OpenAI, Google) via raw `fetch` |
| Scaling | Review units per directory, priority-scored, budget-capped |
| Dependencies | Zero — existing knomit tools + `fetch` |
| Interface | `knomit_synthesize` for one-call autonomy, `knomit_review` for manual control |
