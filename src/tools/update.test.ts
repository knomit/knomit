import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo } from "../git";
import { learnHandler } from "./learn";
import { updateHandler } from "./update";
import { parseFact } from "../facts";
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
    moment_name: "seed",
    facts: [
      {
        path: "worlds/people/alice/likes-rock.md",
        domain: ["personal", "music"],
        confidence: 0.5,
        sources: 1,
        entities: ["alice", "rock_music"],
        title: "Alice likes rock music",
        body: "Initial observation.",
      },
    ],
  });
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("knomit_update", () => {
  it("updates confidence and sources", async () => {
    const result = await updateHandler(repo, {
      file: "worlds/people/alice/likes-rock.md",
      moment_name: "alice-reinforced",
      updates: { confidence: 0.85, sources: 3 },
    });

    expect(result.commit).toMatch(/^[0-9a-f]+$/);
    expect(result.moment_tag).toBe("learn/alice-reinforced");

    const content = await repo.readFile("worlds/people/alice/likes-rock.md");
    const parsed = parseFact(content);
    expect(parsed.frontmatter.confidence).toBe(0.85);
    expect(parsed.frontmatter.sources).toBe(3);
    expect(parsed.frontmatter.domain).toEqual(["personal", "music"]);
    expect(parsed.title).toBe("Alice likes rock music");
  });

  it("replaces body and title", async () => {
    await updateHandler(repo, {
      file: "worlds/people/alice/likes-rock.md",
      moment_name: "alice-corrected",
      updates: {
        title: "Alice loves rock music",
        body: "Updated: she really loves it.",
      },
    });

    const content = await repo.readFile("worlds/people/alice/likes-rock.md");
    const parsed = parseFact(content);
    expect(parsed.title).toBe("Alice loves rock music");
    expect(parsed.body).toBe("Updated: she really loves it.");
  });

  it("appends refs", async () => {
    await updateHandler(repo, {
      file: "worlds/people/alice/likes-rock.md",
      moment_name: "alice-ref-added",
      updates: { refs: ["episodic://event_99"] },
    });

    const content = await repo.readFile("worlds/people/alice/likes-rock.md");
    const parsed = parseFact(content);
    expect(parsed.frontmatter.refs).toContain("episodic://event_99");
  });

  it("throws on nonexistent file", async () => {
    await expect(
      updateHandler(repo, {
        file: "worlds/nonexistent.md",
        moment_name: "nope",
        updates: { confidence: 0.9 },
      })
    ).rejects.toThrow();
  });
});
