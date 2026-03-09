/**
 * E2E tests for search index: upsert, search, sync, reindex, stats,
 * and synthesis log operations across a live repo.
 */
import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { createTestEnv, destroyTestEnv, type TestEnv } from "./helpers";
import { learnHandler } from "../tools/learn";
import { forgetHandler } from "../tools/forget";
import { updateHandler } from "../tools/update";
import { commitFact } from "../fact-ops";

let env: TestEnv;

beforeEach(async () => {
  env = await createTestEnv();
});

afterEach(async () => {
  await destroyTestEnv(env);
});

// ---------------------------------------------------------------------------
// Search functionality
// ---------------------------------------------------------------------------

describe("search index FTS", () => {
  beforeEach(async () => {
    await learnHandler(env.repo, {
      moment_name: "seed",
      facts: [
        {
          path: "worlds/lang/typescript/generics",
          domain: ["programming", "typescript"],
          confidence: 0.9,
          sources: 2,
          entities: ["typescript"],
          title: "TypeScript generics are powerful",
          body: "Generic types enable reusable, type-safe code. Use constraints with extends.",
        },
        {
          path: "worlds/lang/rust/ownership",
          domain: ["programming", "rust"],
          confidence: 0.95,
          sources: 3,
          entities: ["rust"],
          title: "Rust ownership model",
          body: "Each value has exactly one owner. Ownership can be transferred or borrowed.",
        },
        {
          path: "worlds/tools/git/branching",
          domain: ["tools", "git"],
          confidence: 0.85,
          sources: 1,
          entities: ["git"],
          title: "Git branching strategy",
          body: "Use feature branches, merge to main via PR.",
        },
      ],
    }, env.searchIndex);
  });

  it("finds facts by title keywords", async () => {
    const results = await env.searchIndex.search({ text: "generics" });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0].title).toBe("TypeScript generics are powerful");
  });

  it("finds facts by body keywords", async () => {
    const results = await env.searchIndex.search({ text: "ownership" });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0].title).toBe("Rust ownership model");
  });

  it("filters by domain", async () => {
    const results = await env.searchIndex.search({ domain: ["rust"] });
    expect(results.length).toBe(1);
    expect(results[0].entities).toContain("rust");
  });

  it("filters by entity", async () => {
    const results = await env.searchIndex.search({ entities: ["git"] });
    expect(results.length).toBe(1);
    expect(results[0].title).toBe("Git branching strategy");
  });

  it("filters by path prefix", async () => {
    const results = await env.searchIndex.search({ path: "worlds/lang/" });
    expect(results.length).toBe(2);
  });

  it("filters by min_confidence", async () => {
    const results = await env.searchIndex.search({ min_confidence: 0.9 });
    expect(results.length).toBe(2); // typescript (0.9) and rust (0.95)
  });

  it("combines text + domain filter", async () => {
    const results = await env.searchIndex.search({
      text: "type",
      domain: ["typescript"],
    });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0].domain).toContain("typescript");
  });

  it("returns scores normalized 0-100", async () => {
    const results = await env.searchIndex.search({ text: "generics" });
    for (const r of results) {
      expect(r.score).toBeGreaterThanOrEqual(0);
      expect(r.score).toBeLessThanOrEqual(100);
    }
  });
});

// ---------------------------------------------------------------------------
// Sync and reindex
// ---------------------------------------------------------------------------

