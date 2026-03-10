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
          path: "know/lang/typescript/generics",
          domain: ["programming", "typescript"],
          confidence: 0.9,
          sources: 2,
          entities: ["typescript"],
          title: "TypeScript generics are powerful",
          body: "Generic types enable reusable, type-safe code. Use constraints with extends.",
        },
        {
          path: "know/lang/rust/ownership",
          domain: ["programming", "rust"],
          confidence: 0.95,
          sources: 3,
          entities: ["rust"],
          title: "Rust ownership model",
          body: "Each value has exactly one owner. Ownership can be transferred or borrowed.",
        },
        {
          path: "know/tools/git/branching",
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
    const results = await env.searchIndex.search({ path: "know/lang/" });
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
      path: "know/test/direct-commit",
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
        path: "know/test/will-delete",
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
      file: "know/test/will-delete.md",
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
          path: "know/test/reindex-a",
          domain: ["testing"],
          confidence: 0.8,
          sources: 1,
          entities: [],
          title: "Reindex test A",
          body: "First fact for reindex test.",
        },
        {
          path: "know/test/reindex-b",
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
          path: "know/a/fact1",
          domain: ["alpha"],
          confidence: 0.8,
          sources: 1,
          entities: ["entity-x"],
          title: "Fact 1",
          body: "Body 1.",
        },
        {
          path: "know/a/fact2",
          domain: ["alpha", "beta"],
          confidence: 0.6,
          sources: 2,
          entities: ["entity-x", "entity-y"],
          title: "Fact 2",
          body: "Body 2.",
        },
        {
          path: "know/b/fact3",
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
    // know.md counts as a fact too
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
        { path: "know/x/fact1", domain: ["d1"], confidence: 0.8, sources: 1, entities: [], title: "X1", body: "." },
        { path: "know/y/fact1", domain: ["d2"], confidence: 0.9, sources: 1, entities: [], title: "Y1", body: "." },
      ],
    }, env.searchIndex);

    const xStats = env.searchIndex.stats("know/x/");
    expect(xStats.totalFacts).toBe(1);
    expect(xStats.domainCounts.d1).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// Cache–git sync invariants
// ---------------------------------------------------------------------------

describe("search index cache-git sync", () => {
  it("commit_hash stores the actual last commit for each file, not HEAD", async () => {
    // Create two facts in separate commits
    await learnHandler(env.repo, {
      moment_name: "first",
      facts: [{
        path: "know/sync/fact-a",
        domain: ["testing"],
        confidence: 0.8,
        sources: 1,
        entities: [],
        title: "Fact A",
        body: "First fact.",
      }],
    }, env.searchIndex);
    const commitA = await env.repo.lastCommitForFile("know/sync/fact-a.md");

    await learnHandler(env.repo, {
      moment_name: "second",
      facts: [{
        path: "know/sync/fact-b",
        domain: ["testing"],
        confidence: 0.8,
        sources: 1,
        entities: [],
        title: "Fact B",
        body: "Second fact.",
      }],
    }, env.searchIndex);
    const commitB = await env.repo.lastCommitForFile("know/sync/fact-b.md");

    // commitA and commitB should be different commits
    expect(commitA).not.toBe(commitB);

    // After rebuild, each fact should retain its own commit hash
    await env.searchIndex.rebuild(env.repo);

    const resultsA = await env.searchIndex.search({ text: "Fact A" });
    expect(resultsA.length).toBeGreaterThanOrEqual(1);
    expect(resultsA[0].commitHash).toBe(commitA);

    const resultsB = await env.searchIndex.search({ text: "Fact B" });
    expect(resultsB.length).toBeGreaterThanOrEqual(1);
    expect(resultsB[0].commitHash).toBe(commitB);
  });

  it("commit_hash is accurate after sync (not just HEAD)", async () => {
    // Create initial fact and sync
    await commitFact(env.repo, {
      path: "know/sync/early",
      title: "Early fact",
      body: "Created first.",
      domain: ["testing"],
      confidence: 0.7,
      sources: 1,
      entities: ["early"],
      refs: [],
    });
    const earlyCommit = await env.repo.lastCommitForFile("know/sync/early.md");

    // Create another fact (advances HEAD)
    await commitFact(env.repo, {
      path: "know/sync/late",
      title: "Late fact",
      body: "Created second.",
      domain: ["testing"],
      confidence: 0.7,
      sources: 1,
      entities: ["late"],
      refs: [],
    });

    // Sync picks up both — early fact should have its own commit, not HEAD
    await env.searchIndex.sync(env.repo);

    const results = await env.searchIndex.search({ entities: ["early"] });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0].commitHash).toBe(earlyCommit);
  });

  it("reindex preserves per-file commit hashes", async () => {
    await learnHandler(env.repo, {
      moment_name: "reindex-hash",
      facts: [{
        path: "know/sync/reindex-target",
        domain: ["testing"],
        confidence: 0.9,
        sources: 1,
        entities: ["reindex"],
        title: "Reindex target",
        body: "Should keep its commit hash after reindex.",
      }],
    }, env.searchIndex);
    const originalCommit = await env.repo.lastCommitForFile("know/sync/reindex-target.md");

    // Create more commits to advance HEAD
    await commitFact(env.repo, {
      path: "know/sync/filler",
      title: "Filler",
      body: "Advances HEAD.",
      domain: ["testing"],
      confidence: 0.5,
      sources: 1,
      entities: [],
      refs: [],
    });

    await env.searchIndex.reindex(env.repo);

    const results = await env.searchIndex.search({ entities: ["reindex"] });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0].commitHash).toBe(originalCommit);
  });

  it("remove cleans up deleted facts from search results", async () => {
    await learnHandler(env.repo, {
      moment_name: "remove-sync",
      facts: [{
        path: "know/sync/to-remove",
        domain: ["testing"],
        confidence: 0.5,
        sources: 1,
        entities: ["removable"],
        title: "Removable fact",
        body: "Will be removed.",
      }],
    }, env.searchIndex);

    // Verify it exists
    const before = await env.searchIndex.search({ entities: ["removable"] });
    expect(before.length).toBe(1);

    // Delete via forget and sync
    await forgetHandler(env.repo, {
      file: "know/sync/to-remove.md",
      moment_name: "cleanup",
    }, env.searchIndex);

    const after = await env.searchIndex.search({ entities: ["removable"] });
    expect(after.length).toBe(0);

    // Also gone from FTS
    const ftsAfter = await env.searchIndex.search({ text: "removable" });
    expect(ftsAfter.length).toBe(0);
  });

  it("sync removes facts deleted directly in git", async () => {
    // Commit a fact and sync so the index knows about it
    await commitFact(env.repo, {
      path: "know/sync/direct-delete",
      title: "Direct delete target",
      body: "Will be deleted via git.",
      domain: ["testing"],
      confidence: 0.6,
      sources: 1,
      entities: ["direct-del"],
      refs: [],
    });
    await env.searchIndex.sync(env.repo);

    // Verify it's indexed
    const before = await env.searchIndex.search({ entities: ["direct-del"] });
    expect(before.length).toBe(1);

    // Delete the file directly in git (bypassing search index)
    await env.repo.deleteFile("know/sync/direct-delete.md", "remove fact directly");

    // Sync should detect the deletion
    const changed = await env.searchIndex.sync(env.repo);
    expect(changed).toBe(true);

    const results = await env.searchIndex.search({ entities: ["direct-del"] });
    expect(results.length).toBe(0);
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
