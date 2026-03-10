import { describe, it, expect } from "bun:test";
import { clusterFacts, splitByMetadata } from "./cluster";
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
      makeFact("c.md", ["cooking"], ["bob"]),
    ];
    const result = splitByMetadata(facts, 2);
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
    expect(result).toHaveLength(1);
  });
});

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
      makeFact("c.md", ["d"], ["e"]),
    ];
    // only a.md has an embedding
    const embeddings = new Map([["a.md", new Float32Array(384)]]);
    const result = clusterFacts(facts, embeddings, { minClusterSize: 2 });
    expect(result.noise.some((f) => f.path === "b.md")).toBe(true);
    expect(result.noise.some((f) => f.path === "c.md")).toBe(true);
  });

  it("clusters similar embeddings together", () => {
    const n = 10;
    const facts: FactForLLM[] = [];
    const embeddings = new Map<string, Float32Array>();

    // Group A: high values in first half of vector
    for (let i = 0; i < n; i++) {
      const path = `worlds/group-a/f${i}.md`;
      facts.push(makeFact(path, ["security"], ["alice"]));
      const vec = new Float32Array(384);
      for (let j = 0; j < 192; j++) vec[j] = 0.8 + Math.random() * 0.1;
      for (let j = 192; j < 384; j++) vec[j] = Math.random() * 0.1;
      embeddings.set(path, vec);
    }
    // Group B: high values in second half of vector
    for (let i = 0; i < n; i++) {
      const path = `worlds/group-b/f${i}.md`;
      facts.push(makeFact(path, ["cooking"], ["bob"]));
      const vec = new Float32Array(384);
      for (let j = 0; j < 192; j++) vec[j] = Math.random() * 0.1;
      for (let j = 192; j < 384; j++) vec[j] = 0.8 + Math.random() * 0.1;
      embeddings.set(path, vec);
    }

    const result = clusterFacts(facts, embeddings, { minClusterSize: 3, umapDimensions: 5 });
    expect(result.clusters.size).toBeGreaterThanOrEqual(2);
  });
});
