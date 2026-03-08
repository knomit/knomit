# Research: Fact Merge (`knomit_merge`)

## Problem

Two or more machines share the same origin repo, each on its own `machine/{hostname}` branch. Each machine learns facts independently, pushes its branch, and pulls `origin/main` on sync. But nothing merges machine branches into main — knowledge stays siloed.

```
origin/main          ─────────────────────────────────────────────►
                          ↑                          ↑
machine/laptop       ──●──●──●                       │
                                                     │
machine/desktop      ────────────●──●──●             │
                                                     │
                     ◄── who merges these into main? ─┘
```

## Solution: Three-Phase Reconciliation

### Phase 1: Propose

For each machine branch ahead of main, compute a diff — the list of new, changed, and deleted facts. Package each as a "review packet."

### Phase 2: Synthesize

Combine review packets from all branches. Classify each change:

| Case | Git conflict? | Semantic conflict? | Resolution |
|------|--------------|-------------------|------------|
| Disjoint facts | No | No | Auto-merge (git handles it) |
| Same fact reinforced | Yes | No | Deterministic rules (see below) |
| Contradictory facts | No | Yes | LLM review required |
| Add vs delete | Maybe | Maybe | Recency + intent |

### Phase 3: Merge

Apply resolved changes to main. Each machine picks up the merged result on its next `sync()`.

## Merge Cases in Detail

### Case 1: Disjoint Facts (No Conflict)

Two branches add completely different facts. No file overlap.

```
laptop:  + worlds/security/cve-2024-1234.md
desktop: + worlds/people/alice/prefers-dark-mode.md
```

**Resolution:** Trivial. Git auto-merges. No LLM needed.

### Case 2: Same Fact, Independent Reinforcement

Both branches update the same fact file — different confidence bumps, different new sources.

```
laptop:  worlds/people/alice/likes-rock.md  (confidence: 0.85 → 0.9, sources: 3 → 4)
desktop: worlds/people/alice/likes-rock.md  (confidence: 0.85 → 0.88, sources: 3 → 5)
```

**Resolution (deterministic rules):**
- Confidence: `max(0.9, 0.88)` = 0.9
- Sources: sum deltas — `(4-3) + (5-3) + 3` = 6
- Body: merge if different paragraphs changed; flag for review if same paragraph diverged

### Case 3: Semantic Contradiction

No file-level conflict — different files — but the facts contradict each other.

```
laptop:  + worlds/projects/api/uses-rest.md    (confidence: 0.8)
desktop: + worlds/projects/api/uses-graphql.md  (confidence: 0.7)
```

Git merges cleanly (different files), but the knowledge base now contains a contradiction.

**Resolution (LLM required):**
- Option A: Keep higher confidence, forget the other
- Option B: Keep both with lowered confidence (maybe the API genuinely uses both)
- Option C: Flag for human review

**Detection:** Requires semantic analysis — compare new facts against existing facts in the same world/entity path. Hardest case, most justifies LLM involvement.

### Case 4: Obsolescence Across Branches

One branch learns a fact. Another branch deletes it.

```
laptop:  + worlds/security/cve-2024-1234.md  (committed 3 months ago)
desktop: forget worlds/security/cve-2024-1234.md  (committed yesterday, CVE was fixed)
```

**Resolution:** The delete wins — more recent and intentional. Recency + intentional deletion beats older addition.

**Edge case:** If the addition is more recent than the deletion, flag for review — someone may have re-learned a fact that was previously forgotten for good reason.

## Tool Interface

```ts
knomit_merge({
  strategy: "auto" | "review" | "dry_run",
})
```

| Strategy | Behavior |
|----------|----------|
| `dry_run` | Report what would happen: clean merges, conflicts, semantic conflicts |
| `auto` | Merge clean changes, apply deterministic rules for metadata conflicts, skip semantic conflicts |
| `review` | Full LLM-assisted resolution including semantic conflicts |

### Dry Run Output

