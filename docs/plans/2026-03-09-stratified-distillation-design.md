# Stratified Distillation Design

Date: 2026-03-09
Status: approved
Branch: feat/stratified-distillation

## Problem

Current `distill` mode groups facts by scope filters and chunks by size (100KB),
missing cross-path semantic connections. We have richer signals available:
embeddings (384-dim), domains, entities, and FTS — but distill doesn't use them.

## Decision Summary

- **Trigger:** Manual via `knomit synthesize` (existing recipe model)
- **Integration:** Replace `distill` internals with cluster-based grouping (Approach B — separate `cluster.ts` module consumed by `distill`)
- **Dimensionality reduction:** UMAP (configurable target dims, default 5)
- **Clustering:** HDBSCAN with configurable `min_cluster_size` (default 3)
- **Metadata validation:** FCA-lite — split clusters with disjoint domain/entity subgroups
- **Bridge detection:** Skipped — FCA handles ambiguous membership
- **Noise handling:** Leave unclustered facts alone for future runs
- **Recursion:** RAPTOR-style, configurable `max_depth` (default 1)
- **Embeddings required:** `distill` fails if embeddings unavailable (no fallback)

## Architecture

### New module: `src/cluster.ts`

```ts
interface ClusterOptions {
  umapDimensions?: number;      // default 5
  minClusterSize?: number;      // default 3
  minSamples?: number;          // defaults to minClusterSize
}

interface ClusterResult {
  clusters: Map<number, FactForLLM[]>;  // clusterId → facts
  noise: FactForLLM[];                   // unclustered facts (skipped)
}

function clusterFacts(
  facts: FactForLLM[],
  embeddings: Map<string, Float32Array>,  // path → 384-dim vector
  options?: ClusterOptions
): ClusterResult;
```

Internal pipeline:
1. Look up embeddings for each fact (facts without embeddings → noise)
2. UMAP: 384D → target dims
3. HDBSCAN on reduced vectors → cluster labels
4. FCA validation: for each cluster, check if facts partition into subgroups
   sharing zero domains AND zero entities. If so, split. Subgroups below
   `min_cluster_size` become noise.
5. Return clusters and noise

### Changes to `distill` in `src/synthesize.ts`

Current flow:
```
gather by scope → chunk by size → LLM per chunk → cross-chunk consolidation → apply
```

New flow:
```
gather by scope → fetch embeddings → clusterFacts() → LLM distill per cluster → recurse → apply
```

- Chunking replaced by clustering. If a single cluster exceeds context window,
  fall back to size-based chunking within that cluster.
- Cross-chunk consolidation eliminated (clusters are already coherent).
- RAPTOR recursion: after distilling, re-embed new facts, cluster again,
  distill again. Stop at `max_depth` or when no new clusters form.
- Ontology placement unchanged (nearest common ancestor of source paths).

### Recipe schema additions

```yaml
steps:
  - mode: distill
    max_depth: 1          # RAPTOR recursion depth (default 1)
    umap_dimensions: 5    # UMAP target dims (default 5)
    min_cluster_size: 3   # HDBSCAN param (default 3)
```

### FCA validation logic

For each HDBSCAN cluster:
1. Build a "shares at least one domain or entity" graph among facts
2. Find connected components
3. If multiple components exist, split into separate clusters
4. Components below `min_cluster_size` become noise

No lattice computation, no triadic FCA, no merge step.

## Dependencies

| Library | Version | License | Purpose |
|---------|---------|---------|---------|
| umap-js | 1.4.0 | Apache-2.0 | Dimensionality reduction |
| hdbscan-ts | 1.0.17 | MIT | Density-based clustering |

Both pure JS/TS, Bun-compatible.

## File Changes

| File | Change |
|------|--------|
| `src/cluster.ts` | New — `clusterFacts()` + FCA validation |
| `src/cluster.test.ts` | New — unit tests |
| `src/synthesize.ts` | Modify — `distill` uses clustering, RAPTOR recursion, fail without embeddings |
| `src/recipe.ts` | Modify — add optional cluster params to step schema |
| `package.json` | Modify — add `umap-js`, `hdbscan-ts` |
| `spec/stratified-distillation.md` | New — pipeline spec with rationale |

## Testing

- **cluster.ts units:** synthetic embeddings, verify cluster count, noise, FCA splits, min_cluster_size
- **synthesize.ts integration:** mock Embedder + LLM, verify cluster-based LLM calls, ontology placement
- **RAPTOR recursion:** verify max_depth=2 produces two levels, max_depth=1 stops after one
- **Error case:** distill fails with clear message when embeddings unavailable
