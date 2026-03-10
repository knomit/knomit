# Research: Reconciliation Architecture

How and where multi-agent reconciliation runs.

## Git Mechanics

```ts
async reconcile(strategy: string): Promise<MergeResult> {
  // 1. Fetch all remote branches
  await git.fetch("origin");

  // 2. Find agent branches ahead of main
  const branches = (await git.branch({ remote: true }))
    .filter(b => b.startsWith("origin/agent/"));

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

## Dry Run Output

Before merging, `dry_run` mode returns a structured report:

```ts
{
  branches: ["agent/laptop", "agent/desktop"],
  clean_merges: [
    { file: "know/security/cve-2024-1234.md", from: "agent/laptop", action: "add" }
  ],
  conflicts: [
    {
      file: "know/people/alice/likes-rock.md",
      versions: [
        { branch: "agent/laptop", confidence: 0.9, sources: 4 },
        { branch: "agent/desktop", confidence: 0.88, sources: 5 },
      ]
    }
  ],
  semantic_conflicts: [
    {
      files: ["know/projects/api/uses-rest.md", "know/projects/api/uses-graphql.md"],
      reason: "contradictory facts about same entity from different branches"
    }
  ]
}
```

## Where It Runs

| Option | How | Pros | Cons |
|--------|-----|------|------|
| **GitHub Action** | Triggers on push to any `agent/**` branch | Fully automated, no machine needs to be online | Needs LLM API key in GitHub secrets |
| **On a machine** | User runs `knomit merge` manually or on a schedule | Simple, no infra | That machine must be online |
| **Container/server** | Central server runs reconciliation on a cron | Always available | Adds ops burden |

### Recommended: GitHub Action

The cleanest fit. Triggers automatically when any machine pushes, runs reconciliation, and either auto-merges (clean) or opens a PR (conflicts).

```yaml
# .github/workflows/reconcile.yml
on:
  push:
    branches: ["agent/**"]

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
    ├── push agent/laptop                 ├── push agent/desktop
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

Each agent stays dumb: push your branch, pull main. Reconciliation intelligence lives in the action.
