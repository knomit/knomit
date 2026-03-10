import type { FactForLLM } from "./synthesize";

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