describe("search index sync", () => {
  it("syncs new facts committed directly to repo", async () => {
    // Commit a fact directly (bypassing search index)
    await commitFact(env.repo, {
      path: "worlds/test/direct-commit",
      title: "Directly committed fact",
      body: "Not yet in search index.",
      domain: ["testing"],
      confidence: 0.7,
      sources: 1,
      entities: ["direct"],
      refs: [],
    });

    // Sync should pick it up
    const changed = await env.searchIndex.sync(env.repo);
    expect(changed).toBe(true);

    const results = await env.searchIndex.search({ text: "directly committed" });
    expect(results.length).toBeGreaterThanOrEqual(1);
  });

  it("removes deleted facts on sync", async () => {
    await learnHandler(env.repo, {
      moment_name: "to-delete",
      facts: [{
        path: "worlds/test/will-delete",
        domain: ["testing"],
        confidence: 0.5,
        sources: 1,
        entities: [],
        title: "Will be deleted",
        body: "Temporary.",
      }],
    }, env.searchIndex);

    // Delete via forget (which updates index directly)
    await forgetHandler(env.repo, {
      file: "worlds/test/will-delete.md",
      moment_name: "delete-it",
    }, env.searchIndex);

    const results = await env.searchIndex.search({ text: "will be deleted" });
    expect(results.length).toBe(0);
  });

  it("reindex rebuilds from scratch", async () => {
    // Add some facts
    await learnHandler(env.repo, {
      moment_name: "reindex-test",
      facts: [
        {
          path: "worlds/test/reindex-a",
          domain: ["testing"],
          confidence: 0.8,
          sources: 1,
          entities: [],
          title: "Reindex test A",
          body: "First fact for reindex test.",
        },
        {
          path: "worlds/test/reindex-b",
          domain: ["testing"],
          confidence: 0.8,
          sources: 1,
          entities: [],
          title: "Reindex test B",
          body: "Second fact for reindex test.",
        },
      ],
    }, env.searchIndex);

    // Full reindex
    await env.searchIndex.reindex(env.repo);

    // Both facts should still be searchable
    const results = await env.searchIndex.search({ text: "reindex test" });
    expect(results.length).toBe(2);
  });
});

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

describe("search index stats", () => {
  it("returns aggregate statistics", async () => {
    await learnHandler(env.repo, {
      moment_name: "stats-seed",
      facts: [
        {
          path: "worlds/a/fact1",
          domain: ["alpha"],
          confidence: 0.8,
          sources: 1,
          entities: ["entity-x"],
          title: "Fact 1",
          body: "Body 1.",
        },
        {
          path: "worlds/a/fact2",
          domain: ["alpha", "beta"],
          confidence: 0.6,
          sources: 2,
          entities: ["entity-x", "entity-y"],
          title: "Fact 2",
          body: "Body 2.",
        },
        {
          path: "worlds/b/fact3",
          domain: ["beta"],
          confidence: 1.0,
          sources: 1,
          entities: ["entity-y"],
          title: "Fact 3",
          body: "Body 3.",
        },
      ],
    }, env.searchIndex);

    const stats = env.searchIndex.stats();
    // worlds.md counts as a fact too
    expect(stats.totalFacts).toBeGreaterThanOrEqual(3);
    expect(stats.avgConfidence).toBeGreaterThan(0);
    expect(stats.domainCounts.alpha).toBe(2);
    expect(stats.domainCounts.beta).toBe(2);
    expect(stats.entityCounts["entity-x"]).toBe(2);
    expect(stats.entityCounts["entity-y"]).toBe(2);
  });

  it("filters stats by path prefix", async () => {
    await learnHandler(env.repo, {
      moment_name: "path-stats",
      facts: [
        { path: "worlds/x/fact1", domain: ["d1"], confidence: 0.8, sources: 1, entities: [], title: "X1", body: "." },
        { path: "worlds/y/fact1", domain: ["d2"], confidence: 0.9, sources: 1, entities: [], title: "Y1", body: "." },
      ],
    }, env.searchIndex);

    const xStats = env.searchIndex.stats("worlds/x/");
    expect(xStats.totalFacts).toBe(1);
    expect(xStats.domainCounts.d1).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// Synthesis log
// ---------------------------------------------------------------------------

describe("synthesis log", () => {
  it("records and retrieves synthesis runs", () => {
    env.searchIndex.setSynthesisLog("test-recipe", "abc123", 42);

    const log = env.searchIndex.getSynthesisLog("test-recipe");
    expect(log).not.toBeNull();
    expect(log!.lastCommit).toBe("abc123");
    expect(log!.factsProcessed).toBe(42);
    expect(log!.runAt).toBeTruthy();
  });

  it("returns null for unknown recipe", () => {
    const log = env.searchIndex.getSynthesisLog("nonexistent");
    expect(log).toBeNull();
  });

  it("overwrites on repeat runs", () => {
    env.searchIndex.setSynthesisLog("repeat", "commit1", 10);
    env.searchIndex.setSynthesisLog("repeat", "commit2", 20);

    const log = env.searchIndex.getSynthesisLog("repeat");
    expect(log!.lastCommit).toBe("commit2");
    expect(log!.factsProcessed).toBe(20);
  });
});
