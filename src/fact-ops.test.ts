import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { GitRepo } from "./git";
import { commitFact, deleteFact, updateFact } from "./fact-ops";

describe("fact-ops", () => {
  let repoPath: string;
  let repo: GitRepo;

  beforeEach(async () => {
    repoPath = await mkdtemp(join(tmpdir(), "factops-"));
    repo = new GitRepo(repoPath, "test-machine");
    await repo.init();
  });

  afterEach(async () => {
    await rm(repoPath, { recursive: true, force: true });
  });

  it("commitFact creates a fact file and commits", async () => {
    const hash = await commitFact(repo, {
      path: "worlds/test/fact1.md",
      title: "Test fact",
      body: "Some body",
      domain: ["testing"],
      confidence: 0.9,
      sources: 1,
      entities: ["test"],
      refs: [],
    });
    expect(hash).toBeTruthy();
    const content = await repo.readFile("worlds/test/fact1.md");
    expect(content).toContain("Test fact");
  });

  it("deleteFact removes a fact file and commits", async () => {
    await commitFact(repo, {
      path: "worlds/test/fact1.md",
      title: "To delete",
      body: "Body",
      domain: [],
      confidence: 0.5,
      sources: 1,
      entities: [],
      refs: [],
    });
    const hash = await deleteFact(repo, "worlds/test/fact1.md", "test-moment");
    expect(hash).toBeTruthy();
    const exists = await repo.fileExists("worlds/test/fact1.md");
    expect(exists).toBe(false);
  });

  it("updateFact modifies frontmatter and commits", async () => {
    await commitFact(repo, {
      path: "worlds/test/fact1.md",
      title: "Original",
      body: "Body",
      domain: ["a"],
      confidence: 0.5,
      sources: 1,
      entities: [],
      refs: [],
    });
    const hash = await updateFact(repo, "worlds/test/fact1.md", {
      confidence: 0.9,
    });
    expect(hash).toBeTruthy();
    const content = await repo.readFile("worlds/test/fact1.md");
    expect(content).toContain("confidence: 0.9");
  });
});
