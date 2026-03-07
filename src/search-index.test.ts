import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { SearchIndex } from "./search-index";
import { GitRepo } from "./git";
import { learnHandler } from "./tools/learn";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

let cacheDir: string;
let index: SearchIndex;

beforeEach(async () => {
  cacheDir = await mkdtemp(join(tmpdir(), "knomit-idx-"));
  index = new SearchIndex(cacheDir);
});

afterEach(async () => {
  index.close();
  await rm(cacheDir, { recursive: true, force: true });
});

describe("SearchIndex init", () => {
  it("creates the database and tables", async () => {
    await index.init();
    // Verify tables exist by querying sqlite_master
    const tables = index.tableNames();
    expect(tables).toContain("facts");
    expect(tables).toContain("facts_fts");
    expect(tables).toContain("meta");
  });

  it("is idempotent — calling init twice does not error", async () => {
    await index.init();
    await index.init();
    const tables = index.tableNames();
    expect(tables).toContain("facts");
  });
});

describe("SearchIndex upsert/remove", () => {
  it("upserts a fact into the index", async () => {
    await index.init();
    index.upsert("worlds/test/fact.md", {
      title: "Test fact",
      body: "This is a test body.",
      domain: ["testing"],
      entities: ["jest", "bun"],
      confidence: 0.9,
      sources: 2,
      refs: [],
      commitHash: "abc123",
    });

    const row = index.getFact("worlds/test/fact.md");
    expect(row).not.toBeNull();
    expect(row!.title).toBe("Test fact");
    expect(row!.confidence).toBe(0.9);
  });

  it("updates an existing fact on re-upsert", async () => {
    await index.init();
    index.upsert("worlds/test/fact.md", {
      title: "Original",
      body: "Body.",
      domain: ["a"],
      entities: ["x"],
      confidence: 0.5,
      sources: 1,
      refs: [],
      commitHash: "aaa",
    });
    index.upsert("worlds/test/fact.md", {
      title: "Updated",
      body: "New body.",
      domain: ["b"],
      entities: ["y"],
      confidence: 0.8,
      sources: 2,
      refs: ["ref1"],
      commitHash: "bbb",
    });

    const row = index.getFact("worlds/test/fact.md");
    expect(row!.title).toBe("Updated");
    expect(row!.confidence).toBe(0.8);
  });

  it("removes a fact from the index", async () => {
    await index.init();
    index.upsert("worlds/test/fact.md", {
      title: "To remove",
      body: "Body.",
      domain: ["a"],
      entities: ["x"],
      confidence: 0.5,
      sources: 1,
      refs: [],
      commitHash: "aaa",
    });
    index.remove("worlds/test/fact.md");

    const row = index.getFact("worlds/test/fact.md");
    expect(row).toBeNull();
  });
});

