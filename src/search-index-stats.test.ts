import { test, expect, beforeAll, afterAll } from "bun:test";
import { SearchIndex } from "./search-index";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";

let idx: SearchIndex;
let dir: string;

beforeAll(async () => {
  dir = await mkdtemp(join(tmpdir(), "knomit-stats-"));
  idx = new SearchIndex(dir);
  await idx.init();

  await idx.upsert("know/physics/gravity.md", {
    title: "Gravity", body: "Force of attraction", domain: ["physics"],
    entities: ["gravity"], confidence: 0.9, sources: 2, refs: [], commitHash: "abc",
  });
  await idx.upsert("know/physics/light.md", {
    title: "Light", body: "Electromagnetic radiation", domain: ["physics"],
    entities: ["light", "photon"], confidence: 0.8, sources: 1, refs: [], commitHash: "def",
  });
  await idx.upsert("know/math/algebra.md", {
    title: "Algebra", body: "Study of symbols", domain: ["math"],
    entities: ["algebra"], confidence: 0.7, sources: 3, refs: [], commitHash: "ghi",
  });
});

afterAll(async () => {
  idx.close();
  await rm(dir, { recursive: true });
});

test("stats() returns total count and average confidence", () => {
  const s = idx.stats();
  expect(s.totalFacts).toBe(3);
  expect(s.avgConfidence).toBeCloseTo(0.8, 1);
});

test("stats() returns domain distribution", () => {
  const s = idx.stats();
  expect(s.domainCounts).toEqual({ physics: 2, math: 1 });
});

test("stats() with pathPrefix filters to subtree", () => {
  const s = idx.stats("know/physics");
  expect(s.totalFacts).toBe(2);
  expect(s.domainCounts).toEqual({ physics: 2 });
});
