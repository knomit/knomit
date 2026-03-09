/**
 * E2E tests for MCP tool handlers.
 *
 * Each test creates an isolated git repo + search index, exercises the
 * handler functions directly (same code path as the MCP server), and
 * verifies both return values and on-disk/index state.
 */
import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { createTestEnv, destroyTestEnv, type TestEnv } from "./helpers";
import { learnHandler } from "../tools/learn";
import { queryHandler } from "../tools/query";
import { whyHandler } from "../tools/why";
import { updateHandler } from "../tools/update";
import { exploreHandler } from "../tools/explore";
import { forgetHandler } from "../tools/forget";
import { parseFact } from "../facts";

let env: TestEnv;

beforeEach(async () => {
  env = await createTestEnv();
});

afterEach(async () => {
  await destroyTestEnv(env);
});

// ---------------------------------------------------------------------------
// knomit_learn
// ---------------------------------------------------------------------------

describe("knomit_learn", () => {
  it("creates a single fact with correct frontmatter", async () => {
    const result = await learnHandler(env.repo, {
      moment_name: "test-moment",
      facts: [{
        path: "worlds/tools/bun",
        domain: ["tools"],
        confidence: 0.9,
        sources: 1,
        entities: ["bun"],
        refs: ["https://bun.sh"],
        title: "Bun is the preferred runtime",
        body: "We use Bun for all TypeScript projects.",
      }],
    }, env.searchIndex);

    expect(result.commits).toHaveLength(1);
    expect(result.moment_tag).toMatch(/^learn\//);

    // Verify file exists and parses correctly
    const content = await env.repo.readFile("worlds/tools/bun.md");
    const parsed = parseFact(content);
    expect(parsed.title).toBe("Bun is the preferred runtime");
    expect(parsed.frontmatter.domain).toEqual(["tools"]);
    expect(parsed.frontmatter.confidence).toBe(0.9);
    expect(parsed.frontmatter.entities).toEqual(["bun"]);
    expect(parsed.frontmatter.refs).toEqual(["https://bun.sh"]);
  });

  it("creates multiple facts in a single moment", async () => {
    const result = await learnHandler(env.repo, {
      moment_name: "multi-learn",
      facts: [
        {
          path: "worlds/people/alice/likes-coffee",
          domain: ["preferences"],
          confidence: 0.8,
          sources: 1,
          entities: ["alice"],
          title: "Alice likes coffee",
          body: "Prefers espresso.",
        },
        {
          path: "worlds/people/alice/uses-vim",
          domain: ["tools"],
          confidence: 0.95,
          sources: 2,
          entities: ["alice", "vim"],
          title: "Alice uses Vim",
          body: "Primary editor for all work.",
        },
      ],
    }, env.searchIndex);

    expect(result.commits).toHaveLength(2);
    expect(result.moment_tag).toMatch(/^learn\//);

    // Both files exist
    const exists1 = await env.repo.fileExists("worlds/people/alice/likes-coffee.md");
    const exists2 = await env.repo.fileExists("worlds/people/alice/uses-vim.md");
    expect(exists1).toBe(true);
    expect(exists2).toBe(true);
  });

  it("auto-prepends worlds/ and appends .md to paths", async () => {
    const result = await learnHandler(env.repo, {
      moment_name: "path-normalization",
      facts: [{
        path: "projects/webapp/uses-react",
        domain: ["architecture"],
        confidence: 0.9,
        sources: 1,
        entities: ["webapp", "react"],
        title: "WebApp uses React",
        body: "Frontend built with React 19.",
      }],
    }, env.searchIndex);

    expect(result.commits).toHaveLength(1);
    const exists = await env.repo.fileExists("worlds/projects/webapp/uses-react.md");
    expect(exists).toBe(true);
  });

  it("indexes facts in the search index", async () => {
    await learnHandler(env.repo, {
      moment_name: "indexed-learn",
      facts: [{
        path: "worlds/test/searchable",
        domain: ["testing"],
        confidence: 0.85,
        sources: 1,
        entities: ["search-test"],
        title: "Searchable fact for testing",
        body: "This fact should be findable via FTS.",
      }],
    }, env.searchIndex);

    // Query via search index
    const results = await env.searchIndex.search({ text: "searchable" });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0].title).toBe("Searchable fact for testing");
  });

  it("creates git tags for learning moments", async () => {
    const result = await learnHandler(env.repo, {
      moment_name: "tagged-moment",
      facts: [{
        path: "worlds/test/tagged",
        domain: ["testing"],
        confidence: 0.7,
        sources: 1,
        entities: [],
        title: "Tagged fact",
        body: "This moment should have a tag.",
      }],
    }, env.searchIndex);

    const tags = await env.repo.listTags();
    expect(tags).toContain(result.moment_tag);
  });

  it("overwrites existing fact at same path", async () => {
    await learnHandler(env.repo, {
      moment_name: "v1",
      facts: [{
        path: "worlds/test/overwrite",
        domain: ["testing"],
        confidence: 0.5,
        sources: 1,
        entities: [],
        title: "Version 1",
        body: "Original content.",
      }],
    }, env.searchIndex);

    await learnHandler(env.repo, {
      moment_name: "v2",
      facts: [{
        path: "worlds/test/overwrite",
        domain: ["testing"],
        confidence: 0.9,
        sources: 2,
        entities: [],
        title: "Version 2",
        body: "Updated content.",
      }],
    }, env.searchIndex);

    const content = await env.repo.readFile("worlds/test/overwrite.md");
    const parsed = parseFact(content);
    expect(parsed.title).toBe("Version 2");
    expect(parsed.frontmatter.confidence).toBe(0.9);
  });
});

