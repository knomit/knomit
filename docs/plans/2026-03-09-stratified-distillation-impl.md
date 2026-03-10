# Stratified Distillation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace distill mode's size-based chunking with multi-signal clustering (UMAP + HDBSCAN + FCA validation) and add RAPTOR-style recursion.

**Architecture:** New `src/cluster.ts` module handles dimensionality reduction, density-based clustering, and metadata validation. `src/synthesize.ts` consumes it — `executeDistillStep` calls `clusterFacts()` instead of `chunkFacts()`, then iterates clusters for LLM distillation with optional recursion up to `max_depth`.

**Tech Stack:** umap-js (dimensionality reduction), hdbscan-ts (clustering), existing Embedder class for vectors.

---

### Task 1: Add dependencies

**Files:**
- Modify: `src/package.json`

**Step 1: Install umap-js and hdbscan-ts**

Run: `cd src && bun add umap-js hdbscan-ts`

**Step 2: Verify installation**

Run: `cd src && bun -e "import { UMAP } from 'umap-js'; import { HDBSCAN } from 'hdbscan-ts'; console.log('OK')"`
Expected: `OK`

**Step 3: Commit**

```bash
git add src/package.json src/bun.lock
git commit -m "deps: add umap-js and hdbscan-ts for clustering"
```

---

### Task 2: Add cluster params to recipe schema

**Files:**
- Modify: `src/recipe.ts`
- Test: `src/recipe.test.ts`

**Step 1: Write the failing test**

Add to `src/recipe.test.ts`:

```ts
it("parses distill step with cluster params", () => {
  const yaml = `
name: cluster-test
steps:
  - mode: distill
    max_depth: 3
    umap_dimensions: 10
    min_cluster_size: 5
`;
  const recipe = parseRecipe(yaml);
  expect(recipe.steps[0].max_depth).toBe(3);
  expect(recipe.steps[0].umap_dimensions).toBe(10);
  expect(recipe.steps[0].min_cluster_size).toBe(5);
});

it("defaults cluster params when omitted", () => {
  const yaml = `
name: default-test
steps:
  - mode: distill
`;
  const recipe = parseRecipe(yaml);
  expect(recipe.steps[0].max_depth).toBe(1);
  expect(recipe.steps[0].umap_dimensions).toBe(5);
  expect(recipe.steps[0].min_cluster_size).toBe(3);
});
```

**Step 2: Run test to verify it fails**

Run: `cd src && bun test recipe.test.ts`
Expected: FAIL — `max_depth`, `umap_dimensions`, `min_cluster_size` not in schema

**Step 3: Write minimal implementation**

In `src/recipe.ts`, update `StepSchema`:

```ts
const StepSchema = z.object({
  mode: z.enum(["prune", "distill"]),
  model: z.string().optional(),
  prompt: z.string().optional().default(""),
  max_depth: z.number().int().min(1).optional().default(1),
  umap_dimensions: z.number().int().min(2).optional().default(5),
  min_cluster_size: z.number().int().min(2).optional().default(3),
});
```

**Step 4: Run test to verify it passes**

Run: `cd src && bun test recipe.test.ts`
Expected: All PASS

**Step 5: Commit**

```bash
git add src/recipe.ts src/recipe.test.ts
git commit -m "feat: add cluster params to recipe step schema"
```

---

### Task 3: Implement FCA validation (metadata split)

**Files:**
- Create: `src/cluster.ts`
- Test: `src/cluster.test.ts`

This task implements just the FCA validation function — not the full pipeline. This is independently testable with no external deps.

**Step 1: Write the failing test**

Create `src/cluster.test.ts`:

