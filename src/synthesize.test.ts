import { describe, it, expect } from "bun:test";
import {
  buildPrunePrompt,
  buildDistillPrompt,
  parsePruneResponse,
  parseDistillResponse,
  chunkFacts,
} from "./synthesize";

describe("buildPrunePrompt", () => {
  it("includes facts and recipe prompt", () => {
    const facts = [
      { path: "worlds/test.md", title: "Test", body: "Body", domain: ["d"], entities: ["e"], confidence: 0.8, sources: 1, refs: [] },
    ];
    const prompt = buildPrunePrompt(facts, "Focus on security", "Find stale facts");
    expect(prompt).toContain("Test");
    expect(prompt).toContain("Focus on security");
    expect(prompt).toContain("Find stale facts");
    expect(prompt).toContain("worlds/test.md");
  });
});

describe("parsePruneResponse", () => {
  it("parses valid prune JSON", () => {
    const json = JSON.stringify({
      decisions: [
        { file: "worlds/a.md", action: "forget", reason: "stale" },
        { file: "worlds/b.md", action: "keep", reason: "current" },
      ],
      merges: [],
      summary: "Pruned 1",
    });
    const result = parsePruneResponse(json);
    expect(result.decisions).toHaveLength(2);
    expect(result.decisions[0].action).toBe("forget");
  });

  it("extracts JSON from markdown code blocks", () => {
    const wrapped = '```json\n{"decisions":[],"merges":[],"summary":"ok"}\n```';
    const result = parsePruneResponse(wrapped);
    expect(result.summary).toBe("ok");
  });
});

describe("parseDistillResponse", () => {
  it("parses valid distill JSON", () => {
    const json = JSON.stringify({
      synthesize: [{
        path: "worlds/new.md",
        title: "Pattern",
        body: "Insight",
        domain: ["d"],
        confidence: 0.8,
        entities: ["e"],
        refs: ["worlds/old1.md"],
      }],
      forget: ["worlds/old1.md"],
      summary: "Distilled 1",
    });
    const result = parseDistillResponse(json);
    expect(result.synthesize).toHaveLength(1);
    expect(result.forget).toContain("worlds/old1.md");
  });
});

describe("chunkFacts", () => {
  it("returns one chunk when facts fit", () => {
    const facts = Array.from({ length: 5 }, (_, i) => ({
      path: `worlds/f${i}.md`, title: `F${i}`, body: "short",
      domain: [], entities: [], confidence: 0.8, sources: 1, refs: [],
    }));
    const chunks = chunkFacts(facts, 100_000);
    expect(chunks).toHaveLength(1);
  });

  it("splits into multiple chunks when facts exceed budget", () => {
    const facts = Array.from({ length: 100 }, (_, i) => ({
      path: `worlds/f${i}.md`, title: `Fact ${i}`, body: "x".repeat(1000),
      domain: [], entities: [], confidence: 0.8, sources: 1, refs: [],
    }));
    const chunks = chunkFacts(facts, 10_000);
    expect(chunks.length).toBeGreaterThan(1);
  });
});
