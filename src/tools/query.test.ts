import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo } from "../git";
import { SearchIndex } from "../search-index";
import { learnHandler } from "./learn";
import { queryHandler } from "./query";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

let testDir: string;
let repo: GitRepo;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-test-"));
  repo = new GitRepo(join(testDir, "repo"), "test-machine");
  await repo.init();

  // Seed with test facts
  await learnHandler(repo, {
    moment_name: "seed",
    facts: [
      {
        path: "worlds/people/alice/likes-rock.md",
        domain: ["personal", "music"],
        confidence: 0.85,
        sources: 3,
        entities: ["alice", "rock_music"],
        title: "Alice likes rock music",
        body: "Strong preference for rock.",
      },
      {
        path: "worlds/people/bob/likes-jazz.md",
        domain: ["personal", "music"],
        confidence: 0.6,
        sources: 1,
        entities: ["bob", "jazz"],
        title: "Bob likes jazz",
        body: "Bob listens to jazz occasionally.",
      },
      {
        path: "worlds/tech/bun-is-fast.md",
        domain: ["tech", "runtime"],
        confidence: 0.95,
        sources: 5,
        entities: ["bun", "javascript"],
        title: "Bun is fast",
        body: "Bun runtime is significantly faster than Node.",
      },
    ],
  });
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("knomit_query", () => {
  it("finds facts by entity", async () => {
    const result = await queryHandler(repo, { entities: ["alice"] });
    expect(result.facts.length).toBe(1);
    expect(result.facts[0].file).toContain("alice");
  });

  it("finds facts by domain", async () => {
    const result = await queryHandler(repo, { domain: ["music"] });
    expect(result.facts.length).toBe(2);
  });

  it("finds facts by path prefix", async () => {
    const result = await queryHandler(repo, { path: "worlds/tech" });
    expect(result.facts.length).toBe(1);
    expect(result.facts[0].frontmatter.entities).toContain("bun");
  });

  it("filters by min_confidence", async () => {
    const result = await queryHandler(repo, {
      domain: ["music"],
      min_confidence: 0.7,
    });
    expect(result.facts.length).toBe(1);
    expect(result.facts[0].frontmatter.entities).toContain("alice");
  });

  it("returns empty for no matches", async () => {
    const result = await queryHandler(repo, { entities: ["nobody"] });
    expect(result.facts.length).toBe(0);
  });
});

describe("knomit_query with search index", () => {
  let idxDir: string;
  let searchIndex: SearchIndex;

  beforeEach(async () => {
    idxDir = await mkdtemp(join(tmpdir(), "knomit-idx-"));
    searchIndex = new SearchIndex(idxDir);
    await searchIndex.init();
    await searchIndex.rebuild(repo);
  });

  afterEach(async () => {
    searchIndex.close();
    await rm(idxDir, { recursive: true, force: true });
  });

  it("finds facts by text search", async () => {
    const result = await queryHandler(repo, { text: "rock music" }, searchIndex);
    expect(result.facts.length).toBeGreaterThanOrEqual(1);
    expect(result.facts[0].title).toContain("Alice");
  });

  it("text search still applies entity filter", async () => {
    const result = await queryHandler(repo, { text: "music", entities: ["bob"] }, searchIndex);
    expect(result.facts.length).toBe(1);
    expect(result.facts[0].title).toContain("Bob");
  });

  it("works without text (backward compatible)", async () => {
    const result = await queryHandler(repo, { entities: ["alice"] }, searchIndex);
    expect(result.facts.length).toBe(1);
  });
});
