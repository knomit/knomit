/**
 * E2E tests for the synthesis pipeline: prune, distill, multi-step,
 * auto-merge, branch management, delta detection, and chunking.
 *
 * Uses a mock LLM adapter to avoid real API calls.
 */
import { describe, it, expect, beforeEach, afterEach, mock } from "bun:test";
import { mkdtemp, rm, mkdir } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { GitRepo } from "../git";
import { SearchIndex } from "../search-index";
import { commitFact } from "../fact-ops";
import type { Recipe } from "../recipe";
import type { LLMAdapter, LLMConfig } from "../llm";

// --- Mock LLM ---

let llmCallCount = 0;
let llmResponses: string[] = [];
let capturedPrompts: string[] = [];

function resolveProvider(
  model: string,
  explicit?: string,
): "anthropic" | "gemini" | "bedrock" {
  if (explicit) return explicit as "anthropic" | "gemini" | "bedrock";
  if (model.startsWith("claude")) return "anthropic";
  if (model.startsWith("gemini")) return "gemini";
  if (model.includes("anthropic.") || model.startsWith("us.") || model.startsWith("eu."))
    return "bedrock";
  throw new Error(`Cannot infer provider for model "${model}".`);
}

function configFromEnv(): LLMConfig {
  return {
    provider: undefined,
    model: process.env.KNOMIT_LLM_MODEL ?? "claude-sonnet-4-6",
    apiKey: "mock-key",
  };
}

mock.module("../llm", () => ({
  resolveProvider,
  configFromEnv,
  createAdapter: (): LLMAdapter => ({
    complete: async (_system: string, messages: Array<{ role: string; content: string }>, _onChunk?: (text: string) => void): Promise<string> => {
      capturedPrompts.push(messages[0]?.content ?? "");
      const response = llmResponses[llmCallCount] ?? llmResponses[llmResponses.length - 1] ?? "";
      llmCallCount++;
      return response;
    },
  }),
}));

// Import AFTER mock
const { synthesize } = await import("../synthesize");

let repoPath: string;
let repo: GitRepo;
let cacheDir: string;
let searchIndex: SearchIndex;

beforeEach(async () => {
  llmCallCount = 0;
  llmResponses = [];
  capturedPrompts = [];

  repoPath = await mkdtemp(join(tmpdir(), "knomit-synth-e2e-"));
  repo = new GitRepo(repoPath, "test-machine");
  await repo.init();
  cacheDir = join(repoPath, ".cache");
  await mkdir(cacheDir, { recursive: true });
  searchIndex = new SearchIndex(cacheDir);
  await searchIndex.init();
});

afterEach(async () => {
  searchIndex?.close();
  await rm(repoPath, { recursive: true, force: true });
});

/** Seed a set of security-related facts. */
async function seedSecurityFacts() {
  for (let i = 1; i <= 5; i++) {
    await commitFact(repo, {
      path: `worlds/security/cve-${i}`,
      title: `CVE-2024-00${i} in libfoo`,
      body: `Vulnerability ${i} description for libfoo.`,
      domain: ["security"],
      confidence: 0.5 + i * 0.1,
      sources: 1,
      entities: ["libfoo"],
      refs: [],
    }, searchIndex);
  }
  await searchIndex.reindex(repo);
}

// ---------------------------------------------------------------------------
// Prune
// ---------------------------------------------------------------------------