// ---------------------------------------------------------------------------
// knomit_query
// ---------------------------------------------------------------------------

describe("knomit_query", () => {
  beforeEach(async () => {
    // Seed facts for querying
    await learnHandler(env.repo, {
      moment_name: "seed",
      facts: [
        {
          path: "worlds/projects/webapp/uses-react",
          domain: ["architecture", "frontend"],
          confidence: 0.9,
          sources: 2,
          entities: ["webapp", "react"],
          title: "WebApp uses React",
          body: "Frontend built with React 19 and TypeScript.",
        },
        {
          path: "worlds/projects/webapp/uses-postgres",
          domain: ["architecture", "database"],
          confidence: 0.85,
          sources: 1,
          entities: ["webapp", "postgres"],
          title: "WebApp uses PostgreSQL",
          body: "Primary database for transactional data.",
        },
        {
          path: "worlds/people/bob/prefers-dark-mode",
          domain: ["preferences"],
          confidence: 0.95,
          sources: 3,
          entities: ["bob"],
          title: "Bob prefers dark mode",
          body: "Uses dark mode in all editors and terminals.",
        },
        {
          path: "worlds/security/cve-2024-001",
          domain: ["security"],
          confidence: 0.6,
          sources: 1,
          entities: ["libfoo"],
          title: "CVE-2024-001 in libfoo",
          body: "Buffer overflow vulnerability, low severity.",
        },
      ],
    }, env.searchIndex);
  });

  it("queries by entity", async () => {
    const result = await queryHandler(env.repo, { entities: ["webapp"] }, env.searchIndex);
    expect(result.facts.length).toBe(2);
    const titles = result.facts.map(f => f.title);
    expect(titles).toContain("WebApp uses React");
    expect(titles).toContain("WebApp uses PostgreSQL");
  });

  it("queries by domain", async () => {
    const result = await queryHandler(env.repo, { domain: ["security"] }, env.searchIndex);
    expect(result.facts.length).toBe(1);
    expect(result.facts[0].title).toBe("CVE-2024-001 in libfoo");
  });

  it("queries by text search (FTS)", async () => {
    const result = await queryHandler(env.repo, { text: "dark mode" }, env.searchIndex);
    expect(result.facts.length).toBeGreaterThanOrEqual(1);
    expect(result.facts[0].title).toBe("Bob prefers dark mode");
  });

  it("queries by path prefix", async () => {
    const result = await queryHandler(env.repo, { path: "worlds/projects/webapp" }, env.searchIndex);
    expect(result.facts.length).toBe(2);
  });

  it("filters by min_confidence", async () => {
    const result = await queryHandler(env.repo, { domain: ["security"], min_confidence: 0.7 }, env.searchIndex);
    expect(result.facts.length).toBe(0); // CVE has 0.6 confidence
  });

  it("combines entity + domain filters", async () => {
    const result = await queryHandler(env.repo, {
      entities: ["webapp"],
      domain: ["database"],
    }, env.searchIndex);
    expect(result.facts.length).toBe(1);
    expect(result.facts[0].title).toBe("WebApp uses PostgreSQL");
  });

  it("returns empty for no matches", async () => {
    const result = await queryHandler(env.repo, { entities: ["nonexistent"] }, env.searchIndex);
    expect(result.facts.length).toBe(0);
  });

  it("throws when no search criteria provided", async () => {
    await expect(queryHandler(env.repo, {}, env.searchIndex)).rejects.toThrow(
      "At least one of text, entities, domain, or path must be provided"
    );
  });

  it("queries work without search index (grep fallback)", async () => {
    const result = await queryHandler(env.repo, { entities: ["bob"] });
    expect(result.facts.length).toBe(1);
    expect(result.facts[0].title).toBe("Bob prefers dark mode");
  });

  it("returns commit and date metadata", async () => {
    const result = await queryHandler(env.repo, { entities: ["bob"] }, env.searchIndex);
    expect(result.facts[0].commit).toBeTruthy();
    expect(result.facts[0].last_modified).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// knomit_update
// ---------------------------------------------------------------------------

describe("knomit_update", () => {
  beforeEach(async () => {
    await learnHandler(env.repo, {
      moment_name: "setup",
      facts: [{
        path: "worlds/test/updatable",
        domain: ["testing"],
        confidence: 0.5,
        sources: 1,
        entities: ["test-entity"],
        refs: [],
        title: "Updatable fact",
        body: "Original body.",
      }],
    }, env.searchIndex);
  });

  it("updates confidence", async () => {
    const result = await updateHandler(env.repo, {
      file: "worlds/test/updatable.md",
      moment_name: "confidence-boost",
      updates: { confidence: 0.9 },
    }, env.searchIndex);

    expect(result.commit).toBeTruthy();
    expect(result.moment_tag).toMatch(/^learn\//);

    const content = await env.repo.readFile("worlds/test/updatable.md");
    const parsed = parseFact(content);
    expect(parsed.frontmatter.confidence).toBe(0.9);
    // Body should be unchanged
    expect(parsed.body).toBe("Original body.");
  });

  it("updates body while keeping frontmatter", async () => {
    await updateHandler(env.repo, {
      file: "worlds/test/updatable.md",
      moment_name: "body-update",
      updates: { body: "Updated body with more detail." },
    }, env.searchIndex);

    const content = await env.repo.readFile("worlds/test/updatable.md");
    const parsed = parseFact(content);
    expect(parsed.body).toBe("Updated body with more detail.");
    expect(parsed.frontmatter.domain).toEqual(["testing"]);
  });

  it("updates title", async () => {
    await updateHandler(env.repo, {
      file: "worlds/test/updatable.md",
      moment_name: "title-update",
      updates: { title: "Renamed fact" },
    }, env.searchIndex);

    const content = await env.repo.readFile("worlds/test/updatable.md");
    const parsed = parseFact(content);
    expect(parsed.title).toBe("Renamed fact");
  });

  it("appends refs via merge", async () => {
    await updateHandler(env.repo, {
      file: "worlds/test/updatable.md",
      moment_name: "add-refs",
      updates: { refs: ["https://example.com/source1"] },
    }, env.searchIndex);

    const content = await env.repo.readFile("worlds/test/updatable.md");
    const parsed = parseFact(content);
    expect(parsed.frontmatter.refs).toContain("https://example.com/source1");
  });

  it("updates multiple fields at once", async () => {
    await updateHandler(env.repo, {
      file: "worlds/test/updatable.md",
      moment_name: "multi-update",
      updates: {
        confidence: 0.95,
        sources: 5,
        body: "Confirmed across many sessions.",
        domain: ["testing", "verified"],
        entities: ["test-entity", "verified-entity"],
      },
    }, env.searchIndex);

    const content = await env.repo.readFile("worlds/test/updatable.md");
    const parsed = parseFact(content);
    expect(parsed.frontmatter.confidence).toBe(0.95);
    expect(parsed.frontmatter.sources).toBe(5);
    expect(parsed.body).toBe("Confirmed across many sessions.");
    expect(parsed.frontmatter.domain).toContain("verified");
    expect(parsed.frontmatter.entities).toContain("verified-entity");
  });

  it("throws on non-existent file", async () => {
    await expect(updateHandler(env.repo, {
      file: "worlds/nonexistent.md",
      moment_name: "bad-update",
      updates: { confidence: 0.5 },
    }, env.searchIndex)).rejects.toThrow("File not found");
  });

  it("updates search index after update", async () => {
    await updateHandler(env.repo, {
      file: "worlds/test/updatable.md",
      moment_name: "index-update",
      updates: { title: "Completely new title for searching" },
    }, env.searchIndex);

    const results = await env.searchIndex.search({ text: "completely new title" });
    expect(results.length).toBeGreaterThanOrEqual(1);
    expect(results[0].title).toBe("Completely new title for searching");
  });
});

// ---------------------------------------------------------------------------
// knomit_forget
// ---------------------------------------------------------------------------

describe("knomit_forget", () => {
  beforeEach(async () => {
    await learnHandler(env.repo, {
      moment_name: "setup",
      facts: [{
        path: "worlds/test/forgettable",
        domain: ["testing"],
        confidence: 0.5,
        sources: 1,
        entities: [],
        title: "Forgettable fact",
        body: "This will be deleted.",
      }],
    }, env.searchIndex);
  });

  it("deletes the fact file", async () => {
    const result = await forgetHandler(env.repo, {
      file: "worlds/test/forgettable.md",
      moment_name: "cleanup",
    }, env.searchIndex);

    expect(result.commit).toBeTruthy();
    expect(result.moment_tag).toMatch(/^forget\//);

    const exists = await env.repo.fileExists("worlds/test/forgettable.md");
    expect(exists).toBe(false);
  });

  it("removes fact from search index", async () => {
    // Verify it's in the index first
    const before = await env.searchIndex.search({ text: "forgettable" });
    expect(before.length).toBeGreaterThanOrEqual(1);

    await forgetHandler(env.repo, {
      file: "worlds/test/forgettable.md",
      moment_name: "cleanup",
    }, env.searchIndex);

    const after = await env.searchIndex.search({ text: "forgettable" });
    expect(after.length).toBe(0);
  });

  it("creates a forget tag", async () => {
    const result = await forgetHandler(env.repo, {
      file: "worlds/test/forgettable.md",
      moment_name: "cleanup",
    }, env.searchIndex);

    const tags = await env.repo.listTags();
    expect(tags).toContain(result.moment_tag);
  });

  it("throws on non-existent file", async () => {
    await expect(forgetHandler(env.repo, {
      file: "worlds/nonexistent.md",
      moment_name: "bad-forget",
    }, env.searchIndex)).rejects.toThrow();
  });
});

// ---------------------------------------------------------------------------
// knomit_explore
// ---------------------------------------------------------------------------

describe("knomit_explore", () => {
  beforeEach(async () => {
    await learnHandler(env.repo, {
      moment_name: "setup",
      facts: [
        {
          path: "worlds/projects/webapp/uses-react",
          domain: ["architecture"],
          confidence: 0.9,
          sources: 1,
          entities: ["webapp"],
          title: "WebApp uses React",
          body: "Frontend framework.",
        },
        {
          path: "worlds/projects/webapp/uses-postgres",
          domain: ["architecture"],
          confidence: 0.85,
          sources: 1,
          entities: ["webapp"],
          title: "WebApp uses PostgreSQL",
          body: "Database choice.",
        },
        {
          path: "worlds/people/alice/likes-coffee",
          domain: ["preferences"],
          confidence: 0.8,
          sources: 1,
          entities: ["alice"],
          title: "Alice likes coffee",
          body: "Prefers espresso.",
        },
      ],
    }, env.searchIndex);
  });

  it("lists top-level worlds", async () => {
    const result = await exploreHandler(env.repo, { path: "worlds" }, { skipSync: true });
    const names = result.children.map(c => c.name);
    expect(names).toContain("projects");
    expect(names).toContain("people");
  });

  it("shows manifest at root", async () => {
    const result = await exploreHandler(env.repo, { path: "worlds" }, { skipSync: true });
    expect(result.manifest).not.toBeNull();
    expect(result.manifest!.title).toBe("Knowledge Base");
  });

  it("lists facts in a directory", async () => {
    const result = await exploreHandler(env.repo, { path: "worlds/projects/webapp" }, { skipSync: true });
    const facts = result.children.filter(c => c.type === "fact");
    expect(facts.length).toBe(2);
    const names = facts.map(f => f.name);
    expect(names).toContain("uses-react.md");
    expect(names).toContain("uses-postgres.md");
  });

  it("shows sub-worlds (directories) correctly", async () => {
    const result = await exploreHandler(env.repo, { path: "worlds/projects" }, { skipSync: true });
    const worlds = result.children.filter(c => c.type === "world");
    expect(worlds.length).toBe(1);
    expect(worlds[0].name).toBe("webapp");
  });

  it("includes fact summaries (titles)", async () => {
    const result = await exploreHandler(env.repo, { path: "worlds/projects/webapp" }, { skipSync: true });
    const reactFact = result.children.find(c => c.name === "uses-react.md");
    expect(reactFact?.summary).toBe("WebApp uses React");
  });

  it("returns empty children for non-existent path", async () => {
    const result = await exploreHandler(env.repo, { path: "worlds/nonexistent" }, { skipSync: true });
    expect(result.children).toEqual([]);
  });

  it("shows inherited facts from parent directories", async () => {
    // Create a fact at the projects level (not inside webapp/)
    await learnHandler(env.repo, {
      moment_name: "parent-fact",
      facts: [{
        path: "worlds/projects/coding-standards",
        domain: ["standards"],
        confidence: 0.9,
        sources: 1,
        entities: [],
        title: "General coding standards",
        body: "Use TypeScript, test everything.",
      }],
    }, env.searchIndex);

    const result = await exploreHandler(env.repo, { path: "worlds/projects/webapp" }, { skipSync: true });
    const inherited = result.inherited_facts;
    // Should inherit the coding-standards fact from parent
    expect(inherited.length).toBeGreaterThanOrEqual(1);
    const standardsFact = inherited.find(f => f.title === "General coding standards");
    expect(standardsFact).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// knomit_why
// ---------------------------------------------------------------------------

describe("knomit_why", () => {
  it("returns fact provenance with history", async () => {
    await learnHandler(env.repo, {
      moment_name: "initial-learn",
      facts: [{
        path: "worlds/test/provenance",
        domain: ["testing"],
        confidence: 0.7,
        sources: 1,
        entities: ["test"],
        refs: ["https://example.com"],
        title: "Provenance test fact",
        body: "Original version.",
      }],
    }, env.searchIndex);

    const result = await whyHandler(env.repo, { file: "worlds/test/provenance.md" });

    expect(result.fact.title).toBe("Provenance test fact");
    expect(result.fact.frontmatter.confidence).toBe(0.7);
    expect(result.refs).toEqual(["https://example.com"]);
    expect(result.history.length).toBeGreaterThanOrEqual(1);
    expect(result.learning_moment.tag).toMatch(/^learn\//);
  });

  it("tracks history across updates", async () => {
    await learnHandler(env.repo, {
      moment_name: "v1",
      facts: [{
        path: "worlds/test/history-tracked",
        domain: ["testing"],
        confidence: 0.5,
        sources: 1,
        entities: [],
        title: "History tracked fact",
        body: "Version 1.",
      }],
    }, env.searchIndex);

    await updateHandler(env.repo, {
      file: "worlds/test/history-tracked.md",
      moment_name: "v2",
      updates: { confidence: 0.8, body: "Version 2." },
    }, env.searchIndex);

    await updateHandler(env.repo, {
      file: "worlds/test/history-tracked.md",
      moment_name: "v3",
      updates: { confidence: 0.95, body: "Version 3 — confirmed." },
    }, env.searchIndex);

    const result = await whyHandler(env.repo, { file: "worlds/test/history-tracked.md" });
    expect(result.history.length).toBe(3);
    expect(result.fact.frontmatter.confidence).toBe(0.95);
  });

  it("finds sibling facts from the same learning moment", async () => {
    await learnHandler(env.repo, {
      moment_name: "sibling-test",
      facts: [
        {
          path: "worlds/test/sibling-a",
          domain: ["testing"],
          confidence: 0.8,
          sources: 1,
          entities: [],
          title: "Sibling A",
          body: "First of a pair.",
        },
        {
          path: "worlds/test/sibling-b",
          domain: ["testing"],
          confidence: 0.8,
          sources: 1,
          entities: [],
          title: "Sibling B",
          body: "Second of a pair.",
        },
      ],
    }, env.searchIndex);

    const result = await whyHandler(env.repo, { file: "worlds/test/sibling-a.md" });
    expect(result.learning_moment.siblings.length).toBeGreaterThanOrEqual(1);
    const siblingFiles = result.learning_moment.siblings.map(s => s.file);
    expect(siblingFiles).toContain("worlds/test/sibling-b.md");
  });
});

// ---------------------------------------------------------------------------
// Full workflow: learn → query → update → why → forget → explore
// ---------------------------------------------------------------------------

describe("full MCP workflow", () => {
  it("exercises the complete lifecycle", async () => {
    // 1. Learn facts
    const learnResult = await learnHandler(env.repo, {
      moment_name: "full-workflow",
      facts: [
        {
          path: "worlds/projects/api/uses-bun",
          domain: ["architecture", "runtime"],
          confidence: 0.9,
          sources: 1,
          entities: ["api-project", "bun"],
          refs: ["https://bun.sh"],
          title: "API project uses Bun runtime",
          body: "Bun is used for speed and simplicity.",
        },
        {
          path: "worlds/projects/api/rest-endpoints",
          domain: ["architecture", "api"],
          confidence: 0.7,
          sources: 1,
          entities: ["api-project"],
          refs: [],
          title: "API has RESTful endpoints",
          body: "Standard REST with JSON responses.",
        },
      ],
    }, env.searchIndex);
    expect(learnResult.commits).toHaveLength(2);

    // 2. Query to verify
    const queryResult = await queryHandler(env.repo, {
      entities: ["api-project"],
    }, env.searchIndex);
    expect(queryResult.facts.length).toBe(2);

    // 3. Update confidence after confirmation
    await updateHandler(env.repo, {
      file: "worlds/projects/api/rest-endpoints.md",
      moment_name: "rest-confirmed",
      updates: { confidence: 0.95, sources: 3 },
    }, env.searchIndex);

    // 4. Check provenance
    const whyResult = await whyHandler(env.repo, {
      file: "worlds/projects/api/rest-endpoints.md",
    });
    expect(whyResult.history.length).toBe(2); // learn + update
    expect(whyResult.fact.frontmatter.confidence).toBe(0.95);

    // 5. Explore the hierarchy
    const exploreResult = await exploreHandler(env.repo, {
      path: "worlds/projects/api",
    }, { skipSync: true });
    expect(exploreResult.children.length).toBe(2);

    // 6. Forget one fact
    await forgetHandler(env.repo, {
      file: "worlds/projects/api/uses-bun.md",
      moment_name: "remove-bun-fact",
    }, env.searchIndex);

    // 7. Verify it's gone
    const afterForget = await queryHandler(env.repo, {
      entities: ["bun"],
    }, env.searchIndex);
    expect(afterForget.facts.length).toBe(0);

    // 8. Remaining fact still exists
    const remaining = await queryHandler(env.repo, {
      entities: ["api-project"],
    }, env.searchIndex);
    expect(remaining.facts.length).toBe(1);
    expect(remaining.facts[0].title).toBe("API has RESTful endpoints");
  });
});