```ts
import { describe, it, expect } from "bun:test";
import { splitByMetadata } from "./cluster";
import type { FactForLLM } from "./synthesize";

function makeFact(path: string, domain: string[], entities: string[]): FactForLLM {
  return { path, title: path, body: "", domain, entities, confidence: 0.8, sources: 1, refs: [] };
}

describe("splitByMetadata", () => {
  it("does not split a cluster with shared domains", () => {
    const facts = [
      makeFact("a.md", ["security"], ["alice"]),
      makeFact("b.md", ["security"], ["bob"]),
    ];
    const result = splitByMetadata(facts, 2);
    expect(result).toHaveLength(1);
    expect(result[0]).toHaveLength(2);
  });

  it("splits a cluster with disjoint metadata into components", () => {
    const facts = [
      makeFact("a.md", ["security"], ["alice"]),
      makeFact("b.md", ["security"], ["alice"]),
      makeFact("c.md", ["cooking"], ["bob"]),
      makeFact("d.md", ["cooking"], ["bob"]),
    ];
    const result = splitByMetadata(facts, 2);
    expect(result).toHaveLength(2);
  });

  it("demotes subgroups below min_cluster_size to empty (filtered out)", () => {
    const facts = [
      makeFact("a.md", ["security"], ["alice"]),
      makeFact("b.md", ["security"], ["alice"]),
      makeFact("c.md", ["cooking"], ["bob"]),  // singleton subgroup
    ];
    const result = splitByMetadata(facts, 2);
    // security group survives (size 2), cooking group (size 1) is below min
    expect(result).toHaveLength(1);
    expect(result[0]).toHaveLength(2);
  });

  it("keeps cluster intact when all facts have empty metadata", () => {
    const facts = [
      makeFact("a.md", [], []),
      makeFact("b.md", [], []),
    ];
    const result = splitByMetadata(facts, 2);
    expect(result).toHaveLength(1);
  });

  it("connects facts sharing an entity even with different domains", () => {
    const facts = [
      makeFact("a.md", ["security"], ["alice"]),
      makeFact("b.md", ["cooking"], ["alice"]),
    ];
    const result = splitByMetadata(facts, 2);
    expect(result).toHaveLength(1); // connected via "alice"
  });
});
```

**Step 2: Run test to verify it fails**

Run: `cd src && bun test cluster.test.ts`
Expected: FAIL — module not found

**Step 3: Write minimal implementation**

Create `src/cluster.ts`:

```ts
import type { FactForLLM } from "./synthesize";

/**
 * FCA-lite: split a cluster into connected components based on shared
 * domains or entities. Two facts are connected if they share at least
 * one domain tag or one entity tag.
 *
 * Returns groups that meet min_cluster_size. Smaller groups are discarded
 * (their facts become noise — caller handles this).
 */
export function splitByMetadata(
  facts: FactForLLM[],
  minClusterSize: number
): FactForLLM[][] {
  const n = facts.length;
  if (n === 0) return [];

  // Union-Find
  const parent = Array.from({ length: n }, (_, i) => i);
  function find(x: number): number {
    while (parent[x] !== x) { parent[x] = parent[parent[x]]; x = parent[x]; }
    return x;
  }
  function union(a: number, b: number): void {
    const ra = find(a), rb = find(b);
    if (ra !== rb) parent[ra] = rb;
  }

  // Build inverted indices: tag → fact indices
  const tagToIndices = new Map<string, number[]>();
  for (let i = 0; i < n; i++) {
    const fact = facts[i];
    for (const d of fact.domain) {
      const key = `d:${d}`;
      if (!tagToIndices.has(key)) tagToIndices.set(key, []);
      tagToIndices.get(key)!.push(i);
    }
    for (const e of fact.entities) {
      const key = `e:${e}`;
      if (!tagToIndices.has(key)) tagToIndices.set(key, []);
      tagToIndices.get(key)!.push(i);
    }
  }

  // Union facts that share any tag
  for (const indices of tagToIndices.values()) {
    for (let i = 1; i < indices.length; i++) {
      union(indices[0], indices[i]);
    }
  }

  // If no tags at all, everything is one component
  if (tagToIndices.size === 0) return [facts];

  // Group by root
  const components = new Map<number, FactForLLM[]>();
  for (let i = 0; i < n; i++) {
    const root = find(i);
    if (!components.has(root)) components.set(root, []);
    components.get(root)!.push(facts[i]);
  }

  // Filter by min size — if only one component, return it regardless
  const groups = [...components.values()];
  if (groups.length === 1) return groups;
  return groups.filter((g) => g.length >= minClusterSize);
}
```

**Step 4: Run test to verify it passes**

Run: `cd src && bun test cluster.test.ts`
Expected: All PASS

**Step 5: Commit**

