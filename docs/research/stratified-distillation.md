# Stratified Distillation Research

Status: research / brainstorming — not an implementation plan.

## Problem

Current `synthesize` groups facts by ontology path only, missing cross-path
connections and placing results in a meaningless `know/synthesized/` directory.
We have richer signals available: domains, entities, embeddings, and FTS.

## Available Dimensions

| Dimension | Source | Signal type |
|-----------|--------|-------------|
| Ontology path | File hierarchy under `know/` | Structural / hierarchical |
| Domains | YAML frontmatter tags | Categorical / set membership |
| Entities | YAML frontmatter tags | Categorical / set membership |
| Embeddings | Vector similarity (384-dim or model-dependent) | Continuous / semantic |
| FTS | SQLite FTS5 full-text index | Lexical / term overlap |

## Research Findings

### Formal Concept Analysis (FCA)

Objects (facts) × attributes (domains, entities) → concept lattice.
Intersections of attribute sets define formal concepts — natural groupings
where all members share exactly those attributes. Triadic FCA extends to
three dimensions (e.g., facts × domains × entities).

**Fit:** Strong for structured metadata. Produces a lattice of overlapping
groups ordered by generality. Can be computed exactly for small datasets.

### RAPTOR Pattern

Recursive cluster → summarize → re-embed → cluster → summarize.
Builds a bottom-up tree of abstraction levels. Each layer is a distillation
of the layer below.

**Fit:** Good model for recursive roll-up after initial clustering.

### GraphRAG

Hierarchical community detection on entity co-occurrence graphs, followed by
map-reduce LLM summarization over community summaries.

**Fit:** Relevant pattern — entity co-occurrence is computable from our
metadata. Community detection is a form of clustering.

### BERTopic Pipeline

Embed → UMAP → HDBSCAN → class-based TF-IDF → topic labels.
The standard NLP topic modeling pipeline. UMAP handles dimensionality
reduction; HDBSCAN finds variable-density clusters; TF-IDF extracts
representative terms per cluster.

**Fit:** Directly applicable. Our embeddings slot into the first step.

### Rough Sets

Upper/lower approximation of clusters using attribute overlap.
Facts that partially match a cluster belong to the "boundary region."
Useful for deciding when a fact bridges two clusters.

**Fit:** Useful as a refinement step — pruning unproductive groupings.

## Proposed Strategy: Multi-Signal Clustering

### Pipeline

| Pass | Input | Method | Output |
|------|-------|--------|--------|
| 1 | Embeddings | UMAP → HDBSCAN | Semantic clusters + noise points |
| 2 | Clusters + metadata | FCA / set intersections on domains × entities | Validate, split, or merge clusters |
| 3 | Cluster centroids | FTS rare-term overlap | Cross-cluster bridge facts |
| 4 | Validated clusters | LLM summarize per cluster | Distilled facts at ontology common ancestor |
| 5 | Distilled facts | Re-embed → repeat | Higher-order insights (RAPTOR) |

### Why Multi-Signal?

- **Embeddings alone** miss structured relationships (two facts about the same
  entity in different semantic contexts won't cluster).
- **Tags alone** miss semantic similarity (facts with different tags but similar
  meaning won't group).
- **FTS alone** is lexical — synonyms and paraphrases are invisible.
- **Combining all three** catches groupings that any single signal misses,
  and cross-validates to avoid spurious clusters.

### Ontology Placement

Distilled facts go to the nearest common ancestor of their source facts'
ontology paths, not a generic `synthesized/` directory.

## Available Libraries (Pure JS/TS, Bun-compatible)

### hdbscan-ts

- npm: `hdbscan-ts` (v1.0.17, published 2026-02-14)
- Pure TypeScript, ~600 LOC, MIT license
- 12K weekly downloads
- Implements: mutual reachability, MST (Prim's), condensed hierarchy,
  stability-based cluster extraction, membership probabilities
- GitHub: GeLi2001/hdbscan-ts

### umap-js

- npm: `umap-js` (v1.4.0, published 2024-06-05)
- Pure JS/TS by Google PAIR team, Apache-2.0
- 37K weekly downloads
- Implements: full UMAP with sync/async APIs
- Known limitation: random init instead of spectral (fine for small datasets)
- GitHub: PAIR-code/umap-js

### Practical Notes

- At N=hundreds to low thousands, brute-force pairwise distance is fine (O(N²))
- Cosine distance is the right metric for text embeddings
- UMAP reduces 384/1536-dim embeddings to ~10-20 dims where HDBSCAN
  density estimation works reliably
- Without UMAP, HDBSCAN on 384-dim may over-classify as noise due to
  curse of dimensionality

## HDBSCAN Algorithm Summary

For reference — the five stages of the core algorithm:

1. **Core distances**: For each point, distance to k-th nearest neighbor
   (k = min_samples). Local density estimate.

2. **Mutual reachability**: d_mreach(a,b) = max(core(a), core(b), d(a,b)).
   Pushes apart sparse-region points, preserves dense-region distances.

3. **MST**: Minimum spanning tree of the mutual reachability graph.
   Prim's algorithm on dense adjacency matrix, O(N²).

4. **Condensed tree**: Walk single-linkage dendrogram top-down.
   Splits where both children ≥ min_cluster_size are real splits;
   smaller children "fall out" as noise. Track lambda (1/distance)
   at which each point departs its cluster.

5. **Cluster extraction (EOM)**: Bottom-up stability comparison.
   stability(C) = Σ(lambda_p - lambda_birth(C)) over points in C.
   Select cluster or its children based on which has higher total stability.

Key parameters:
- `min_cluster_size` (default 5) — primary tuning knob
- `min_samples` (defaults to min_cluster_size) — density conservatism
- `cluster_selection_method` — "eom" (variable density) or "leaf" (many small)

## Key References

- Campello et al. (2013) "Density-Based Clustering Based on Hierarchical
  Density Estimates" — PAKDD 2013 (original HDBSCAN paper)
- Campello et al. (2015) Extended journal version — ACM TKDD
- McInnes, Healy, Astels (2017) "hdbscan: Hierarchical density based
  clustering" — JOSS (Python library paper)
- McInnes, Healy, Melville (2018) "UMAP: Uniform Manifold Approximation
  and Projection for Dimension Reduction"
- Grootendorst (2022) BERTopic — embedding → UMAP → HDBSCAN pipeline
- Sarthi et al. (2024) RAPTOR — recursive abstractive processing for
  tree-organized retrieval
- Microsoft Research (2024) GraphRAG — hierarchical community detection +
  map-reduce summarization

## Open Questions

- What min_cluster_size works well for knowledge base facts? (Probably 3-5)
- Should UMAP target 2D, 5D, or 10-20D? (Higher = more structure preserved,
  lower = better density estimation. Likely 5-10D.)
- How to weight structured metadata vs embedding similarity when they disagree?
- Should FTS bridging happen before or after LLM distillation?
- How does PCA compare to UMAP as a simpler dimensionality reduction step?
  (PCA is trivial to implement; UMAP preserves local structure better.)
