/**
 * E2E tests for bootstrap, reset, and CLI argument resolution.
 *
 * Tests the full initialization path (repo creation, search index setup)
 * and the reset path (complete wipe), using env vars and explicit args.
 */
import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { mkdtemp, rm, stat } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { existsSync } from "node:fs";
import { bootstrap, reset, resolveRepo, resolveCacheDir } from "../bootstrap";

let baseDir: string;

beforeEach(async () => {
  baseDir = await mkdtemp(join(tmpdir(), "knomit-bootstrap-e2e-"));
});

afterEach(async () => {
  await rm(baseDir, { recursive: true, force: true });
});

// ---------------------------------------------------------------------------
// bootstrap
// ---------------------------------------------------------------------------

describe("bootstrap", () => {
  it("creates a new repo and search index from scratch", async () => {
    const repoPath = join(baseDir, "repo");
    const cacheDir = join(baseDir, "cache");

    const { repo, searchIndex, agentId } = await bootstrap({
      repo: repoPath,
      cacheDir,
      embeddings: false,
    });

    // Repo exists and is initialized
    expect(existsSync(join(repoPath, ".git"))).toBe(true);

    // Root manifest exists
    const rootExists = await repo.fileExists("know.md");
    expect(rootExists).toBe(true);

    // Machine branch is current
    const branch = await repo.currentBranch();
    expect(branch).toMatch(/^agent\//);
    expect(agentId).toBeTruthy();

    // Search index is functional
    const searchStats = searchIndex.stats();
    expect(searchStats.totalFacts).toBeGreaterThanOrEqual(0);

    // Cache DB file exists
    expect(existsSync(join(cacheDir, "index.db"))).toBe(true);

    searchIndex.close();
  });

  it("re-opens existing repo without reinitializing", async () => {
    const repoPath = join(baseDir, "repo");
    const cacheDir = join(baseDir, "cache");

    // First bootstrap
    const first = await bootstrap({ repo: repoPath, cacheDir, embeddings: false });

    // Learn a fact to prove state persists
    const { learnHandler } = await import("../tools/learn");
    await learnHandler(first.repo, {
      moment_name: "persist-test",
      facts: [{
        path: "know/test/persist",
        domain: ["testing"],
        confidence: 0.8,
        sources: 1,
        entities: [],
        title: "Persistence test",
        body: "Should survive re-bootstrap.",
      }],
    }, first.searchIndex);
    first.searchIndex.close();

    // Second bootstrap — should reuse existing repo
    const second = await bootstrap({ repo: repoPath, cacheDir, embeddings: false });

    const exists = await second.repo.fileExists("know/test/persist.md");
    expect(exists).toBe(true);

    const results = await second.searchIndex.search({ text: "persistence" });
    expect(results.length).toBeGreaterThanOrEqual(1);

    second.searchIndex.close();
  });

  it("uses KNOMIT_AGENT_ID env var for branch name", async () => {
    const repoPath = join(baseDir, "repo");
    const cacheDir = join(baseDir, "cache");
    const oldId = process.env.KNOMIT_AGENT_ID;

    try {
      process.env.KNOMIT_AGENT_ID = "test-custom-agent";
      const { repo, searchIndex } = await bootstrap({
        repo: repoPath,
        cacheDir,
        embeddings: false,
      });

      const branch = await repo.currentBranch();
      expect(branch).toBe("agent/test-custom-agent");
      searchIndex.close();
    } finally {
      if (oldId === undefined) delete process.env.KNOMIT_AGENT_ID;
      else process.env.KNOMIT_AGENT_ID = oldId;
    }
  });
});

// ---------------------------------------------------------------------------
// reset
// ---------------------------------------------------------------------------

describe("reset", () => {
  it("removes both repo and cache directories", async () => {
    const repoPath = join(baseDir, "repo");
    const cacheDir = join(baseDir, "cache");

    // Bootstrap first
    const { searchIndex } = await bootstrap({
      repo: repoPath,
      cacheDir,
      embeddings: false,
    });
    searchIndex.close();

    expect(existsSync(repoPath)).toBe(true);
    expect(existsSync(cacheDir)).toBe(true);

    // Reset
    await reset(repoPath, cacheDir);

    expect(existsSync(repoPath)).toBe(false);
    expect(existsSync(cacheDir)).toBe(false);
  });

  it("handles non-existent directories gracefully", async () => {
    // Should not throw
    await reset(
      join(baseDir, "nonexistent-repo"),
      join(baseDir, "nonexistent-cache"),
    );
  });
});

// ---------------------------------------------------------------------------
// Path resolution
// ---------------------------------------------------------------------------

describe("path resolution", () => {
  it("resolveRepo uses explicit override", () => {
    const path = resolveRepo("/custom/repo/path");
    expect(path).toBe("/custom/repo/path");
  });

  it("resolveRepo uses KNOMIT_REPO env var", () => {
    const old = process.env.KNOMIT_REPO;
    try {
      process.env.KNOMIT_REPO = "/env/repo/path";
      const path = resolveRepo();
      expect(path).toBe("/env/repo/path");
    } finally {
      if (old === undefined) delete process.env.KNOMIT_REPO;
      else process.env.KNOMIT_REPO = old;
    }
  });

  it("resolveCacheDir uses explicit override", () => {
    const path = resolveCacheDir("/custom/cache/path");
    expect(path).toBe("/custom/cache/path");
  });

  it("resolveCacheDir uses KNOMIT_CACHE_DIR env var", () => {
    const old = process.env.KNOMIT_CACHE_DIR;
    try {
      process.env.KNOMIT_CACHE_DIR = "/env/cache/path";
      const path = resolveCacheDir();
      expect(path).toBe("/env/cache/path");
    } finally {
      if (old === undefined) delete process.env.KNOMIT_CACHE_DIR;
      else process.env.KNOMIT_CACHE_DIR = old;
    }
  });
});