```bash
git add src/cluster.ts src/cluster.test.ts
git commit -m "feat: FCA-lite metadata validation for cluster splitting"
```

---

### Task 4: Implement full clusterFacts pipeline

**Files:**
- Modify: `src/cluster.ts`
- Modify: `src/cluster.test.ts`

**Step 1: Write the failing tests**

Add to `src/cluster.test.ts`:

```ts
import { clusterFacts, splitByMetadata } from "./cluster";

describe("clusterFacts", () => {
  it("returns all facts as noise when fewer than minClusterSize", () => {
    const facts = [makeFact("a.md", ["d"], ["e"])];
    const embeddings = new Map([["a.md", new Float32Array(384)]]);
    const result = clusterFacts(facts, embeddings, { minClusterSize: 3 });
    expect(result.clusters.size).toBe(0);
    expect(result.noise).toHaveLength(1);
  });

  it("puts facts without embeddings into noise", () => {
    const facts = [
      makeFact("a.md", ["d"], ["e"]),
      makeFact("b.md", ["d"], ["e"]),
    ];
    const embeddings = new Map([["a.md", new Float32Array(384)]]);
    // b.md has no embedding
    const result = clusterFacts(facts, embeddings, { minClusterSize: 2 });
    expect(result.noise.some((f) => f.path === "b.md")).toBe(true);
  });

  it("clusters similar embeddings together", () => {
    // Create two groups of similar embeddings
    const n = 10;
    const facts: FactForLLM[] = [];
    const embeddings = new Map<string, Float32Array>();
    for (let i = 0; i < n; i++) {
      const path = `know/group-a/f${i}.md`;
      facts.push(makeFact(path, ["security"], ["alice"]));
      const vec = new Float32Array(384);
      // Group A: high values in first half
      for (let j = 0; j < 192; j++) vec[j] = 0.8 + Math.random() * 0.1;
      for (let j = 192; j < 384; j++) vec[j] = Math.random() * 0.1;
      embeddings.set(path, vec);
    }
    for (let i = 0; i < n; i++) {
      const path = `know/group-b/f${i}.md`;
      facts.push(makeFact(path, ["cooking"], ["bob"]));
      const vec = new Float32Array(384);
      // Group B: high values in second half
      for (let j = 0; j < 192; j++) vec[j] = Math.random() * 0.1;
      for (let j = 192; j < 384; j++) vec[j] = 0.8 + Math.random() * 0.1;
      embeddings.set(path, vec);
    }
    const result = clusterFacts(facts, embeddings, { minClusterSize: 3, umapDimensions: 5 });
    // Should find at least 2 clusters (the two clear groups)
    expect(result.clusters.size).toBeGreaterThanOrEqual(2);
  });
});
```

**Step 2: Run test to verify it fails**

Run: `cd src && bun test cluster.test.ts`
Expected: FAIL — `clusterFacts` not exported

**Step 3: Write the implementation**

Add to `src/cluster.ts`:

