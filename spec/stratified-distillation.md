# Stratified Distillation

## What It Is

Stratified distillation is a multi-signal clustering pipeline for knowledge base consolidation. It identifies groups of semantically related facts using vector embeddings, dimensionality reduction, and density-based clustering, then uses an LLM to distill each group into higher-order insights.

The goal is to keep a growing knowledge base from becoming an undifferentiated pile of facts. As observations accumulate, patterns emerge — stratified distillation surfaces those patterns and records them as new, higher-order facts.

## The Pipeline

### Step 1: Embedding Lookup

Each fact has a 384-dimensional embedding vector computed from its title, body, entities, and domains using all-MiniLM-L6-v2. These embeddings are pre-computed and stored in the search index.

**Why:** Clustering requires a numeric representation of semantic content. The embedding captures what a fact is "about" in a way that supports distance calculations between facts.

### Step 2: UMAP Dimensionality Reduction (384D → 5D)

Uniform Manifold Approximation and Projection reduces the 384-dimensional embeddings to a configurable low-dimensional space (default 5 dimensions).

**Why:** Density-based clustering fails in high-dimensional spaces. In 384 dimensions, all points are roughly equidistant — the "curse of dimensionality" makes density estimation meaningless. The embeddings must be projected down before clustering can work.

UMAP is chosen over PCA because it preserves local neighborhood structure rather than global variance. Two facts that are nearest neighbors in 384D remain nearest neighbors in 5D. This is exactly what the downstream clustering step needs — it groups points by local density, so preserving local relationships matters more than preserving global distances.

### Step 3: HDBSCAN Clustering

Hierarchical Density-Based Spatial Clustering of Applications with Noise finds variable-density clusters in the UMAP-reduced space. It does not require a pre-specified cluster count. Points that don't belong to any dense region are labeled as noise and excluded from distillation.

**Why:** Unlike k-means, HDBSCAN does not force every point into a cluster. A fact that genuinely has no semantic neighbors is not shoved into an unrelated group — it is left alone. This is critical for distillation quality: a cluster sent to the LLM should contain facts that actually belong together.

HDBSCAN also handles variable-density clusters. A tight cluster of 4 closely related facts and a loose cluster of 20 broadly related facts can coexist. Fixed-radius or fixed-count methods would miss one or the other.

The configurable `min_cluster_size` parameter (default 3) sets the minimum number of facts required to form a cluster. Below this threshold, a group is treated as noise.

### Step 4: FCA Validation (Metadata Split)

For each cluster produced by HDBSCAN, the pipeline checks whether the facts partition into subgroups that share zero domains and zero entities. If disjoint subgroups exist, the cluster is split along those boundaries.

The check uses Union-Find on a graph where facts are connected if they share at least one domain or entity. Connected components that are fully disconnected from each other become separate clusters.

**Why:** Embeddings capture semantic similarity but miss structured relationships. Two facts about completely different entities and domains can embed close together if they use similar language patterns. For example, a fact about "React component lifecycle" and a fact about "cell division lifecycle" might land near each other in embedding space despite having nothing in common.

The metadata split catches these false groupings. If the facts in a cluster partition into subgroups with no shared domains or entities, the cluster was likely a coincidence of language rather than a genuine semantic relationship.

This is a lightweight form of Formal Concept Analysis — using the structured metadata (domains, entities) as a second signal to validate the unstructured signal (embeddings).

### Step 5: LLM Distillation Per Cluster

Each validated cluster is sent to an LLM as a coherent group. The LLM identifies patterns across the facts and produces higher-order insights — new facts that capture what the cluster collectively implies. It also marks original facts that are fully subsumed by the new distilled facts.

**Why:** Because each cluster is semantically coherent, the LLM receives focused context rather than arbitrary chunks of the knowledge base. This produces better distillations than the alternative of splitting facts into fixed-size batches and hoping related facts end up together.

### Step 6: RAPTOR Recursion (Optional)

Distilled facts from Step 5 are re-embedded and fed back through Steps 1–5. This continues up to a configurable `max_depth` (default 1, meaning no recursion). The pipeline stops early if no new clusters form.

**Why:** Distilled facts can themselves form higher-order patterns. A first-pass distillation might produce a fact about "team velocity trends" and another about "hiring patterns." A second pass might cluster those into "organizational scaling dynamics." This is the RAPTOR principle: recursive abstraction builds a tree of increasingly general knowledge.

The default depth of 1 (no recursion) reflects the fact that most knowledge bases don't have enough volume to benefit from multi-level abstraction. The option exists for large, mature bases where it becomes useful.

## What Doesn't Happen

### No Bridge Detection

A separate pass to detect facts that bridge multiple clusters (using full-text search to find cross-cluster references) was considered. FCA validation already handles ambiguous cluster membership by splitting clusters along metadata boundaries. Adding bridge detection increases pipeline complexity with unclear return on investment.

### No Cross-Chunk Consolidation

The previous distillation approach split facts into arbitrary size-based chunks, then needed a second LLM pass to reconcile insights across chunks. Semantic clustering eliminates this problem — each LLM call receives a coherent group, so there is nothing to reconcile.

### No Forced Noise Assignment

HDBSCAN noise points — facts that don't belong to any dense cluster — are left alone. Forcing them into the nearest cluster would degrade distillation quality. These facts will be picked up in future distillation runs as more related facts accumulate and density increases around them.

## Configuration

Three parameters are available at the recipe step level:

| Parameter | Default | Meaning |
|---|---|---|
| `max_depth` | 1 | RAPTOR recursion depth. 1 = single pass, no recursion. |
| `umap_dimensions` | 5 | Target dimensions for UMAP reduction. |
| `min_cluster_size` | 3 | Minimum facts for HDBSCAN to consider a cluster. |

## Key References

- Campello, Moulavi, Sander (2013) — *Density-Based Clustering Based on Hierarchical Density Estimates.* The HDBSCAN algorithm.
- McInnes, Healy, Melville (2018) — *UMAP: Uniform Manifold Approximation and Projection for Dimension Reduction.*
- Grootendorst (2022) — *BERTopic: Neural Topic Modeling with a Class-Based TF-IDF Procedure.* Popularized the embed→UMAP→HDBSCAN pipeline for topic discovery.
- Sarthi, Abdullah, Tuli, Khanna, Goldie, Manning (2024) — *RAPTOR: Recursive Abstractive Processing for Tree-Organized Retrieval.* Recursive clustering and summarization for retrieval-augmented generation.