describe("SearchIndex FTS search", () => {
  beforeEach(async () => {
    await index.init();
    index.upsert("worlds/people/alice.md", {
      title: "Alice likes rock music",
      body: "Strong preference for rock and alternative genres.",
      domain: ["personal", "music"],
      entities: ["alice", "rock_music"],
      confidence: 0.85,
      sources: 3,
      refs: [],
      commitHash: "aaa",
    });
    index.upsert("worlds/people/bob.md", {
      title: "Bob likes jazz",
      body: "Bob listens to jazz occasionally.",
      domain: ["personal", "music"],
      entities: ["bob", "jazz"],
      confidence: 0.6,
      sources: 1,
      refs: [],
      commitHash: "bbb",
    });
    index.upsert("worlds/tech/bun.md", {
      title: "Bun is fast",
      body: "Bun runtime is significantly faster than Node.",
      domain: ["tech", "runtime"],
      entities: ["bun", "javascript"],
      confidence: 0.95,
      sources: 5,
      refs: [],
      commitHash: "ccc",
    });
  });

  it("searches by free text", async () => {
    const results = await index.search({ text: "rock music" });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0].path).toBe("worlds/people/alice.md");
  });

  it("searches by free text and filters by domain", async () => {
    const results = await index.search({ text: "fast", domain: ["tech"] });
    expect(results.length).toBe(1);
    expect(results[0].path).toBe("worlds/tech/bun.md");
  });

  it("filters by entity without text", async () => {
    const results = await index.search({ entities: ["bob"] });
    expect(results.length).toBe(1);
    expect(results[0].path).toBe("worlds/people/bob.md");
  });

  it("filters by path prefix", async () => {
    const results = await index.search({ path: "worlds/tech" });
    expect(results.length).toBe(1);
  });

  it("filters by min_confidence", async () => {
    const results = await index.search({ domain: ["music"], min_confidence: 0.7 });
    expect(results.length).toBe(1);
    expect(results[0].path).toBe("worlds/people/alice.md");
  });

  it("returns empty array for no matches", async () => {
    const results = await index.search({ text: "nonexistent" });
    expect(results.length).toBe(0);
  });

  it("limits results", async () => {
    const results = await index.search({ text: "music", limit: 1 });
    expect(results.length).toBe(1);
  });
});

describe("SearchIndex sync", () => {
  let testDir: string;
  let repo: GitRepo;

  beforeEach(async () => {
    testDir = await mkdtemp(join(tmpdir(), "knomit-sync-"));
    repo = new GitRepo(join(testDir, "repo"), "test-machine");
    await repo.init();
  });

  afterEach(async () => {
    await rm(testDir, { recursive: true, force: true });
  });

  it("rebuilds index from git repo", async () => {
    await learnHandler(repo, {
      moment_name: "seed",
      facts: [
        {
          path: "worlds/test/fact.md",
          domain: ["test"],
          confidence: 0.9,
          sources: 1,
          entities: ["x"],
          title: "Test fact",
          body: "Test body.",
        },
      ],
    });

    await index.init();
    await index.rebuild(repo);

    const results = await index.search({ text: "test" });
    expect(results.length).toBe(1);
    expect(results[0].title).toBe("Test fact");
  });

  it("sync detects new commits and indexes them", async () => {
    await index.init();
    await index.rebuild(repo);

    // Add a new fact after initial rebuild
    await learnHandler(repo, {
      moment_name: "new-fact",
      facts: [
        {
          path: "worlds/new/fact.md",
          domain: ["new"],
          confidence: 0.7,
          sources: 1,
          entities: ["y"],
          title: "New fact",
          body: "Added after rebuild.",
        },
      ],
    });

    const synced = await index.sync(repo);
    expect(synced).toBe(true);

    const results = await index.search({ text: "new" });
    expect(results.length).toBeGreaterThanOrEqual(1);
  });

  it("sync is a no-op when HEAD unchanged", async () => {
    await index.init();
    await index.rebuild(repo);
    const synced = await index.sync(repo);
    expect(synced).toBe(false);
  });
});

describe("SearchIndex with embeddings", () => {
  it.skipIf(!process.env.KNOMIT_EMBEDDINGS)("vector search finds semantically similar facts", async () => {
    // This test requires KNOMIT_EMBEDDINGS=1 and downloaded assets
    const embeddedIndex = new SearchIndex(cacheDir, { embeddings: true });
    await embeddedIndex.init();

    await embeddedIndex.upsert("worlds/drinks/coffee.md", {
      title: "Enjoys flat white every morning",
      body: "A strong coffee preference, especially flat whites from local cafes.",
      domain: ["preferences"],
      entities: ["coffee", "flat_white"],
      confidence: 0.9,
      sources: 2,
      refs: [],
      commitHash: "aaa",
    });

    // Semantic search — "what does the user like to drink" should find coffee
    const results = await embeddedIndex.search({ text: "what does the user like to drink" });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0]!.path).toBe("worlds/drinks/coffee.md");

    embeddedIndex.close();
  });
});