```ts
import { UMAP } from "umap-js";
import { HDBSCAN } from "hdbscan-ts";

export interface ClusterOptions {
  umapDimensions?: number;
  minClusterSize?: number;
  minSamples?: number;
}

export interface ClusterResult {
  clusters: Map<number, FactForLLM[]>;
  noise: FactForLLM[];
}

export function clusterFacts(
  facts: FactForLLM[],
  embeddings: Map<string, Float32Array>,
  options?: ClusterOptions
): ClusterResult {
  const umapDims = options?.umapDimensions ?? 5;
  const minClusterSize = options?.minClusterSize ?? 3;
  const minSamples = options?.minSamples ?? minClusterSize;

  // Separate facts with and without embeddings
  const withEmbeddings: { fact: FactForLLM; vec: Float32Array }[] = [];
  const noise: FactForLLM[] = [];

  for (const fact of facts) {
    const vec = embeddings.get(fact.path);
    if (vec) {
      withEmbeddings.push({ fact, vec });
    } else {
      noise.push(fact);
    }
  }

  if (withEmbeddings.length < minClusterSize) {
    return { clusters: new Map(), noise: [...noise, ...withEmbeddings.map((e) => e.fact)] };
  }

  // UMAP: reduce to target dimensions
  const vectors = withEmbeddings.map((e) => Array.from(e.vec));
  const umap = new UMAP({
    nComponents: umapDims,
    nNeighbors: Math.min(15, Math.floor(withEmbeddings.length / 2)),
    minDist: 0.1,
    spread: 1.0,
  });
  const reduced = umap.fit(vectors);

  // HDBSCAN: density-based clustering
  const hdbscan = new HDBSCAN({
    minClusterSize,
    minSamples,
  });
  const labels = hdbscan.fit(reduced);

  // Group by cluster label (-1 = noise)
  const rawClusters = new Map<number, FactForLLM[]>();
  for (let i = 0; i < labels.length; i++) {
    const label = labels[i];
    if (label === -1) {
      noise.push(withEmbeddings[i].fact);
    } else {
      if (!rawClusters.has(label)) rawClusters.set(label, []);
      rawClusters.get(label)!.push(withEmbeddings[i].fact);
    }
  }

  // FCA validation: split clusters with disjoint metadata
  const clusters = new Map<number, FactForLLM[]>();
  let nextId = 0;
  for (const [, clusterFacts] of rawClusters) {
    const subgroups = splitByMetadata(clusterFacts, minClusterSize);
    for (const group of subgroups) {
      clusters.set(nextId++, group);
    }
    // Facts that fell below minClusterSize in split become noise
    const keptPaths = new Set(subgroups.flat().map((f) => f.path));
    for (const fact of clusterFacts) {
      if (!keptPaths.has(fact.path)) noise.push(fact);
    }
  }

  return { clusters, noise };
}
```

Note: The exact import paths for `umap-js` and `hdbscan-ts` may vary. Check the installed packages:
- `umap-js`: `import { UMAP } from "umap-js"`
- `hdbscan-ts`: Check `node_modules/hdbscan-ts/` for the actual export. May be `import HDBSCAN from "hdbscan-ts"` or `import { HDBSCAN } from "hdbscan-ts"`. Verify with: `bun -e "import x from 'hdbscan-ts'; console.log(typeof x)"`

The `fit()` return types and constructor signatures also need verification against the actual library APIs. Check:
- `node_modules/hdbscan-ts/dist/index.d.ts` for HDBSCAN constructor options and `fit()` signature
- `node_modules/umap-js/dist/umap.d.ts` for UMAP constructor options and `fit()` return type

**Step 4: Run test to verify it passes**

