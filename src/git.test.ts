import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo, vendoredGitEnv } from "./git";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

describe("vendoredGitEnv", () => {
  it("returns env vars when bin is under vendor/git/", () => {
    const env = vendoredGitEnv("/app/vendor/git/bin/git");
    expect(env).toEqual({
      GIT_EXEC_PATH: "/app/vendor/git/libexec/git-core",
      GIT_TEMPLATE_DIR: "/app/vendor/git/share/git-core/templates",
      GIT_SSL_CAINFO: "/app/vendor/git/ssl/cacert.pem",
    });
  });

  it("returns null when bin is system git", () => {
    const env = vendoredGitEnv("/usr/bin/git");
    expect(env).toBeNull();
  });
});

let testDir: string;
let repo: GitRepo;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-test-"));
  repo = new GitRepo(join(testDir, "repo"), "test-agent");
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("GitRepo.init", () => {
  it("creates a new repo with know.md and agent branch", async () => {
    await repo.init();

    // Should be on agent branch
    const branch = await repo.currentBranch();
    expect(branch).toBe("agent/test-agent");

    // know.md should exist
    const content = await repo.readFile("know.md");
    expect(content).toContain("Knowledge Base");
  });

  it("is idempotent — calling init twice doesn't error", async () => {
    await repo.init();
    await repo.init();
    const branch = await repo.currentBranch();
    expect(branch).toBe("agent/test-agent");
  });
});

describe("GitRepo.commit", () => {
  it("commits a file and returns the hash", async () => {
    await repo.init();
    const hash = await repo.commit(
      [{ path: "know/test.md", content: "# Test\n\nHello." }],
      "add test fact"
    );
    expect(hash).toMatch(/^[0-9a-f]{7,40}$/);

    const content = await repo.readFile("know/test.md");
    expect(content).toContain("Hello.");
  });
});

describe("GitRepo.tag", () => {
  it("creates a tag on the current HEAD", async () => {
    await repo.init();
    await repo.commit(
      [{ path: "know/test.md", content: "test" }],
      "test commit"
    );
    await repo.tag("learn/test-moment");

    const tags = await repo.listTags();
    expect(tags).toContain("learn/test-moment");
  });

  it("appends timestamp if tag exists", async () => {
    await repo.init();
    await repo.commit([{ path: "know/a.md", content: "a" }], "first");
    await repo.tag("learn/dupe");
    await repo.commit([{ path: "know/b.md", content: "b" }], "second");
    await repo.tag("learn/dupe");

    const tags = await repo.listTags();
    expect(tags.filter(t => t.startsWith("learn/dupe")).length).toBe(2);
  });
});

describe("GitRepo.log", () => {
  it("returns commit history for a file", async () => {
    await repo.init();
    await repo.commit(
      [{ path: "know/evolving.md", content: "v1" }],
      "version 1"
    );
    await repo.commit(
      [{ path: "know/evolving.md", content: "v2" }],
      "version 2"
    );

    const history = await repo.log("know/evolving.md");
    expect(history.length).toBe(2);
    expect(history[0].message).toBe("version 2");
    expect(history[1].message).toBe("version 1");
  });

  it("fromCommit scopes log to start from a specific commit", async () => {
    // Regression: repo.log() ran git log from HEAD, which could miss the
    // target commit when navigating from changed-file items in history view.
    await repo.init();
    const c1 = await repo.commit(
      [{ path: "know/file.md", content: "v1" }],
      "first"
    );
    const c2 = await repo.commit(
      [{ path: "know/file.md", content: "v2" }],
      "second"
    );
    await repo.commit(
      [{ path: "know/file.md", content: "v3" }],
      "third"
    );

    // Without fromCommit: see all 3
    const all = await repo.log("know/file.md");
    expect(all.length).toBe(3);

    // With fromCommit=c2: only see c2 and c1 (not c3)
    const fromC2 = await repo.log("know/file.md", c2);
    expect(fromC2.length).toBe(2);
    expect(fromC2[0].message).toBe("second");
    expect(fromC2[1].message).toBe("first");
  });
});

describe("GitRepo.listDir", () => {
  it("lists directory contents", async () => {
    await repo.init();
    await repo.commit(
      [
        { path: "know/people/alice.md", content: "alice manifest" },
        { path: "know/people/alice/likes-rock.md", content: "rock" },
      ],
      "add alice"
    );

    const entries = await repo.listDir("know/people");
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
          path: "know/a.md",
          content: '---\nentities: [alice, bob]\n---\n# A\n\nBody.',
        },
        {
          path: "know/b.md",
          content: '---\nentities: [charlie]\n---\n# B\n\nBody.',
        },
      ],
      "add facts"
    );

    const matches = await repo.grep("alice");
    expect(matches).toContain("know/a.md");
    expect(matches).not.toContain("know/b.md");
  });
});