describe("synthesize prune", () => {
  it("deletes forgotten facts and keeps the rest", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      decisions: [
        { file: "worlds/security/cve-1.md", action: "forget", reason: "Patched" },
        { file: "worlds/security/cve-2.md", action: "forget", reason: "Duplicate" },
        { file: "worlds/security/cve-3.md", action: "keep", reason: "Still active" },
        { file: "worlds/security/cve-4.md", action: "keep", reason: "Under investigation" },
        { file: "worlds/security/cve-5.md", action: "keep", reason: "Critical" },
      ],
      merges: [],
      summary: "Pruned 2 resolved CVEs",
    })];

    const recipe: Recipe = {
      name: "prune-test",
      prompt: "Security review",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "prune", prompt: "Review CVEs" }],
    };

    const result = await synthesize(repo, searchIndex, recipe);
    expect(result.merged).toBe(true);
    expect(result.stepSummaries).toHaveLength(1);
    expect(result.stepSummaries[0]).toContain("forgotten");

    // Verify deletions
    expect(await repo.fileExists("worlds/security/cve-1.md")).toBe(false);
    expect(await repo.fileExists("worlds/security/cve-2.md")).toBe(false);

    // Verify keeps
    expect(await repo.fileExists("worlds/security/cve-3.md")).toBe(true);
    expect(await repo.fileExists("worlds/security/cve-4.md")).toBe(true);
    expect(await repo.fileExists("worlds/security/cve-5.md")).toBe(true);
  });

  it("updates confidence on 'update' decisions", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      decisions: [
        { file: "worlds/security/cve-1.md", action: "update", confidence: 0.3, reason: "Likely fixed" },
        { file: "worlds/security/cve-2.md", action: "keep", reason: "Still valid" },
        { file: "worlds/security/cve-3.md", action: "keep", reason: "Active" },
        { file: "worlds/security/cve-4.md", action: "keep", reason: "Active" },
        { file: "worlds/security/cve-5.md", action: "keep", reason: "Active" },
      ],
      merges: [],
      summary: "Updated 1 CVE confidence",
    })];

    const recipe: Recipe = {
      name: "update-confidence",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "prune", prompt: "" }],
    };

    await synthesize(repo, searchIndex, recipe);

    const { parseFact } = await import("../facts");
    const content = await repo.readFile("worlds/security/cve-1.md");
    const parsed = parseFact(content);
    expect(parsed.frontmatter.confidence).toBe(0.3);
  });

  it("merges duplicate facts", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      decisions: [
        { file: "worlds/security/cve-1.md", action: "keep", reason: "Will merge" },
        { file: "worlds/security/cve-2.md", action: "keep", reason: "Will merge" },
        { file: "worlds/security/cve-3.md", action: "keep", reason: "Active" },
        { file: "worlds/security/cve-4.md", action: "keep", reason: "Active" },
        { file: "worlds/security/cve-5.md", action: "keep", reason: "Active" },
      ],
      merges: [{
        sources: ["worlds/security/cve-1.md", "worlds/security/cve-2.md"],
        merged: {
          path: "worlds/security/libfoo-overflow",
          title: "Multiple buffer overflows in libfoo",
          body: "Combined CVE-1 and CVE-2.",
          domain: ["security"],
          confidence: 0.85,
          entities: ["libfoo"],
          refs: ["worlds/security/cve-1.md", "worlds/security/cve-2.md"],
        },
      }],
      summary: "Merged 2 CVEs into 1",
    })];

    const recipe: Recipe = {
      name: "merge-test",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "prune", prompt: "" }],
    };

    await synthesize(repo, searchIndex, recipe);

    // Merged fact exists
    expect(await repo.fileExists("worlds/security/libfoo-overflow.md")).toBe(true);

    // Source facts deleted (merge sources are removed)
    expect(await repo.fileExists("worlds/security/cve-1.md")).toBe(false);
    expect(await repo.fileExists("worlds/security/cve-2.md")).toBe(false);

    // Other facts untouched
    expect(await repo.fileExists("worlds/security/cve-3.md")).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Distill
// ---------------------------------------------------------------------------

