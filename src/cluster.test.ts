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
