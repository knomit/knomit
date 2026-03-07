import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo } from "./git";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

let testDir: string;
let repo: GitRepo;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-test-"));
  repo = new GitRepo(join(testDir, "repo"), "test-machine");
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("GitRepo.init", () => {
  it("creates a new repo with worlds.md and machine branch", async () => {
    await repo.init();

    // Should be on machine branch
    const branch = await repo.currentBranch();
    expect(branch).toBe("machine/test-machine");

    // worlds.md should exist
    const content = await repo.readFile("worlds.md");
    expect(content).toContain("Knowledge Base");
  });

  it("is idempotent — calling init twice doesn't error", async () => {
    await repo.init();
    await repo.init();
    const branch = await repo.currentBranch();
    expect(branch).toBe("machine/test-machine");
  });
});

describe("GitRepo.commit", () => {
  it("commits a file and returns the hash", async () => {
    await repo.init();
    const hash = await repo.commit(
      [{ path: "worlds/test.md", content: "# Test\n\nHello." }],
      "add test fact"
    );
    expect(hash).toMatch(/^[0-9a-f]{7,40}$/);

    const content = await repo.readFile("worlds/test.md");
    expect(content).toContain("Hello.");
  });
});

describe("GitRepo.tag", () => {
  it("creates a tag on the current HEAD", async () => {
    await repo.init();
    await repo.commit(
      [{ path: "worlds/test.md", content: "test" }],
      "test commit"
    );
    await repo.tag("learn/test-moment");

    const tags = await repo.listTags();
    expect(tags).toContain("learn/test-moment");
  });

  it("appends timestamp if tag exists", async () => {
    await repo.init();
    await repo.commit([{ path: "worlds/a.md", content: "a" }], "first");
    await repo.tag("learn/dupe");
    await repo.commit([{ path: "worlds/b.md", content: "b" }], "second");
    await repo.tag("learn/dupe");

    const tags = await repo.listTags();
    expect(tags.filter(t => t.startsWith("learn/dupe")).length).toBe(2);
  });
});

describe("GitRepo.log", () => {
  it("returns commit history for a file", async () => {
    await repo.init();
    await repo.commit(
      [{ path: "worlds/evolving.md", content: "v1" }],
      "version 1"
    );
    await repo.commit(
      [{ path: "worlds/evolving.md", content: "v2" }],
      "version 2"
    );

    const history = await repo.log("worlds/evolving.md");
    expect(history.length).toBe(2);
    expect(history[0].message).toBe("version 2");
    expect(history[1].message).toBe("version 1");
  });
});

describe("GitRepo.listDir", () => {
  it("lists directory contents", async () => {
    await repo.init();
    await repo.commit(
      [
        { path: "worlds/people/alice.md", content: "alice manifest" },
        { path: "worlds/people/alice/likes-rock.md", content: "rock" },
      ],
      "add alice"
    );

    const entries = await repo.listDir("worlds/people");
    expect(entries.map(e => e.name)).toContain("alice.md");
    expect(entries.map(e => e.name)).toContain("alice");
  });
});

describe("GitRepo.grep", () => {
  it("finds files matching a pattern in frontmatter", async () => {
    await repo.init();
    await repo.commit(
      [
        {
          path: "worlds/a.md",
          content: '---\nentities: [alice, bob]\n---\n# A\n\nBody.',
        },
        {
          path: "worlds/b.md",
          content: '---\nentities: [charlie]\n---\n# B\n\nBody.',
        },
      ],
      "add facts"
    );

    const matches = await repo.grep("alice");
    expect(matches).toContain("worlds/a.md");
    expect(matches).not.toContain("worlds/b.md");
  });
});
