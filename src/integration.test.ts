import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo } from "./git";
import { SearchIndex } from "./search-index";
import { learnHandler } from "./tools/learn";
import { queryHandler } from "./tools/query";
import { whyHandler } from "./tools/why";
import { updateHandler } from "./tools/update";
import { exploreHandler } from "./tools/explore";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

let testDir: string;
let repo: GitRepo;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-integration-"));
  repo = new GitRepo(join(testDir, "repo"), "test-agent");
  await repo.init();
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("full workflow", () => {
  it("learn → query → update → why → explore", async () => {
    // 1. Learn some facts
    const learnResult = await learnHandler(repo, {
      moment_name: "alice-prefs",
      facts: [
        {
          path: "know/people/alice/likes-rock.md",
          domain: ["personal", "music"],
          confidence: 0.7,
          sources: 1,
          entities: ["alice", "rock_music"],
          title: "Alice likes rock music",
          body: "Initial signal from conversation.",
        },
        {
          path: "know/people/alice/prefers-vim.md",
          domain: ["personal", "tools"],
          confidence: 0.9,
          sources: 2,
          entities: ["alice", "vim"],
          title: "Alice prefers Vim",
          body: "Uses Vim for all editing.",
        },
      ],
    });
    expect(learnResult.commits.length).toBe(2);

    // 2. Query by entity
    const queryResult = await queryHandler(repo, { entities: ["alice"] });
    expect(queryResult.facts.length).toBe(2);

    // 3. Query by domain
    const musicFacts = await queryHandler(repo, { domain: ["music"] });
    expect(musicFacts.facts.length).toBe(1);

    // 4. Update a fact
    const updateResult = await updateHandler(repo, {
      file: "know/people/alice/likes-rock.md",
      moment_name: "alice-rock-confirmed",
      updates: { confidence: 0.9, sources: 3 },
    });
    expect(updateResult.commit).toBeTruthy();

    // 5. Why is this true?
    const whyResult = await whyHandler(repo, {
      file: "know/people/alice/likes-rock.md",
    });
    expect(whyResult.history.length).toBe(2); // original + update
    expect(whyResult.fact.frontmatter).toHaveProperty("confidence", 0.9);

    // 6. Explore
    const exploreResult = await exploreHandler(repo, { path: "know/people/alice" });
    const factNames = exploreResult.children.map(c => c.name);
    expect(factNames).toContain("likes-rock.md");
    expect(factNames).toContain("prefers-vim.md");
  });
});

describe("full workflow with search index", () => {
  it("learn → query via FTS5 → update → query again", async () => {
    const idxDir = await mkdtemp(join(tmpdir(), "knomit-integ-idx-"));
    const searchIndex = new SearchIndex(idxDir);
    await searchIndex.init();

    // Learn a fact
    const learnResult = await learnHandler(repo, {
      moment_name: "fts-test",
      facts: [{
        path: "know/test/fts-fact.md",
        domain: ["testing"],
        confidence: 0.8,
        sources: 1,
        entities: ["fts"],
        title: "FTS5 works great",
        body: "Full text search is fast and relevant.",
      }],
    }, searchIndex);

    expect(learnResult.commits.length).toBe(1);

    // Query via text search
    const queryResult = await queryHandler(repo, { text: "full text search" }, searchIndex);
    expect(queryResult.facts.length).toBeGreaterThanOrEqual(1);
    expect(queryResult.facts[0].title).toBe("FTS5 works great");

    searchIndex.close();
    await rm(idxDir, { recursive: true, force: true });
  });
});
