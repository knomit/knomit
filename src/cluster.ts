import type { FactForLLM } from "./synthesize";
import { UMAP } from "umap-js";
import { HDBSCAN } from "hdbscan-ts";

export interface ClusterOptions {
  umapDimensions?: number;    // default 5
  minClusterSize?: number;    // default 3
  minSamples?: number;        // defaults to minClusterSize
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
  const umapDimensions = options?.umapDimensions ?? 5;
  const minClusterSize = options?.minClusterSize ?? 3;
  const minSamples = options?.minSamples ?? minClusterSize;

  const noise: FactForLLM[] = [];
  const withEmbeddings: { fact: FactForLLM; vec: Float32Array }[] = [];

  // 1. Separate facts with/without embeddings
  for (const fact of facts) {
    const vec = embeddings.get(fact.path);
    if (vec) {
      withEmbeddings.push({ fact, vec });
    } else {
      noise.push(fact);
    }
  }

  // 2. If fewer than minClusterSize have embeddings, all go to noise
  if (withEmbeddings.length < minClusterSize) {
    for (const { fact } of withEmbeddings) noise.push(fact);
    return { clusters: new Map(), noise };
  }

  // 3. UMAP: reduce dimensionality
  const count = withEmbeddings.length;
  const data: number[][] = withEmbeddings.map(({ vec }) => Array.from(vec));
  const umap = new UMAP({
    nComponents: umapDimensions,
    nNeighbors: Math.min(15, Math.floor(count / 2)),
    minDist: 0.1,
    spread: 1.0,
  });
  const reduced = umap.fit(data);

  // 4. HDBSCAN: cluster reduced vectors
  const hdbscan = new HDBSCAN({ minClusterSize, minSamples });
  const labels = hdbscan.fit(reduced);

  // 5. Group by cluster label; -1 → noise
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

  // 6. FCA validation: splitByMetadata on each raw cluster
  const clusters = new Map<number, FactForLLM[]>();
  let clusterIdx = 0;
  for (const clusterFacts of rawClusters.values()) {
    const subgroups = splitByMetadata(clusterFacts, minClusterSize);
    for (const group of subgroups) {
      if (group.length >= minClusterSize) {
        clusters.set(clusterIdx++, group);
      } else {
        for (const f of group) noise.push(f);
      }
    }
  }

  // 7. Return
  return { clusters, noise };
}

/**
 * FCA-lite: split a cluster into connected components based on shared
 * domains or entities. Two facts are connected if they share at least
 * one domain tag or one entity tag. Uses Union-Find internally.
 *
 * Returns groups that meet minClusterSize. Smaller groups are discarded
 * (their facts become noise).
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

  // Build inverted indices: tag -> fact indices
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

  // Filter by min size - if only one component, return it regardless
  const groups = [...components.values()];
  if (groups.length === 1) return groups;
  return groups.filter((g) => g.length >= minClusterSize);
}