describe("synthesize distill", () => {
  it("creates synthesized facts and forgets subsumed ones", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      synthesize: [{
        path: "worlds/security/libfoo-pattern",
        title: "libfoo has recurring vulnerabilities",
        body: "Pattern of buffer overflows across 5 CVEs in libfoo.",
        domain: ["security"],
        confidence: 0.9,
        entities: ["libfoo"],
        refs: ["worlds/security/cve-1.md", "worlds/security/cve-2.md"],
      }],
      forget: ["worlds/security/cve-1.md", "worlds/security/cve-2.md"],
      summary: "Consolidated 2 CVEs into 1 pattern",
    })];

    const recipe: Recipe = {
      name: "distill-test",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "distill", prompt: "" }],
    };

    const result = await synthesize(repo, searchIndex, recipe);
    expect(result.merged).toBe(true);
    expect(result.stepSummaries[0]).toContain("learned");

    // Synthesized fact exists
    expect(await repo.fileExists("worlds/security/libfoo-pattern.md")).toBe(true);

    // Subsumed facts deleted
    expect(await repo.fileExists("worlds/security/cve-1.md")).toBe(false);
    expect(await repo.fileExists("worlds/security/cve-2.md")).toBe(false);

    // Others untouched
    expect(await repo.fileExists("worlds/security/cve-3.md")).toBe(true);
  });

  it("keeps all facts when forget list is empty", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      synthesize: [{
        path: "worlds/security/overview",
        title: "Security overview",
        body: "5 CVEs tracked for libfoo.",
        domain: ["security"],
        confidence: 0.7,
        entities: ["libfoo"],
        refs: [],
      }],
      forget: [],
      summary: "Added overview without removing originals",
    })];

    const recipe: Recipe = {
      name: "no-forget",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "distill", prompt: "" }],
    };

    await synthesize(repo, searchIndex, recipe);

    // All originals still exist
    for (let i = 1; i <= 5; i++) {
      expect(await repo.fileExists(`worlds/security/cve-${i}.md`)).toBe(true);
    }
    // New fact also exists
    expect(await repo.fileExists("worlds/security/overview.md")).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Multi-step pipeline
// ---------------------------------------------------------------------------

describe("synthesize multi-step", () => {
  it("runs prune then distill sequentially", async () => {
    await seedSecurityFacts();

    llmResponses = [
      // Step 1: Prune — delete cve-1
      JSON.stringify({
        decisions: [
          { file: "worlds/security/cve-1.md", action: "forget", reason: "stale" },
          { file: "worlds/security/cve-2.md", action: "keep", reason: "active" },
          { file: "worlds/security/cve-3.md", action: "keep", reason: "active" },
          { file: "worlds/security/cve-4.md", action: "keep", reason: "active" },
          { file: "worlds/security/cve-5.md", action: "keep", reason: "active" },
        ],
        merges: [],
        summary: "Pruned 1",
      }),
      // Step 2: Distill — synthesize from remaining
      JSON.stringify({
        synthesize: [{
          path: "worlds/security/active-summary",
          title: "4 active CVEs in libfoo",
          body: "Summary of remaining vulnerabilities.",
          domain: ["security"],
          confidence: 0.8,
          entities: ["libfoo"],
          refs: [],
        }],
        forget: [],
        summary: "Summarized 4 CVEs",
      }),
    ];

    const recipe: Recipe = {
      name: "multi-step",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [
        { mode: "prune", prompt: "" },
        { mode: "distill", prompt: "" },
      ],
    };

    const result = await synthesize(repo, searchIndex, recipe);
    expect(result.merged).toBe(true);
    expect(result.stepSummaries).toHaveLength(2);
    expect(llmCallCount).toBe(2);

    // cve-1 pruned
    expect(await repo.fileExists("worlds/security/cve-1.md")).toBe(false);
    // Summary created
    expect(await repo.fileExists("worlds/security/active-summary.md")).toBe(true);
    // Others still exist
    expect(await repo.fileExists("worlds/security/cve-2.md")).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Branch management
// ---------------------------------------------------------------------------

describe("synthesize branching", () => {
  it("auto-merge returns to original branch and deletes synthesis branch", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      decisions: [
        { file: "worlds/security/cve-1.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-2.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-3.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-4.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-5.md", action: "keep", reason: "active" },
      ],
      merges: [],
      summary: "All current",
    })];

    const recipe: Recipe = {
      name: "branch-test",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "prune", prompt: "" }],
    };

    await synthesize(repo, searchIndex, recipe);

    // Should be on original machine branch
    const branch = await repo.currentBranch();
    expect(branch).toBe("machine/test-machine");

    // Synthesis branch should be deleted
    const branches = await repo.listBranches();
    expect(branches).not.toContain("synthesize/branch-test");
  });

  it("no-auto-merge keeps synthesis branch", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      synthesize: [{
        path: "worlds/security/insight",
        title: "Security insight",
        body: "Interesting pattern.",
        domain: ["security"],
        confidence: 0.7,
        entities: [],
        refs: [],
      }],
      forget: [],
      summary: "Found pattern",
    })];

    const recipe: Recipe = {
      name: "review-branch",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: false,
      steps: [{ mode: "distill", prompt: "" }],
    };

    const result = await synthesize(repo, searchIndex, recipe);
    expect(result.merged).toBe(false);
    expect(result.branch).toBe("synthesize/review-branch");

    // Should return to original branch
    const branch = await repo.currentBranch();
    expect(branch).toBe("machine/test-machine");

    // Synthesis branch should still exist
    const branches = await repo.listBranches();
    expect(branches).toContain("synthesize/review-branch");
  });
});

