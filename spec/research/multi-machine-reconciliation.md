# Research: Multi-Agent Reconciliation

## Problem

Two (or more) agents share the same origin repo, each working on its own `agent/{hostname}` branch. Each agent learns facts independently, pushes its branch, and pulls `origin/main` on sync. But nothing ever merges agent branches into main — knowledge stays siloed.

```
origin/main          ─────────────────────────────────────────────►
                          ↑                          ↑
agent/laptop       ──●──●──●                       │
                                                     │
agent/desktop      ────────────●──●──●             │
                                                     │
                     ◄── who merges these into main? ─┘
```

## Solution: Three-Phase Reconciliation

### Phase 1: Propose

For each agent branch ahead of main, compute a diff — the list of new, changed, and deleted facts. Package each as a "review packet."

### Phase 2: Synthesize

Combine review packets from all branches. Classify each change:

- **Clean merge** — no overlap, git handles it automatically
- **Metadata conflict** — same file changed on two branches (confidence, sources differ)
- **Semantic conflict** — different files that contradict each other (only LLM catches this)
- **Obsolescence** — one branch adds a fact, another deletes it

For metadata conflicts, deterministic rules can resolve most cases. For semantic conflicts, an LLM reviews and resolves.

### Phase 3: Merge

Apply resolved changes to main. Each agent picks up the merged result on its next `sync()`.

## Merge Tool API

```ts
knomit_merge({
  strategy: "auto" | "review" | "dry_run",
})
```

- **`dry_run`** — report what would happen: clean merges, conflicts, semantic conflicts
- **`auto`** — merge clean changes, apply deterministic rules for metadata conflicts, skip semantic conflicts
- **`review`** — full LLM-assisted resolution including semantic conflicts

## Key Principle

Agents stay simple — push your branch, pull main. The reconciliation intelligence lives outside the agents, either in a GitHub Action or a dedicated merge tool invocation.