```ts
{
  branches: ["machine/laptop", "machine/desktop"],
  clean_merges: [
    { file: "worlds/security/cve-2024-1234.md", from: "machine/laptop", action: "add" }
  ],
  conflicts: [
    {
      file: "worlds/people/alice/likes-rock.md",
      versions: [
        { branch: "machine/laptop", confidence: 0.9, sources: 4 },
        { branch: "machine/desktop", confidence: 0.88, sources: 5 },
      ]
    }
  ],
  semantic_conflicts: [
    {
      files: ["worlds/projects/api/uses-rest.md", "worlds/projects/api/uses-graphql.md"],
      reason: "contradictory facts about same entity from different branches"
    }
  ]
}
```

## Git Mechanics

```ts
async reconcile(strategy: string): Promise<MergeResult> {
  // 1. Fetch all remote branches
  await git.fetch("origin");

  // 2. Find machine branches ahead of main
  const branches = (await git.branch({ remote: true }))
    .filter(b => b.startsWith("origin/machine/"));

  // 3. For each branch, compute diff vs main
  const diffs = await Promise.all(
    branches.map(b => git.diffFiles("origin/main", b))
  );

  // 4. Partition into clean merges vs conflicts
  const { clean, conflicting } = partitionByConflict(diffs);

  // 5. Create a temp merge branch off main
  await git.checkout("-b", "merge/reconcile", "origin/main");

  // 6. Apply clean merges
  for (const change of clean) {
    await git.cherryPick(change.commit);
  }

  // 7. Resolve conflicts (deterministic or LLM-assisted)
  for (const conflict of conflicting) {
    const resolution = resolve(conflict, strategy);
    await applyResolution(resolution);
  }

  // 8. Push merge branch → PR or direct merge to main
  await git.push("origin", "merge/reconcile");
}
```

## Where It Runs

| Option | How | Pros | Cons |
|--------|-----|------|------|
| **GitHub Action** | Triggers on push to any `machine/**` branch | Fully automated, no machine needs to be online | Needs LLM API key in GitHub secrets |
| **On a machine** | User runs `knomit merge` manually or on a schedule | Simple, no infra | That machine must be online |
| **Container/server** | Central server runs reconciliation on a cron | Always available | Adds ops burden |

### Recommended: GitHub Action

Triggers automatically when any machine pushes, runs reconciliation, and either auto-merges (clean) or opens a PR (conflicts).

```yaml
# .github/workflows/reconcile.yml
on:
  push:
    branches: ["machine/**"]

jobs:
  reconcile:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: oven-sh/setup-bun@v2
      - run: bun install
      - run: bun src/cli.ts merge --strategy=auto
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
```

## End-to-End Flow

```
Machine A (laptop)                    Machine B (desktop)
    │                                       │
    ├── learn facts                         ├── learn facts
    ├── push machine/laptop                 ├── push machine/desktop
    │                                       │
    └──────────► origin ◄──────────────────┘
                    │
                    ▼
              GitHub Action triggers
                    │
              knomit merge --strategy
                    │
              ┌─────┴─────┐
              │ clean?     │ conflicts?
              │ auto-merge │ LLM resolves → PR
              └─────┬─────┘
                    │
                    ▼
               origin/main updated
                    │
        ┌───────────┴───────────┐
        ▼                       ▼
  Machine A sync()        Machine B sync()
  (pulls new main)        (pulls new main)
```

## Key Principle

Machines stay simple — push your branch, pull main. The reconciliation intelligence lives outside the machines, either in a GitHub Action or a dedicated merge tool invocation.

## Relationship to `knomit_synthesize`

Both tools involve LLM-assisted fact reasoning, but they solve different problems:

| | `knomit_merge` | `knomit_synthesize` |
|-|----------------|---------------------|
| **Trigger** | Multiple machine branches diverge from main | Fact count grows, facts age |
| **Input** | Diffs between branches | Facts within a scope |
| **Problem** | Reconcile divergent knowledge across machines | Maintain knowledge quality within one branch |
| **LLM role** | Resolve semantic conflicts between versions | Identify staleness, redundancy, patterns |
| **Git ops** | Merge branches | Learn/update/forget on current branch |

They complement each other: `knomit_merge` unifies knowledge across machines, `knomit_synthesize` maintains the quality of the unified result. A typical flow might be: merge first, then synthesize the merged result to prune duplicates that arose from independent learning.
