/**
 * E2E tests for GitRepo operations: init, commit, tag, branch management,
 * file operations, log/history, diff tracking.
 */
import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { mkdtemp, rm, readdir } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { existsSync } from "node:fs";
import { GitRepo, toMomentTag } from "../git";
import { parseFact } from "../facts";

let testDir: string;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-git-e2e-"));
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

// ---------------------------------------------------------------------------
// Initialization
// ---------------------------------------------------------------------------

describe("GitRepo init", () => {
  it("creates a new repo with main branch and worlds.md", async () => {
    const repoPath = join(testDir, "repo");
    const repo = new GitRepo(repoPath, "my-machine");
    await repo.init();

    expect(existsSync(join(repoPath, ".git"))).toBe(true);
    const branch = await repo.currentBranch();
    expect(branch).toBe("machine/my-machine");

    const worldsMd = await repo.readFile("worlds.md");
    expect(worldsMd).toContain("Knowledge Base");
  });

  it("re-opens existing repo and switches to machine branch", async () => {
    const repoPath = join(testDir, "repo");

    // First init
    const repo1 = new GitRepo(repoPath, "machine-a");
    await repo1.init();

    // Re-open with same machine ID
    const repo2 = new GitRepo(repoPath, "machine-a");
    await repo2.init();

    const branch = await repo2.currentBranch();
    expect(branch).toBe("machine/machine-a");
  });

  it("creates separate machine branches", async () => {
    const repoPath = join(testDir, "repo");

    const repo1 = new GitRepo(repoPath, "machine-a");
    await repo1.init();

    // Commit something on machine-a
    await repo1.commit(
      [{ path: "worlds/test.md", content: "test" }],
      "test commit"
    );

    // Switch to machine-b
    const repo2 = new GitRepo(repoPath, "machine-b");
    await repo2.init();

    const branch = await repo2.currentBranch();
    expect(branch).toBe("machine/machine-b");

    const branches = await repo2.listBranches();
    expect(branches).toContain("machine/machine-a");
    expect(branches).toContain("machine/machine-b");
  });
});

// ---------------------------------------------------------------------------
// Commits
// ---------------------------------------------------------------------------

describe("GitRepo commit", () => {
  it("commits a single file and returns hash", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    const hash = await repo.commit(
      [{ path: "worlds/test/fact.md", content: "Hello" }],
      "test: add fact"
    );

    expect(hash).toBeTruthy();
    expect(hash.length).toBeGreaterThanOrEqual(6);

    const content = await repo.readFile("worlds/test/fact.md");
    expect(content).toBe("Hello");
  });

  it("commits multiple files atomically", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    const hash = await repo.commit(
      [
        { path: "worlds/a.md", content: "A" },
        { path: "worlds/b.md", content: "B" },
        { path: "worlds/deep/nested/c.md", content: "C" },
      ],
      "multi-file commit"
    );

    expect(hash).toBeTruthy();
    expect(await repo.readFile("worlds/a.md")).toBe("A");
    expect(await repo.readFile("worlds/b.md")).toBe("B");
    expect(await repo.readFile("worlds/deep/nested/c.md")).toBe("C");
  });

  it("creates intermediate directories automatically", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit(
      [{ path: "worlds/very/deep/path/fact.md", content: "Deep" }],
      "deep commit"
    );

    expect(await repo.fileExists("worlds/very/deep/path/fact.md")).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

describe("GitRepo tags", () => {
  it("creates a tag and lists it", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([{ path: "worlds/fact.md", content: "." }], "add fact");
    const tagName = await repo.tag("learn/my-moment");

    expect(tagName).toBe("learn/my-moment");
    const tags = await repo.listTags();
    expect(tags).toContain("learn/my-moment");
  });

  it("appends timestamp suffix on tag collision", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([{ path: "worlds/a.md", content: "A" }], "a");
    const tag1 = await repo.tag("learn/dupe");

    await repo.commit([{ path: "worlds/b.md", content: "B" }], "b");
    const tag2 = await repo.tag("learn/dupe");

    expect(tag1).toBe("learn/dupe");
    expect(tag2).toMatch(/^learn\/dupe-\d+$/);
  });

  it("toMomentTag sanitizes special characters", () => {
    expect(toMomentTag("hello world!")).toBe("learn/hello-world-");
    expect(toMomentTag("test/moment")).toBe("learn/test/moment");
    expect(toMomentTag("Special @#$ chars")).toBe("learn/Special-----chars");
  });
});

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