Run: `cd src && bun test cluster.test.ts`
Expected: All PASS (the synthetic embedding test may need tuning if HDBSCAN doesn't find exactly 2 clusters — adjust `minClusterSize` or vector separation as needed)

**Step 5: Commit**

```bash
git add src/cluster.ts src/cluster.test.ts
git commit -m "feat: clusterFacts pipeline with UMAP, HDBSCAN, and FCA validation"
```

---

### Task 5: Add `getEmbeddings` method to SearchIndex

**Files:**
- Modify: `src/search-index.ts`

The synthesize engine needs to fetch all embeddings for a set of fact paths. Currently there's no bulk embedding retrieval method.

**Step 1: Write the failing test**

Add to `src/search-index.test.ts` (or verify the test pattern — this is a simple getter, may not need a dedicated test if covered by integration):

This is a simple method — skip dedicated unit test, will be covered by integration tests in Task 7.

**Step 2: Add the method**

Add to `SearchIndex` class in `src/search-index.ts` (after the `hasEmbeddings` getter around line 563):

```ts
getEmbeddings(paths: string[]): Map<string, Float32Array> {
  if (!this.db || !this.embedder) return new Map();
  const result = new Map<string, Float32Array>();
  const stmt = this.db.query("SELECT path, embedding FROM facts_vec WHERE path = ?");
  for (const p of paths) {
    const row = stmt.get(p) as { path: string; embedding: Float32Array } | null;
    if (row) result.set(row.path, new Float32Array(row.embedding));
  }
  return result;
}
```

**Step 3: Commit**

```bash
git add src/search-index.ts
git commit -m "feat: add getEmbeddings bulk retrieval to SearchIndex"
```

---

### Task 6: Expose Embedder for on-the-fly embedding

**Files:**
- Modify: `src/search-index.ts`

`distill` needs to embed newly created facts during RAPTOR recursion (they won't be in the index yet). Expose the Embedder instance.

**Step 1: Add getter**

Add to `SearchIndex` class:

```ts
getEmbedder(): Embedder | null {
  return this.embedder;
}
```

**Step 2: Commit**

```bash
git add src/search-index.ts
git commit -m "feat: expose Embedder from SearchIndex for on-the-fly embedding"
```

---

### Task 7: Rewrite executeDistillStep to use clustering

**Files:**
- Modify: `src/synthesize.ts`

This is the core integration. Replace the chunk-based flow with cluster-based flow.

**Step 1: Write the failing test**

Add to `src/synthesize.test.ts`:

```ts
describe("executeDistillStep integration", () => {
  // This tests the prompt building and response parsing side.
  // The full integration with clustering requires mocking UMAP/HDBSCAN
  // which is better tested in synthesize-e2e.test.ts.
  // Here we verify the distill prompt still works correctly.

  it("buildDistillPrompt includes cluster context hint", () => {
    const facts = [
      { path: "know/test.md", title: "Test", body: "Body", domain: ["d"], entities: ["e"], confidence: 0.8, sources: 1, refs: [] },
    ];
    const prompt = buildDistillPrompt(facts, "recipe ctx", "step ctx");
    expect(prompt).toContain("synthesizing facts");
    expect(prompt).toContain("Test");
  });
});
```

**Step 2: Modify executeDistillStep**

In `src/synthesize.ts`, rewrite `executeDistillStep` (lines 497-615):

Key changes:
1. At the start, check `searchIndex.hasEmbeddings` — if false, throw: `"Distill mode requires embeddings. Enable embeddings in your SearchIndex configuration."`
2. After gathering facts, call `searchIndex.getEmbeddings(facts.map(f => f.path))` to get embeddings
3. Call `clusterFacts(facts, embeddings, { umapDimensions: step.umap_dimensions, minClusterSize: step.min_cluster_size })` instead of `chunkFacts(facts, 100_000)`
4. Log noise count: `log.info(\`distill: ${result.noise.length} facts classified as noise (skipped)\`)`
5. For each cluster, build distill prompt and call LLM (same as current per-chunk logic)
6. If a single cluster's JSON exceeds 100KB, fall back to `chunkFacts` within that cluster
7. Remove the cross-chunk consolidation pass (lines 542-573 in current code)
8. Add RAPTOR recursion loop around the cluster→distill flow, controlled by `step.max_depth`
9. Add new progress events: `{ phase: "cluster"; clusters: number; noise: number }` and `{ phase: "raptor-depth"; depth: number; maxDepth: number }`

The RAPTOR loop:
```ts
let currentFacts = facts;
let currentEmbeddings = embeddingsMap;
const allSynthesized: DistillFact[] = [];
const allForget: string[] = [];
const summaries: string[] = [];

for (let depth = 0; depth < step.max_depth; depth++) {
  onProgress?.({ phase: "raptor-depth", depth: depth + 1, maxDepth: step.max_depth });

  const clusterResult = clusterFacts(currentFacts, currentEmbeddings, {
    umapDimensions: step.umap_dimensions,
    minClusterSize: step.min_cluster_size,
  });

  if (clusterResult.clusters.size === 0) {
    log.info(`distill: no clusters at depth ${depth + 1}, stopping`);
    break;
  }

  // LLM distill per cluster...
  // Collect new synthesized facts

  if (depth + 1 < step.max_depth && roundSynthesized.length > 0) {
    // Re-embed new facts for next depth
    const embedder = searchIndex.getEmbedder()!;
    const newEmbeddings = new Map<string, Float32Array>();
    for (const fact of roundSynthesized) {
      const text = `${fact.title} ${fact.body} ${fact.entities.join(" ")} ${fact.domain.join(" ")}`;
      const vec = await embedder.embed(text);
      newEmbeddings.set(fact.path, vec);
    }
    currentFacts = roundSynthesized.map(f => ({ ...f, sources: 1 }));
    currentEmbeddings = newEmbeddings;
  }
}
```

**Step 3: Update ProgressEvent type**

Add to the `ProgressEvent` union in `src/synthesize.ts`:

```ts
| { phase: "cluster"; clusters: number; noise: number }
| { phase: "raptor-depth"; depth: number; maxDepth: number }
```

**Step 4: Add import**

At top of `src/synthesize.ts`:
```ts
import { clusterFacts } from "./cluster";
```

**Step 5: Run all tests**

Run: `cd src && bun test`
Expected: All existing tests still pass. The unit tests for buildPrompt/parseResponse are unchanged.

**Step 6: Commit**

```bash
git add src/synthesize.ts
git commit -m "feat: replace distill chunking with cluster-based grouping and RAPTOR recursion"
```

---

### Task 8: Update CLI progress rendering

**Files:**
- Modify: `src/cli/synthesize.ts`

**Step 1: Check current progress handler**

Read `src/cli/synthesize.ts` and find the `onProgress` callback handler.

**Step 2: Add handlers for new events**

Add cases for `phase: "cluster"` and `phase: "raptor-depth"` to the progress rendering switch/if-chain.

Example:
- `cluster`: `"Clustered N facts into M clusters (K noise)"`
- `raptor-depth`: `"RAPTOR depth N/M"`

**Step 3: Run synthesize command manually to verify output**

Run: `cd src && bun run dev -- synthesize --help` (just verify it doesn't crash)

**Step 4: Commit**

```bash
git add src/cli/synthesize.ts
git commit -m "feat: render cluster and RAPTOR progress in CLI"
```

---

### Task 9: Write spec document

**Files:**
- Create: `spec/stratified-distillation.md`

Write a human-readable spec explaining:
1. **What stratified distillation is** — multi-signal clustering for knowledge base consolidation
2. **The pipeline** — step by step with why each step exists:
   - Embedding lookup (why: need vector representation for semantic similarity)
   - UMAP reduction (why: HDBSCAN needs low-dim input for density estimation; curse of dimensionality)
   - HDBSCAN clustering (why: finds variable-density clusters without requiring cluster count; noise-aware)
   - FCA validation (why: embeddings miss structured relationships; metadata split catches entity confusion)
   - LLM distillation per cluster (why: coherent groups produce better summaries)
   - RAPTOR recursion (why: distilled facts can themselves form higher-order patterns)
3. **What doesn't happen** — bridge detection (FCA handles it), cross-chunk consolidation (clusters are coherent), forced noise assignment (noise stays for future runs)
4. **Configuration** — the three knobs and their defaults
5. **Key references** — HDBSCAN paper, UMAP paper, BERTopic, RAPTOR

This is NOT a code document. It explains the pipeline to someone who wants to understand the system, not modify it.

**Step 1: Write the spec**

**Step 2: Commit**

```bash
git add -f spec/stratified-distillation.md
git commit -m "spec: stratified distillation pipeline and rationale"
```

---

### Task 10: Final integration test

**Files:**
- Modify: `src/synthesize-e2e.test.ts` (or create if needed)

**Step 1: Read existing e2e test to understand the pattern**

Read `src/synthesize-e2e.test.ts` to see how the existing e2e test sets up git repos, search indices, and LLM mocks.

**Step 2: Add a clustering e2e test**

Test that:
- A recipe with `mode: distill` and `max_depth: 1` correctly clusters facts and produces distilled output
- The LLM receives one call per cluster (not per size-chunk)
- Noise facts are left untouched
- The test needs to mock/stub the Embedder (or use real embeddings if the model is available in CI)

**Step 3: Run full test suite**

Run: `cd src && bun test`
Expected: All PASS

**Step 4: Commit**

```bash
git add src/synthesize-e2e.test.ts
git commit -m "test: e2e test for cluster-based distillation"
```

---

## Task Dependencies

```
Task 1 (deps) ──────────────┐
Task 2 (recipe schema) ─────┤
Task 3 (FCA validation) ────┼──→ Task 4 (full pipeline) ──→ Task 7 (rewrite distill) ──→ Task 8 (CLI) ──→ Task 10 (e2e)
Task 5 (getEmbeddings) ─────┤                                                              │
Task 6 (expose Embedder) ───┘                                                              └──→ Task 9 (spec)
```

Tasks 1-3 and 5-6 can be done in parallel. Task 4 depends on 1 and 3. Task 7 depends on 2, 4, 5, 6. Tasks 8-10 depend on 7.
