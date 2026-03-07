import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo } from "../git";
import { learnHandler } from "./learn";
import { whyHandler } from "./why";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

let testDir: string;
let repo: GitRepo;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-test-"));
  repo = new GitRepo(join(testDir, "repo"), "test-machine");
  await repo.init();

  await learnHandler(repo, {
    moment_name: "alice-music",
    facts: [
      {
        path: "worlds/people/alice/likes-rock.md",
        domain: ["personal", "music"],
        confidence: 0.85,
        sources: 3,
        entities: ["alice", "rock_music"],
        refs: ["episodic://event_88"],
        title: "Alice likes rock music",
        body: "Strong preference for rock.",
      },
      {
        path: "worlds/people/alice/bought-album.md",
        domain: ["personal", "music"],
        confidence: 0.9,
        sources: 1,
        entities: ["alice", "album_x"],
        title: "Alice bought Album X",
        body: "Purchased in 2024.",
      },
    ],
  });
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("knomit_why", () => {
  it("returns fact details, learning moment, and history", async () => {
    const result = await whyHandler(repo, {
      file: "worlds/people/alice/likes-rock.md",
    });

    expect(result.fact.file).toBe("worlds/people/alice/likes-rock.md");
    expect(result.fact.frontmatter.confidence).toBe(0.85);
    expect(result.refs).toContain("episodic://event_88");
    expect(result.history.length).toBeGreaterThan(0);
    expect(result.learning_moment.tag).toBe("learn/alice-music");
  });

  it("finds sibling facts from the same learning moment", async () => {
    const result = await whyHandler(repo, {
      file: "worlds/people/alice/likes-rock.md",
    });

    const siblingFiles = result.learning_moment.siblings.map(s => s.file);
    expect(siblingFiles).toContain("worlds/people/alice/bought-album.md");

    const sibling = result.learning_moment.siblings.find(s => s.file === "worlds/people/alice/bought-album.md");
    expect(sibling?.title).toBe("Alice bought Album X");
    expect(sibling?.commit).toMatch(/^[0-9a-f]+$/);
  });

  it("returns empty result for nonexistent file", async () => {
    const result = await whyHandler(repo, {
      file: "worlds/nonexistent.md",
    });

    expect(result.fact.file).toBe("worlds/nonexistent.md");
    expect(result.history).toEqual([]);
  });
});