describe("GitRepo deleteFile", () => {
  it("deletes a file and commits", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([{ path: "worlds/del.md", content: "to delete" }], "add");
    expect(await repo.fileExists("worlds/del.md")).toBe(true);

    const hash = await repo.deleteFile("worlds/del.md", "forget: del.md");
    expect(hash).toBeTruthy();
    expect(await repo.fileExists("worlds/del.md")).toBe(false);
  });

  it("throws when deleting non-existent file", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await expect(
      repo.deleteFile("worlds/no-such-file.md", "forget")
    ).rejects.toThrow("File not found");
  });
});

// ---------------------------------------------------------------------------
// Log / History
// ---------------------------------------------------------------------------

describe("GitRepo log", () => {
  it("returns commit history for a file", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([{ path: "worlds/fact.md", content: "v1" }], "learn: v1");
    await repo.commit([{ path: "worlds/fact.md", content: "v2" }], "update: v2");
    await repo.commit([{ path: "worlds/fact.md", content: "v3" }], "update: v3");

    const log = await repo.log("worlds/fact.md");
    expect(log.length).toBe(3);
    // Most recent first
    expect(log[0].message).toBe("update: v3");
    expect(log[2].message).toBe("learn: v1");
  });

  it("associates episode tags with log entries", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([{ path: "worlds/fact.md", content: "v1" }], "learn: fact");
    await repo.tag("learn/my-episode");

    const log = await repo.log("worlds/fact.md");
    expect(log[0].episode).toBe("my-episode");
  });

  it("readFileAtCommit returns historical content", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([{ path: "worlds/fact.md", content: "original" }], "v1");
    const history1 = await repo.log("worlds/fact.md");
    const v1Commit = history1[0].commit;

    await repo.commit([{ path: "worlds/fact.md", content: "updated" }], "v2");

    // Current version
    expect(await repo.readFile("worlds/fact.md")).toBe("updated");

    // Historical version
    expect(await repo.readFileAtCommit("worlds/fact.md", v1Commit)).toBe("original");
  });
});

// ---------------------------------------------------------------------------
// Directory listing
// ---------------------------------------------------------------------------

