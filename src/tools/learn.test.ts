import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo } from "../git";
import { learnHandler } from "./learn";
import { parseFact } from "../facts";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

let testDir: string;
let repo: GitRepo;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-test-"));
  repo = new GitRepo(join(testDir, "repo"), "test-agent");
  await repo.init();
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("knomit_learn", () => {
  it("creates fact files, commits, and tags", async () => {
    const result = await learnHandler(repo, {
      moment_name: "alice-music-2025",
      facts: [
        {
          path: "know/people/alice/alice-likes-rock.md",
          domain: ["personal", "music"],
          confidence: 0.85,
          sources: 3,
          entities: ["alice", "rock_music"],
          title: "Alice likes rock music",
          body: "She has a strong preference for rock.",
        },
      ],
    });

    expect(result.moment_tag).toBe("learn/alice-music-2025");
    expect(result.commits.length).toBe(1);
    expect(result.commits[0].file).toBe("know/people/alice/alice-likes-rock.md");

    // Verify file exists and parses correctly
    const content = await repo.readFile("know/people/alice/alice-likes-rock.md");
    const parsed = parseFact(content);
    expect(parsed.frontmatter.confidence).toBe(0.85);
    expect(parsed.title).toBe("Alice likes rock music");

    // Verify tag exists
    const tags = await repo.listTags();
    expect(tags).toContain("learn/alice-music-2025");
  });

  it("creates multiple facts with one commit each", async () => {
    const result = await learnHandler(repo, {
      moment_name: "multi-facts",
      facts: [
        {
          path: "know/a.md",
          domain: ["test"],
          confidence: 0.5,
          sources: 1,
          entities: ["a"],
          title: "Fact A",
          body: "Body A.",
        },
        {
          path: "know/b.md",
          domain: ["test"],
          confidence: 0.6,
          sources: 1,
          entities: ["b"],
          title: "Fact B",
          body: "Body B.",
        },
      ],
    });

    expect(result.commits.length).toBe(2);
    expect(result.commits[0].hash).not.toBe(result.commits[1].hash);
  });
});