// ---------------------------------------------------------------------------
// Synthesis log and delta detection
// ---------------------------------------------------------------------------

describe("synthesis log", () => {
  it("records synthesis run in the log", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      decisions: [
        { file: "worlds/security/cve-1.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-2.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-3.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-4.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-5.md", action: "keep", reason: "active" },
      ],
      merges: [],
      summary: "All current",
    })];

    const recipe: Recipe = {
      name: "log-test",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "prune", prompt: "" }],
    };

    await synthesize(repo, searchIndex, recipe);

    const logEntry = searchIndex.getSynthesisLog("log-test");
    expect(logEntry).not.toBeNull();
    expect(logEntry!.lastCommit).toBeTruthy();
    expect(logEntry!.factsProcessed).toBeGreaterThanOrEqual(1);
  });
});

// ---------------------------------------------------------------------------
// Progress events
// ---------------------------------------------------------------------------

describe("synthesize progress events", () => {
  it("fires progress events in correct order", async () => {
    await seedSecurityFacts();

    llmResponses = [JSON.stringify({
      decisions: [
        { file: "worlds/security/cve-1.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-2.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-3.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-4.md", action: "keep", reason: "active" },
        { file: "worlds/security/cve-5.md", action: "keep", reason: "active" },
      ],
      merges: [],
      summary: "All current",
    })];

    const recipe: Recipe = {
      name: "progress-test",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "prune", prompt: "" }],
    };

    const events: string[] = [];
    await synthesize(repo, searchIndex, recipe, (event) => {
      events.push(event.phase);
    });

    // Key phases should fire in order
    expect(events).toContain("step-start");
    expect(events).toContain("reindex");
    expect(events).toContain("gather");
    expect(events).toContain("llm");
    expect(events).toContain("llm-done");
    expect(events).toContain("apply");
    expect(events).toContain("merge");
    expect(events).toContain("done");

    // step-start before gather
    expect(events.indexOf("step-start")).toBeLessThan(events.indexOf("gather"));
    // gather before llm
    expect(events.indexOf("gather")).toBeLessThan(events.indexOf("llm"));
    // done is last
    expect(events.indexOf("done")).toBe(events.length - 1);
  });
});

// ---------------------------------------------------------------------------
// Empty scope
// ---------------------------------------------------------------------------

describe("synthesize edge cases", () => {
  it("handles empty fact set gracefully", async () => {
    // No facts seeded — scope has no matches
    llmResponses = []; // Should not be called

    const recipe: Recipe = {
      name: "empty-scope",
      prompt: "",
      scope: { domain: ["nonexistent"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "prune", prompt: "" }],
    };

    const result = await synthesize(repo, searchIndex, recipe);
    expect(result.merged).toBe(true);
    expect(result.stepSummaries[0]).toContain("No facts");
    expect(llmCallCount).toBe(0); // LLM should not be called
  });
});
