import { describe, it, expect, beforeEach, afterEach, mock } from "bun:test";
import { mkdtemp, rm, mkdir } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { GitRepo } from "./git";
import { SearchIndex } from "./search-index";
import { commitFact } from "./fact-ops";
import type { Recipe } from "./recipe";
import type { LLMAdapter, LLMConfig } from "./llm";

// Track LLM calls so we can return different responses per step
let llmCallCount = 0;
let llmResponses: string[] = [];
let useMockLlm = false;

// Replicate the real resolveProvider logic so it works for other tests
function resolveProvider(
  model: string,
  explicit?: string,
): "anthropic" | "gemini" | "bedrock" {
  if (explicit) return explicit as "anthropic" | "gemini" | "bedrock";
  if (model.startsWith("claude")) return "anthropic";
  if (model.startsWith("gemini")) return "gemini";
  if (model.includes("anthropic.") || model.startsWith("us.") || model.startsWith("eu."))
    return "bedrock";
  throw new Error(
    `Cannot infer provider for model "${model}". Set KNOMIT_LLM_PROVIDER or specify provider explicitly.`,
  );
}

function configFromEnv(): LLMConfig {
  const provider = process.env.KNOMIT_LLM_PROVIDER as
    | "anthropic"
    | "gemini"
    | "bedrock"
    | undefined;
  const model = process.env.KNOMIT_LLM_MODEL ?? "claude-sonnet-4-6";
  return {
    provider,
    model,
    apiKey:
      provider === "gemini"
        ? process.env.GOOGLE_AI_API_KEY
        : process.env.ANTHROPIC_API_KEY,
    region: process.env.AWS_REGION,
    accessKeyId: process.env.AWS_ACCESS_KEY_ID,
    secretAccessKey: process.env.AWS_SECRET_ACCESS_KEY,
  };
}

// Mock the LLM module. We replicate the real resolveProvider/configFromEnv
// so that other test files (like llm.test.ts) still get correct behavior.
// Only createAdapter is replaced: it returns a fake adapter when
// llmResponses is populated, otherwise it throws like the real one would
// when credentials are missing.
mock.module("./llm", () => ({
  resolveProvider,
  configFromEnv,
  createAdapter: (config: LLMConfig): LLMAdapter => {
    // When our e2e tests are running, return mock adapter
    if (useMockLlm) {
      return {
        complete: async (): Promise<string> => {
          const response = llmResponses[llmCallCount] ?? llmResponses[llmResponses.length - 1] ?? "";
          llmCallCount++;
          return response;
        },
      };
    }
    // Otherwise, behave like the real createAdapter — validate config
    const provider = resolveProvider(config.model, config.provider);
    if (provider === "anthropic") {
      const apiKey = config.apiKey ?? process.env.ANTHROPIC_API_KEY;
      if (!apiKey) throw new Error("ANTHROPIC_API_KEY is required for Anthropic provider");
    } else if (provider === "gemini") {
      const apiKey = config.apiKey ?? process.env.GOOGLE_AI_API_KEY;
      if (!apiKey) throw new Error("GOOGLE_AI_API_KEY is required for Gemini provider");
    } else if (provider === "bedrock") {
      const accessKeyId = config.accessKeyId ?? process.env.AWS_ACCESS_KEY_ID;
      const secretAccessKey = config.secretAccessKey ?? process.env.AWS_SECRET_ACCESS_KEY;
      if (!accessKeyId || !secretAccessKey)
        throw new Error("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required for Bedrock provider");
    }
    // Return a dummy adapter (won't be called in real tests)
    return {
      complete: async () => { throw new Error("real LLM adapter called in test"); },
    };
  },
}));

// Import synthesize AFTER mock is set up
const { synthesize } = await import("./synthesize");

