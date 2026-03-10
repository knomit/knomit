import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo } from "../git";
import { learnHandler } from "./learn";
import { forgetHandler } from "./forget";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

let testDir: string;
let repo: GitRepo;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-test-"));
  repo = new GitRepo(join(testDir, "repo"), "test-agent");
  await repo.init();

  await learnHandler(repo, {
    moment_name: "seed",
    facts: [
      {
        path: "know/people/alice/likes-rock.md",
        domain: ["personal", "music"],
        confidence: 0.8,
        sources: 2,
        entities: ["alice"],
        title: "Alice likes rock music",
        body: "Strong preference.",
      },
    ],
  });
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("knomit_forget", () => {
  it("deletes a fact file and creates a commit", async () => {
    const result = await forgetHandler(repo, {
      file: "know/people/alice/likes-rock.md",
      moment_name: "outdated-preference",
    });

    expect(result.commit).toMatch(/^[0-9a-f]+$/);
    expect(result.file).toBe("know/people/alice/likes-rock.md");
    expect(result.moment_tag).toBe("forget/outdated-preference");

    const exists = await repo.fileExists("know/people/alice/likes-rock.md");
    expect(exists).toBe(false);
  });

  it("tags the commit with forget/ prefix", async () => {
    await forgetHandler(repo, {
      file: "know/people/alice/likes-rock.md",
      moment_name: "no-longer-true",
    });

    const tags = await repo.listTags();
    expect(tags).toContain("forget/no-longer-true");
  });

  it("throws on nonexistent file", async () => {
    await expect(
      forgetHandler(repo, {
        file: "know/nonexistent.md",
        moment_name: "nope",
      })
    ).rejects.toThrow("File not found");
  });
});