describe("GitRepo listDir", () => {
  it("lists files and directories", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([
      { path: "worlds/a/fact1.md", content: "1" },
      { path: "worlds/a/fact2.md", content: "2" },
      { path: "worlds/a/sub/nested.md", content: "n" },
    ], "add files");

    const entries = await repo.listDir("worlds/a");
    const names = entries.map(e => e.name);

    expect(names).toContain("fact1.md");
    expect(names).toContain("fact2.md");
    expect(names).toContain("sub");

    const dirs = entries.filter(e => e.isDirectory);
    expect(dirs.map(d => d.name)).toContain("sub");

    const files = entries.filter(e => !e.isDirectory);
    expect(files.length).toBe(2);
  });

  it("excludes hidden files", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    // .git directory should not appear
    const entries = await repo.listDir(".");
    const names = entries.map(e => e.name);
    expect(names).not.toContain(".git");
  });

  it("returns empty for non-existent directory", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    const entries = await repo.listDir("worlds/nonexistent");
    expect(entries).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Grep
// ---------------------------------------------------------------------------

describe("GitRepo grep", () => {
  it("finds files containing a pattern", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([
      { path: "worlds/a.md", content: "---\nentities: [alice]\n---\n# Alice fact" },
      { path: "worlds/b.md", content: "---\nentities: [bob]\n---\n# Bob fact" },
    ], "add facts");

    const aliceFiles = await repo.grep("alice", "worlds/");
    expect(aliceFiles).toContain("worlds/a.md");
    expect(aliceFiles).not.toContain("worlds/b.md");
  });

  it("returns empty for no matches", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    const files = await repo.grep("nonexistent-pattern", "worlds/");
    expect(files).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Path validation
// ---------------------------------------------------------------------------

describe("GitRepo path validation", () => {
  it("rejects paths that escape the repository", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    expect(() => repo.validatePath("../../etc/passwd")).toThrow("Path escapes repository");
  });

  it("allows valid paths", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    // Should not throw
    repo.validatePath("worlds/test/fact.md");
    repo.validatePath("worlds.md");
  });
});

// ---------------------------------------------------------------------------
// Branch management
// ---------------------------------------------------------------------------

describe("GitRepo branches", () => {
  it("creates and switches branches", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.checkoutBranch("feature/test", true);
    expect(await repo.currentBranch()).toBe("feature/test");

    await repo.checkoutBranch("machine/test");
    expect(await repo.currentBranch()).toBe("machine/test");
  });

  it("lists all branches", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.checkoutBranch("feature/a", true);
    await repo.checkoutBranch("feature/b", true);

    const branches = await repo.listBranches();
    expect(branches).toContain("main");
    expect(branches).toContain("machine/test");
    expect(branches).toContain("feature/a");
    expect(branches).toContain("feature/b");
  });

  it("deletes a branch", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.checkoutBranch("temp-branch", true);
    await repo.commit([{ path: "worlds/t.md", content: "t" }], "temp");
    await repo.checkoutBranch("machine/test");

    // Merge first so -d works (branch must be fully merged)
    await repo.mergeBranch("temp-branch");
    await repo.deleteBranch("temp-branch");
    const branches = await repo.listBranches();
    expect(branches).not.toContain("temp-branch");
  });

  it("merges a branch", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    // Create and commit on feature branch
    await repo.checkoutBranch("feature/merge-test", true);
    await repo.commit([{ path: "worlds/merged.md", content: "merged" }], "add merged");

    // Back to machine branch and merge
    await repo.checkoutBranch("machine/test");
    await repo.mergeBranch("feature/merge-test");

    // File should exist on machine branch
    expect(await repo.fileExists("worlds/merged.md")).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Diff tracking
// ---------------------------------------------------------------------------

describe("GitRepo diff", () => {
  it("tracks added, modified, and deleted files", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    // Initial commit
    await repo.commit([
      { path: "worlds/existing.md", content: "original" },
      { path: "worlds/to-delete.md", content: "will go" },
    ], "initial");

    const baseCommit = await repo.headCommit();

    // Add, modify, delete
    await repo.commit([{ path: "worlds/new.md", content: "new" }], "add new");
    await repo.commit([{ path: "worlds/existing.md", content: "modified" }], "modify");
    await repo.deleteFile("worlds/to-delete.md", "delete");

    const diff = await repo.diffFiles(baseCommit);
    expect(diff.added).toContain("worlds/new.md");
    expect(diff.modified).toContain("worlds/existing.md");
    expect(diff.deleted).toContain("worlds/to-delete.md");
  });
});

// ---------------------------------------------------------------------------
// Tags containing / at commit
// ---------------------------------------------------------------------------

describe("GitRepo tag queries", () => {
  it("tagsContaining finds tags that include a commit", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([{ path: "worlds/a.md", content: "a" }], "add a");
    const commit = await repo.headCommit();
    await repo.tag("learn/test-tag");

    const tags = await repo.tagsContaining(commit);
    expect(tags).toContain("learn/test-tag");
  });

  it("commitsBetweenTags returns commits in a moment", async () => {
    const repo = new GitRepo(join(testDir, "repo"), "test");
    await repo.init();

    await repo.commit([{ path: "worlds/a.md", content: "a" }], "learn: A");
    await repo.commit([{ path: "worlds/b.md", content: "b" }], "learn: B");
    await repo.tag("learn/episode-1");

    const commits = await repo.commitsBetweenTags("learn/episode-1");
    expect(commits.length).toBeGreaterThanOrEqual(2);
    const files = commits.map(c => c.file);
    expect(files).toContain("worlds/a.md");
    expect(files).toContain("worlds/b.md");
  });
});