describe("synthesize e2e", () => {
  let repoPath: string;
  let repo: GitRepo;
  let cacheDir: string;
  let searchIndex: SearchIndex;

  beforeEach(async () => {
    llmCallCount = 0;
    llmResponses = [];
    useMockLlm = true;

    repoPath = await mkdtemp(join(tmpdir(), "synth-e2e-"));
    repo = new GitRepo(repoPath, "test-machine");
    await repo.init();

    cacheDir = join(repoPath, ".cache");
    await mkdir(cacheDir, { recursive: true });
    searchIndex = new SearchIndex(cacheDir);
    await searchIndex.init();

    // Create facts
    await commitFact(
      repo,
      {
        path: "worlds/security/cve-1.md",
        title: "CVE-2024-001 in libfoo",
        body: "Buffer overflow in libfoo 1.0",
        domain: ["security"],
        confidence: 0.9,
        sources: 1,
        entities: ["libfoo"],
        refs: [],
      },
      searchIndex,
    );

    await commitFact(
      repo,
      {
        path: "worlds/security/cve-2.md",
        title: "CVE-2024-002 in libfoo",
        body: "Another buffer overflow in libfoo 1.0",
        domain: ["security"],
        confidence: 0.8,
        sources: 1,
        entities: ["libfoo"],
        refs: [],
      },
      searchIndex,
    );

    await searchIndex.reindex(repo);
  });

  afterEach(async () => {
    useMockLlm = false;
    searchIndex?.close();
    await rm(repoPath, { recursive: true, force: true });
  });

  it("prune step deletes forgotten facts and merges back", async () => {
    llmResponses = [
      JSON.stringify({
        decisions: [
          { file: "worlds/security/cve-1.md", action: "forget", reason: "stale" },
          { file: "worlds/security/cve-2.md", action: "keep", reason: "current" },
        ],
        merges: [],
        summary: "Pruned 1 stale fact",
      }),
    ];

    const recipe: Recipe = {
      name: "test-prune",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "prune", prompt: "" }],
    };

    const result = await synthesize(repo, searchIndex, recipe);

    expect(result.merged).toBe(true);
    expect(result.stepSummaries.length).toBe(1);
    expect(result.stepSummaries[0]).toContain("forgotten");

    // Verify the forgotten fact was actually deleted
    const exists = await repo.fileExists("worlds/security/cve-1.md");
    expect(exists).toBe(false);

    // Verify the kept fact still exists
    const exists2 = await repo.fileExists("worlds/security/cve-2.md");
    expect(exists2).toBe(true);

    // Verify we're back on the original branch (not the synthesis branch)
    const branch = await repo.currentBranch();
    expect(branch).toBe("machine/test-machine");

    // Verify synthesis log was updated
    const logEntry = searchIndex.getSynthesisLog("test-prune");
    expect(logEntry).not.toBeNull();
    expect(logEntry!.factsProcessed).toBe(1);
  });

  it("distill step creates synthesized facts and forgets subsumed ones", async () => {
    llmResponses = [
      JSON.stringify({
        synthesize: [
          {
            path: "worlds/security/libfoo-vulns.md",
            title: "Multiple buffer overflows in libfoo 1.0",
            body: "libfoo 1.0 has multiple buffer overflow vulnerabilities (CVE-2024-001, CVE-2024-002).",
            domain: ["security"],
            confidence: 0.9,
            entities: ["libfoo"],
            refs: ["worlds/security/cve-1.md", "worlds/security/cve-2.md"],
          },
        ],
        forget: ["worlds/security/cve-1.md", "worlds/security/cve-2.md"],
        summary: "Consolidated 2 CVEs into 1 pattern",
      }),
    ];

    const recipe: Recipe = {
      name: "test-distill",
      prompt: "",
      scope: { domain: ["security"], entities: [], search: [], path: "" },
      auto_merge: true,
      steps: [{ mode: "distill", prompt: "" }],
    };

    const result = await synthesize(repo, searchIndex, recipe);

    expect(result.merged).toBe(true);
    expect(result.stepSummaries.length).toBe(1);
    expect(result.stepSummaries[0]).toContain("learned");
    expect(result.stepSummaries[0]).toContain("forgotten");

    // The synthesized fact should exist
    const synthExists = await repo.fileExists("worlds/security/libfoo-vulns.md");
    expect(synthExists).toBe(true);

    // The subsumed facts should be deleted
    const cve1Exists = await repo.fileExists("worlds/security/cve-1.md");
    expect(cve1Exists).toBe(false);
    const cve2Exists = await repo.fileExists("worlds/security/cve-2.md");
    expect(cve2Exists).toBe(false);

    // Back on original branch
    const branch = await repo.currentBranch();
    expect(branch).toBe("machine/test-machine");

    // Synthesis log recorded
    const logEntry = searchIndex.getSynthesisLog("test-distill");
    expect(logEntry).not.toBeNull();
  });

  it("multi-step recipe runs prune then distill", async () => {
    // First call: prune response (keep both facts)
    // Second call: distill response (synthesize a new one)
    llmResponses = [
      JSON.stringify({
        decisions: [
          { file: "worlds/security/cve-1.md", action: "keep", reason: "current" },
          { file: "worlds/security/cve-2.md", action: "keep", reason: "current" },
        ],
        merges: [],
        summary: "All facts current",
      }),
      JSON.stringify({
        synthesize: [
          {
            path: "worlds/security/libfoo-summary.md",
            title: "libfoo vulnerability pattern",
            body: "Pattern of buffer overflows in libfoo.",
            domain: ["security"],
            confidence: 0.85,
            entities: ["libfoo"],
            refs: ["worlds/security/cve-1.md", "worlds/security/cve-2.md"],
          },
        ],
        forget: [],
        summary: "Found 1 pattern",
      }),
    ];

    const recipe: Recipe = {
      name: "test-multi",
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
    expect(result.stepSummaries.length).toBe(2);

    // Both original facts kept (prune kept them) plus new synthesized fact
    const cve1 = await repo.fileExists("worlds/security/cve-1.md");
    expect(cve1).toBe(true);
    const cve2 = await repo.fileExists("worlds/security/cve-2.md");
    expect(cve2).toBe(true);
    const summary = await repo.fileExists("worlds/security/libfoo-summary.md");
    expect(summary).toBe(true);

    expect(llmCallCount).toBe(2);
  });
});
