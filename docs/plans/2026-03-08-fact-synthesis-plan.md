# Fact Synthesis Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a `knomit synthesize` CLI subcommand that loads recipe files, queries facts, sends them to an LLM for prune/distill decisions, and executes the results on a git branch with proper learning moment tags.

**Architecture:** Three new modules: `src/llm.ts` (multi-provider LLM adapter), `src/synthesize.ts` (core synthesis engine), `src/cli/synthesize.ts` (citty subcommand). Refactor existing tool handlers to extract core commit/tag logic into reusable functions. Recipes stored in `<repo>/.knomit/synthesize/*.yml`.

**Tech Stack:** Bun, TypeScript, citty, yaml, raw `fetch` for LLM APIs (Anthropic, Gemini, AWS Bedrock)

---

### Task 1: LLM adapter module

**Files:**
- Create: `src/llm.ts`
- Create: `src/llm.test.ts`

**Step 1: Write tests for adapter creation and provider resolution**

```ts
// src/llm.test.ts
import { describe, it, expect } from "bun:test";
import { createAdapter, resolveProvider } from "./llm";

describe("resolveProvider", () => {
  it("resolves provider from model name", () => {
    expect(resolveProvider("claude-sonnet-4-6")).toBe("anthropic");
    expect(resolveProvider("gemini-2.0-flash")).toBe("gemini");
    expect(resolveProvider("us.anthropic.claude-sonnet-4-6-v1")).toBe("bedrock");
  });

  it("returns explicit provider when given", () => {
    expect(resolveProvider("my-model", "anthropic")).toBe("anthropic");
  });

  it("throws on unknown model without provider", () => {
    expect(() => resolveProvider("unknown-model")).toThrow();
  });
});

describe("createAdapter", () => {
  it("creates an anthropic adapter", () => {
    const adapter = createAdapter({
      provider: "anthropic",
      model: "claude-sonnet-4-6",
      apiKey: "sk-test",
    });
    expect(adapter).toBeDefined();
    expect(typeof adapter.complete).toBe("function");
  });

  it("creates a gemini adapter", () => {
    const adapter = createAdapter({
      provider: "gemini",
      model: "gemini-2.0-flash",
      apiKey: "AI-test",
    });
    expect(adapter).toBeDefined();
  });

  it("creates a bedrock adapter", () => {
    const adapter = createAdapter({
      provider: "bedrock",
      model: "us.anthropic.claude-sonnet-4-6-v1",
      region: "us-east-1",
      accessKeyId: "AKIA-test",
      secretAccessKey: "secret-test",
    });
    expect(adapter).toBeDefined();
  });

  it("throws without api key for anthropic", () => {
    expect(() =>
      createAdapter({ provider: "anthropic", model: "claude-sonnet-4-6" })
    ).toThrow();
  });
});
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test llm.test.ts`
Expected: FAIL — module not found

**Step 3: Implement the LLM adapter**

```ts
// src/llm.ts
export interface Message {
  role: "user" | "assistant";
  content: string;
}

export interface LLMAdapter {
  complete(system: string, messages: Message[]): Promise<string>;
}

export interface LLMConfig {
  provider?: "anthropic" | "gemini" | "bedrock";
  model: string;
  apiKey?: string;
  region?: string;
  accessKeyId?: string;
  secretAccessKey?: string;
}

export function resolveProvider(
  model: string,
  explicit?: string
): "anthropic" | "gemini" | "bedrock" {
  if (explicit) return explicit as "anthropic" | "gemini" | "bedrock";
  if (model.startsWith("claude")) return "anthropic";
  if (model.startsWith("gemini")) return "gemini";
  if (model.includes("anthropic.") || model.startsWith("us.") || model.startsWith("eu."))
    return "bedrock";
  throw new Error(
    `Cannot infer provider for model "${model}". Set KNOMIT_LLM_PROVIDER or specify provider explicitly.`
  );
}

export function createAdapter(config: LLMConfig): LLMAdapter {
  const provider = resolveProvider(config.model, config.provider);
  switch (provider) {
    case "anthropic":
      return createAnthropicAdapter(config);
    case "gemini":
      return createGeminiAdapter(config);
    case "bedrock":
      return createBedrockAdapter(config);
  }
}

function createAnthropicAdapter(config: LLMConfig): LLMAdapter {
  const apiKey = config.apiKey ?? process.env.ANTHROPIC_API_KEY;
  if (!apiKey) throw new Error("ANTHROPIC_API_KEY is required for Anthropic provider");
  const model = config.model;

  return {
    async complete(system: string, messages: Message[]): Promise<string> {
      const resp = await fetch("https://api.anthropic.com/v1/messages", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-api-key": apiKey,
          "anthropic-version": "2023-06-01",
        },
        body: JSON.stringify({
          model,
          max_tokens: 8192,
          system,
          messages: messages.map((m) => ({
            role: m.role,
            content: m.content,
          })),
        }),
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`Anthropic API error ${resp.status}: ${body}`);
      }
      const data = (await resp.json()) as {
        content: Array<{ type: string; text: string }>;
      };
      const textBlock = data.content.find((c) => c.type === "text");
      if (!textBlock) throw new Error("No text in Anthropic response");
      return textBlock.text;
    },
  };
}

function createGeminiAdapter(config: LLMConfig): LLMAdapter {
  const apiKey = config.apiKey ?? process.env.GOOGLE_AI_API_KEY;
  if (!apiKey) throw new Error("GOOGLE_AI_API_KEY is required for Gemini provider");
  const model = config.model;

  return {
    async complete(system: string, messages: Message[]): Promise<string> {
      const contents = messages.map((m) => ({
        role: m.role === "assistant" ? "model" : "user",
        parts: [{ text: m.content }],
      }));
      const resp = await fetch(
        `https://generativelanguage.googleapis.com/v1beta/models/${model}:generateContent?key=${apiKey}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            system_instruction: { parts: [{ text: system }] },
            contents,
          }),
        }
      );
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`Gemini API error ${resp.status}: ${body}`);
      }
      const data = (await resp.json()) as {
        candidates: Array<{ content: { parts: Array<{ text: string }> } }>;
      };
      return data.candidates[0].content.parts[0].text;
    },
  };
}

function createBedrockAdapter(config: LLMConfig): LLMAdapter {
  const region = config.region ?? process.env.AWS_REGION ?? "us-east-1";
  const accessKeyId = config.accessKeyId ?? process.env.AWS_ACCESS_KEY_ID;
  const secretAccessKey = config.secretAccessKey ?? process.env.AWS_SECRET_ACCESS_KEY;
  if (!accessKeyId || !secretAccessKey)
    throw new Error("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required for Bedrock provider");
  const model = config.model;

  return {
    async complete(system: string, messages: Message[]): Promise<string> {
      const url = `https://bedrock-runtime.${region}.amazonaws.com/model/${encodeURIComponent(model)}/invoke`;
      const payload = JSON.stringify({
        anthropic_version: "bedrock-2023-05-31",
        max_tokens: 8192,
        system,
        messages: messages.map((m) => ({
          role: m.role,
          content: m.content,
        })),
      });

      const headers = await signAwsRequest(
        "POST",
        url,
        payload,
        region,
        accessKeyId,
        secretAccessKey
      );

      const resp = await fetch(url, {
        method: "POST",
        headers: { ...headers, "Content-Type": "application/json" },
        body: payload,
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`Bedrock API error ${resp.status}: ${body}`);
      }
      const data = (await resp.json()) as {
        content: Array<{ type: string; text: string }>;
      };
      const textBlock = data.content.find((c) => c.type === "text");
      if (!textBlock) throw new Error("No text in Bedrock response");
      return textBlock.text;
    },
  };
}

/** Minimal AWS Signature V4 signing for Bedrock requests. */
async function signAwsRequest(
  method: string,
  url: string,
  body: string,
  region: string,
  accessKeyId: string,
  secretAccessKey: string
): Promise<Record<string, string>> {
  const encoder = new TextEncoder();
  const parsed = new URL(url);
  const now = new Date();
  const amzDate = now.toISOString().replace(/[-:]/g, "").replace(/\.\d{3}/, "");
  const dateStamp = amzDate.slice(0, 8);
  const service = "bedrock";

  const sha256 = async (data: string | Uint8Array): Promise<string> => {
    const buf = typeof data === "string" ? encoder.encode(data) : data;
    const hash = new Bun.CryptoHasher("sha256").update(buf).digest("hex");
    return hash;
  };

  const hmac = async (
    key: string | Uint8Array,
    data: string
  ): Promise<Uint8Array> => {
    const keyBuf = typeof key === "string" ? encoder.encode(key) : key;
    const hasher = new Bun.CryptoHasher("sha256", keyBuf);
    hasher.update(encoder.encode(data));
    return new Uint8Array(hasher.digest());
  };

  const payloadHash = await sha256(body);
  const canonicalHeaders =
    `host:${parsed.host}\nx-amz-date:${amzDate}\n`;
  const signedHeaders = "host;x-amz-date";
  const canonicalRequest = [
    method,
    parsed.pathname,
    "",
    canonicalHeaders,
    signedHeaders,
    payloadHash,
  ].join("\n");

  const credentialScope = `${dateStamp}/${region}/${service}/aws4_request`;
  const stringToSign = [
    "AWS4-HMAC-SHA256",
    amzDate,
    credentialScope,
    await sha256(canonicalRequest),
  ].join("\n");

  const kDate = await hmac(`AWS4${secretAccessKey}`, dateStamp);
  const kRegion = await hmac(kDate, region);
  const kService = await hmac(kRegion, service);
  const kSigning = await hmac(kService, "aws4_request");

  const signatureHasher = new Bun.CryptoHasher("sha256", kSigning);
  signatureHasher.update(encoder.encode(stringToSign));
  const signature = signatureHasher.digest("hex");

  return {
    "X-Amz-Date": amzDate,
    Authorization: `AWS4-HMAC-SHA256 Credential=${accessKeyId}/${credentialScope}, SignedHeaders=${signedHeaders}, Signature=${signature}`,
    "x-amz-content-sha256": payloadHash,
  };
}

/** Resolve LLM config from environment variables. */
export function configFromEnv(): LLMConfig {
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
```

**Step 4: Run tests to verify they pass**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test llm.test.ts`
Expected: PASS

**Step 5: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test`
Expected: all pass

**Step 6: Commit**

```bash
git add src/llm.ts src/llm.test.ts
git commit -m "feat: add multi-provider LLM adapter (anthropic, gemini, bedrock)"
```

---

### Task 2: Extract core fact operations from tool handlers

The existing `learnHandler`, `forgetHandler`, `updateHandler` in `src/tools/` call `sync()` and `push()` internally. Synthesis needs the core logic without those. Extract reusable functions.

**Files:**
- Create: `src/fact-ops.ts`
- Create: `src/fact-ops.test.ts`
- Modify: `src/tools/learn.ts` — use extracted functions
- Modify: `src/tools/forget.ts` — use extracted functions
- Modify: `src/tools/update.ts` — use extracted functions

**Step 1: Write tests for core fact operations**

```ts
// src/fact-ops.test.ts
import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { GitRepo } from "./git";
import { commitFact, deleteFact, updateFact } from "./fact-ops";

describe("fact-ops", () => {
  let repoPath: string;
  let repo: GitRepo;

  beforeEach(async () => {
    repoPath = await mkdtemp(join(tmpdir(), "factops-"));
    repo = new GitRepo(repoPath, "test-machine");
    await repo.init();
  });

  afterEach(async () => {
    await rm(repoPath, { recursive: true, force: true });
  });

  it("commitFact creates a fact file and commits", async () => {
    const hash = await commitFact(repo, {
      path: "know/test/fact1.md",
      title: "Test fact",
      body: "Some body",
      domain: ["testing"],
      confidence: 0.9,
      sources: 1,
      entities: ["test"],
      refs: [],
    });
    expect(hash).toBeTruthy();
    const content = await repo.readFile("know/test/fact1.md");
    expect(content).toContain("Test fact");
  });

  it("deleteFact removes a fact file and commits", async () => {
    await commitFact(repo, {
      path: "know/test/fact1.md",
      title: "To delete",
      body: "Body",
      domain: [],
      confidence: 0.5,
      sources: 1,
      entities: [],
      refs: [],
    });
    const hash = await deleteFact(repo, "know/test/fact1.md", "test-moment");
    expect(hash).toBeTruthy();
    const exists = await repo.fileExists("know/test/fact1.md");
    expect(exists).toBe(false);
  });

  it("updateFact modifies frontmatter and commits", async () => {
    await commitFact(repo, {
      path: "know/test/fact1.md",
      title: "Original",
      body: "Body",
      domain: ["a"],
      confidence: 0.5,
      sources: 1,
      entities: [],
      refs: [],
    });
    const hash = await updateFact(repo, "know/test/fact1.md", {
      confidence: 0.9,
    });
    expect(hash).toBeTruthy();
    const content = await repo.readFile("know/test/fact1.md");
    expect(content).toContain("confidence: 0.9");
  });
});
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test fact-ops.test.ts`
Expected: FAIL — module not found

**Step 3: Implement core fact operations**

```ts
// src/fact-ops.ts
import type { GitRepo } from "./git";
import type { SearchIndex } from "./search-index";
import { serializeFact, parseFact, mergeFrontmatter } from "./facts";

export interface FactData {
  path: string;
  title: string;
  body: string;
  domain: string[];
  confidence: number;
  sources: number;
  entities: string[];
  refs: string[];
}

/** Create or overwrite a fact file and commit. Returns the commit hash. */
export async function commitFact(
  repo: GitRepo,
  fact: FactData,
  searchIndex?: SearchIndex
): Promise<string> {
  let factPath = fact.path;
  if (!factPath.startsWith("know/")) factPath = `know/${factPath}`;
  if (!factPath.endsWith(".md")) factPath = `${factPath}.md`;

  const content = serializeFact(
    {
      domain: fact.domain,
      confidence: fact.confidence,
      sources: fact.sources,
      entities: fact.entities,
      refs: fact.refs,
    },
    fact.title,
    fact.body
  );

  const hash = await repo.commit(
    [{ path: factPath, content }],
    `learn: ${fact.title}`
  );

  await searchIndex?.upsert(factPath, {
    title: fact.title,
    body: fact.body,
    domain: fact.domain,
    entities: fact.entities,
    confidence: fact.confidence,
    sources: fact.sources,
    refs: fact.refs,
    commitHash: hash,
  });

  return hash;
}

/** Delete a fact file and commit. Returns the commit hash. */
export async function deleteFact(
  repo: GitRepo,
  file: string,
  momentName: string,
  searchIndex?: SearchIndex
): Promise<string> {
  const hash = await repo.deleteFile(
    file,
    `forget(${momentName}): ${file}`
  );
  searchIndex?.remove(file);
  return hash;
}

/** Update a fact's frontmatter/content and commit. Returns the commit hash. */
export async function updateFact(
  repo: GitRepo,
  file: string,
  updates: {
    confidence?: number;
    sources?: number;
    body?: string;
    title?: string;
    refs?: string[];
    domain?: string[];
    entities?: string[];
  },
  searchIndex?: SearchIndex
): Promise<string> {
  const content = await repo.readFile(file);
  const parsed = parseFact(content);
  const newFrontmatter = mergeFrontmatter(parsed.frontmatter, updates);
  const newTitle = updates.title ?? parsed.title;
  const newBody = updates.body ?? parsed.body;
  const serialized = serializeFact(newFrontmatter, newTitle, newBody);

  const hash = await repo.commit(
    [{ path: file, content: serialized }],
    `update: ${newTitle}`
  );

  await searchIndex?.upsert(file, {
    title: newTitle,
    body: newBody,
    domain: newFrontmatter.domain,
    entities: newFrontmatter.entities,
    confidence: newFrontmatter.confidence,
    sources: newFrontmatter.sources,
    refs: newFrontmatter.refs,
    commitHash: hash,
  });

  return hash;
}
```

**Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test fact-ops.test.ts`
Expected: PASS

**Step 5: Refactor tool handlers to use core functions**

In `src/tools/learn.ts`, replace lines 44-84 (the for loop) with:

```ts
  const { commitFact } = await import("../fact-ops.js");

  for (const fact of input.facts) {
    let factPath = fact.path;
    if (!factPath.startsWith("know/")) factPath = `know/${factPath}`;
    if (!factPath.endsWith(".md")) factPath = `${factPath}.md`;

    const hash = await commitFact(repo, {
      path: factPath,
      title: fact.title,
      body: fact.body,
      domain: fact.domain,
      confidence: fact.confidence,
      sources: fact.sources,
      entities: fact.entities,
      refs: fact.refs ?? [],
    }, searchIndex);

    log.debug(`learn: committed ${factPath} as ${hash}`);
    commits.push({ file: factPath, hash });
  }
```

In `src/tools/forget.ts`, replace lines 34-39 with:

```ts
  const { deleteFact } = await import("../fact-ops.js");
  const commit = await deleteFact(repo, input.file, input.moment_name, searchIndex);
```

In `src/tools/update.ts`, replace lines 40-71 with:

```ts
  const { updateFact } = await import("../fact-ops.js");
  const fileExists = await repo.fileExists(input.file);
  if (!fileExists) throw new Error(`File not found: ${input.file}`);

  const commit = await updateFact(repo, input.file, input.updates, searchIndex);
```

**Step 6: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test`
Expected: all pass — refactored handlers behave identically

**Step 7: Commit**

```bash
git add src/fact-ops.ts src/fact-ops.test.ts src/tools/learn.ts src/tools/forget.ts src/tools/update.ts
git commit -m "refactor: extract core fact operations from tool handlers"
```

---

### Task 3: Recipe loading and validation

**Files:**
- Create: `src/recipe.ts`
- Create: `src/recipe.test.ts`

**Step 1: Write tests for recipe parsing**

```ts
// src/recipe.test.ts
import { describe, it, expect } from "bun:test";
import { parseRecipe, type Recipe } from "./recipe";

describe("parseRecipe", () => {
  it("parses a valid recipe", () => {
    const yaml = `
name: cve-review
prompt: "Review CVEs for staleness"
scope:
  domain: [security]
  entities: [acme]
  search: ["patch"]
  path: know/security/
auto_merge: false
steps:
  - mode: prune
    model: gemini-2.0-flash
    prompt: "Find stale CVEs"
  - mode: distill
    model: claude-sonnet-4-6
    prompt: "Find patterns"
`;
    const recipe = parseRecipe(yaml);
    expect(recipe.name).toBe("cve-review");
    expect(recipe.steps).toHaveLength(2);
    expect(recipe.steps[0].mode).toBe("prune");
    expect(recipe.steps[0].model).toBe("gemini-2.0-flash");
    expect(recipe.steps[1].mode).toBe("distill");
    expect(recipe.scope.domain).toEqual(["security"]);
    expect(recipe.scope.entities).toEqual(["acme"]);
    expect(recipe.scope.search).toEqual(["patch"]);
    expect(recipe.scope.path).toBe("know/security/");
    expect(recipe.auto_merge).toBe(false);
  });

  it("defaults auto_merge to false", () => {
    const yaml = `
name: test
prompt: ""
steps:
  - mode: prune
`;
    const recipe = parseRecipe(yaml);
    expect(recipe.auto_merge).toBe(false);
  });

  it("throws on missing name", () => {
    const yaml = `
steps:
  - mode: prune
`;
    expect(() => parseRecipe(yaml)).toThrow();
  });

  it("throws on invalid mode", () => {
    const yaml = `
name: test
steps:
  - mode: invalid
`;
    expect(() => parseRecipe(yaml)).toThrow();
  });

  it("defaults empty scope fields", () => {
    const yaml = `
name: test
scope:
  domain: []
steps:
  - mode: prune
`;
    const recipe = parseRecipe(yaml);
    expect(recipe.scope!.domain).toEqual([]);
    expect(recipe.scope!.entities).toEqual([]);
    expect(recipe.scope!.search).toEqual([]);
    expect(recipe.scope!.path).toBe("");
  });

  it("allows omitting scope entirely for auto-discovery", () => {
    const yaml = `
name: auto-test
steps:
  - mode: prune
`;
    const recipe = parseRecipe(yaml);
    expect(recipe.scope).toBeUndefined();
  });
});
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test recipe.test.ts`
Expected: FAIL — module not found

**Step 3: Implement recipe parsing**

```ts
// src/recipe.ts
import { parse as parseYaml } from "yaml";
import { z } from "zod";

const StepSchema = z.object({
  mode: z.enum(["prune", "distill"]),
  model: z.string().optional(),
  prompt: z.string().optional().default(""),
});

const ScopeSchema = z.object({
  domain: z.array(z.string()).optional().default([]),
  entities: z.array(z.string()).optional().default([]),
  search: z.array(z.string()).optional().default([]),
  path: z.string().optional().default(""),
});

const RecipeSchema = z.object({
  name: z.string().min(1),
  prompt: z.string().optional().default(""),
  scope: ScopeSchema.optional(), // undefined = auto-discovery mode
  auto_merge: z.boolean().optional().default(false),
  steps: z.array(StepSchema).min(1),
});

export type Recipe = z.infer<typeof RecipeSchema>;
export type RecipeStep = z.infer<typeof StepSchema>;

export function parseRecipe(raw: string): Recipe {
  const parsed = parseYaml(raw);
  return RecipeSchema.parse(parsed);
}
```

**Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test recipe.test.ts`
Expected: PASS

**Step 5: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test`
Expected: all pass

**Step 6: Commit**

```bash
git add src/recipe.ts src/recipe.test.ts
git commit -m "feat: add recipe YAML parsing with zod validation"
```

---

### Task 4: Synthesis engine

**Files:**
- Create: `src/synthesize.ts`
- Create: `src/synthesize.test.ts`

**Step 1: Write tests for the synthesis engine**

Focus on testable parts: prompt building, response parsing, chunking. Don't test actual LLM calls.

```ts
// src/synthesize.test.ts
import { describe, it, expect } from "bun:test";
import {
  buildPrunePrompt,
  buildDistillPrompt,
  parsePruneResponse,
  parseDistillResponse,
  chunkFacts,
} from "./synthesize";

describe("buildPrunePrompt", () => {
  it("includes facts and recipe prompt", () => {
    const facts = [
      { path: "know/test.md", title: "Test", body: "Body", domain: ["d"], entities: ["e"], confidence: 0.8, sources: 1, refs: [] },
    ];
    const prompt = buildPrunePrompt(facts, "Focus on security", "Find stale facts");
    expect(prompt).toContain("Test");
    expect(prompt).toContain("Focus on security");
    expect(prompt).toContain("Find stale facts");
    expect(prompt).toContain("know/test.md");
  });
});

describe("parsePruneResponse", () => {
  it("parses valid prune JSON", () => {
    const json = JSON.stringify({
      decisions: [
        { file: "know/a.md", action: "forget", reason: "stale" },
        { file: "know/b.md", action: "keep", reason: "current" },
      ],
      merges: [],
      summary: "Pruned 1",
    });
    const result = parsePruneResponse(json);
    expect(result.decisions).toHaveLength(2);
    expect(result.decisions[0].action).toBe("forget");
  });

  it("extracts JSON from markdown code blocks", () => {
    const wrapped = '```json\n{"decisions":[],"merges":[],"summary":"ok"}\n```';
    const result = parsePruneResponse(wrapped);
    expect(result.summary).toBe("ok");
  });
});

describe("parseDistillResponse", () => {
  it("parses valid distill JSON", () => {
    const json = JSON.stringify({
      synthesize: [{
        path: "know/new.md",
        title: "Pattern",
        body: "Insight",
        domain: ["d"],
        confidence: 0.8,
        entities: ["e"],
        refs: ["know/old1.md"],
      }],
      forget: ["know/old1.md"],
      summary: "Distilled 1",
    });
    const result = parseDistillResponse(json);
    expect(result.synthesize).toHaveLength(1);
    expect(result.forget).toContain("know/old1.md");
  });
});

describe("chunkFacts", () => {
  it("returns one chunk when facts fit", () => {
    const facts = Array.from({ length: 5 }, (_, i) => ({
      path: `know/f${i}.md`, title: `F${i}`, body: "short",
      domain: [], entities: [], confidence: 0.8, sources: 1, refs: [],
    }));
    const chunks = chunkFacts(facts, 100_000);
    expect(chunks).toHaveLength(1);
  });

  it("splits into multiple chunks when facts exceed budget", () => {
    const facts = Array.from({ length: 100 }, (_, i) => ({
      path: `know/f${i}.md`, title: `Fact ${i}`, body: "x".repeat(1000),
      domain: [], entities: [], confidence: 0.8, sources: 1, refs: [],
    }));
    const chunks = chunkFacts(facts, 10_000);
    expect(chunks.length).toBeGreaterThan(1);
  });
});
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test synthesize.test.ts`
Expected: FAIL — module not found

**Step 3: Implement the synthesis engine**

```ts
// src/synthesize.ts
import type { GitRepo } from "./git";
import type { SearchIndex, SearchResult } from "./search-index";
import type { Recipe, RecipeStep } from "./recipe";
import type { LLMAdapter } from "./llm";
import { createAdapter, resolveProvider, configFromEnv } from "./llm";
import { commitFact, deleteFact, updateFact } from "./fact-ops";
import { toMomentTag } from "./git";
import { log } from "./logger";

export interface FactForLLM {
  path: string;
  title: string;
  body: string;
  domain: string[];
  entities: string[];
  confidence: number;
  sources: number;
  refs: string[];
}

// --- Prune types ---

export interface PruneDecision {
  file: string;
  action: "keep" | "forget" | "update";
  confidence?: number;
  reason: string;
}

export interface PruneMerge {
  sources: string[];
  merged: {
    path: string;
    title: string;
    body: string;
    domain: string[];
    confidence: number;
    entities: string[];
    refs: string[];
  };
}

export interface PruneResult {
  decisions: PruneDecision[];
  merges: PruneMerge[];
  summary: string;
}

// --- Distill types ---

export interface DistillFact {
  path: string;
  title: string;
  body: string;
  domain: string[];
  confidence: number;
  entities: string[];
  refs: string[];
}

export interface DistillResult {
  synthesize: DistillFact[];
  forget: string[];
  summary: string;
}

// --- Prompt builders ---

export function buildPrunePrompt(
  facts: FactForLLM[],
  recipePrompt: string,
  stepPrompt: string
): string {
  const factsJson = JSON.stringify(
    facts.map((f) => ({
      file: f.path,
      title: f.title,
      body: f.body,
      domain: f.domain,
      entities: f.entities,
      confidence: f.confidence,
      sources: f.sources,
    })),
    null,
    2
  );

  return `You are reviewing facts in a knowledge base for staleness, redundancy, and duplication.

${recipePrompt ? `Context: ${recipePrompt}\n` : ""}${stepPrompt ? `Instructions: ${stepPrompt}\n` : ""}
Facts to review:
${factsJson}

For each fact, decide:
- KEEP: fact is current and valuable
- FORGET: fact is obsolete, superseded, or no longer true
- UPDATE: fact needs confidence adjusted (provide new value and reason)

Also identify facts that say the same thing and should be merged into a single unified fact.

Respond as JSON (no markdown wrapping):
{
  "decisions": [
    { "file": "...", "action": "keep|forget|update", "confidence": 0.X, "reason": "..." }
  ],
  "merges": [
    {
      "sources": ["file1.md", "file2.md"],
      "merged": {
        "path": "know/...",
        "title": "...",
        "body": "...",
        "domain": [...],
        "confidence": 0.X,
        "entities": [...],
        "refs": ["file1.md", "file2.md"]
      }
    }
  ],
  "summary": "one sentence summary of what changed"
}`;
}

export function buildDistillPrompt(
  facts: FactForLLM[],
  recipePrompt: string,
  stepPrompt: string
): string {
  const factsJson = JSON.stringify(
    facts.map((f) => ({
      file: f.path,
      title: f.title,
      body: f.body,
      domain: f.domain,
      entities: f.entities,
      confidence: f.confidence,
      sources: f.sources,
    })),
    null,
    2
  );

  return `You are synthesizing facts in a knowledge base to find patterns and higher-order insights.

${recipePrompt ? `Context: ${recipePrompt}\n` : ""}${stepPrompt ? `Instructions: ${stepPrompt}\n` : ""}
Facts in scope:
${factsJson}

Identify patterns across these facts. Produce:
1. New higher-order facts that capture patterns (with refs to source fact files)
2. Which original facts are fully subsumed and can be forgotten

Respond as JSON (no markdown wrapping):
{
  "synthesize": [
    {
      "path": "know/...",
      "title": "...",
      "body": "...",
      "domain": [...],
      "confidence": 0.X,
      "entities": [...],
      "refs": ["source-file1.md", "source-file2.md"]
    }
  ],
  "forget": ["file1.md", "file2.md"],
  "summary": "one sentence summary"
}`;
}

// --- Response parsers ---

function extractJson(text: string): string {
  // Strip markdown code blocks if present
  const match = text.match(/```(?:json)?\s*\n?([\s\S]*?)\n?```/);
  return match ? match[1].trim() : text.trim();
}

export function parsePruneResponse(text: string): PruneResult {
  const json = extractJson(text);
  const parsed = JSON.parse(json);
  return {
    decisions: parsed.decisions ?? [],
    merges: parsed.merges ?? [],
    summary: parsed.summary ?? "",
  };
}

export function parseDistillResponse(text: string): DistillResult {
  const json = extractJson(text);
  const parsed = JSON.parse(json);
  return {
    synthesize: parsed.synthesize ?? [],
    forget: parsed.forget ?? [],
    summary: parsed.summary ?? "",
  };
}

// --- Chunking ---

export function chunkFacts(
  facts: FactForLLM[],
  maxCharsPerChunk: number
): FactForLLM[][] {
  const chunks: FactForLLM[][] = [];
  let current: FactForLLM[] = [];
  let currentSize = 0;

  for (const fact of facts) {
    const factSize = JSON.stringify(fact).length;
    if (currentSize + factSize > maxCharsPerChunk && current.length > 0) {
      chunks.push(current);
      current = [];
      currentSize = 0;
    }
    current.push(fact);
    currentSize += factSize;
  }
  if (current.length > 0) chunks.push(current);
  return chunks;
}

// --- Fact gathering ---

async function gatherFacts(
  searchIndex: SearchIndex,
  scope: Recipe["scope"]
): Promise<FactForLLM[]> {
  if (!scope) {
    throw new Error("gatherFacts requires explicit scope. Use gatherFactsByDelta for auto-discovery.");
  }

  const allFacts: Map<string, FactForLLM> = new Map();

  // Primary query by domain/entities/path
  const baseResults = await searchIndex.search({
    domain: scope.domain.length > 0 ? scope.domain : undefined,
    entities: scope.entities.length > 0 ? scope.entities : undefined,
    path: scope.path || undefined,
    limit: 10_000,
  });
  for (const r of baseResults) {
    allFacts.set(r.path, searchResultToFact(r));
  }

  // Additional search queries
  for (const query of scope.search) {
    const results = await searchIndex.search({
      text: query,
      domain: scope.domain.length > 0 ? scope.domain : undefined,
      entities: scope.entities.length > 0 ? scope.entities : undefined,
      path: scope.path || undefined,
      limit: 10_000,
    });
    for (const r of results) {
      allFacts.set(r.path, searchResultToFact(r));
    }
  }

  return [...allFacts.values()];
}

/** Auto-discovery: gather facts that changed since last synthesis run. */
async function gatherFactsByDelta(
  repo: GitRepo,
  searchIndex: SearchIndex,
  recipeName: string
): Promise<FactForLLM[]> {
  const lastRun = searchIndex.getSynthesisLog(recipeName);
  if (!lastRun) {
    // First run — gather all facts
    log.info(`auto-discovery: first run for "${recipeName}", gathering all facts`);
    const results = await searchIndex.search({ limit: 100_000 });
    return results.map(searchResultToFact);
  }

  log.info(`auto-discovery: finding changes since ${lastRun.lastCommit.slice(0, 7)}`);
  const diff = await repo.diffFiles(lastRun.lastCommit);
  const changedPaths = [...diff.added, ...diff.modified].filter((f) => f.endsWith(".md"));

  if (changedPaths.length === 0) {
    log.info("auto-discovery: no changes since last run");
    return [];
  }

  const facts: FactForLLM[] = [];
  for (const path of changedPaths) {
    try {
      const content = await repo.readFile(path);
      const parsed = (await import("./facts.js")).parseFact(content);
      facts.push({
        path,
        title: parsed.title,
        body: parsed.body,
        domain: parsed.frontmatter.domain,
        entities: parsed.frontmatter.entities,
        confidence: parsed.frontmatter.confidence,
        sources: parsed.frontmatter.sources,
        refs: parsed.frontmatter.refs,
      });
    } catch {
      // Skip files that fail to parse
    }
  }

  log.info(`auto-discovery: ${facts.length} changed facts since last run`);
  return facts;
}

function searchResultToFact(r: SearchResult): FactForLLM {
  return {
    path: r.path,
    title: r.title,
    body: r.body,
    domain: r.domain,
    entities: r.entities,
    confidence: r.confidence,
    sources: r.sources,
    refs: r.refs,
  };
}

// --- Step execution ---

function adapterForStep(step: RecipeStep): LLMAdapter {
  if (step.model) {
    const envConfig = configFromEnv();
    const provider = resolveProvider(step.model, envConfig.provider);
    return createAdapter({ ...envConfig, provider, model: step.model });
  }
  return createAdapter(configFromEnv());
}

async function executePruneStep(
  repo: GitRepo,
  searchIndex: SearchIndex,
  recipe: Recipe,
  step: RecipeStep,
  recipeName: string
): Promise<string> {
  const facts = recipe.scope
    ? await gatherFacts(searchIndex, recipe.scope)
    : await gatherFactsByDelta(repo, searchIndex, recipeName);
  if (facts.length === 0) return "No facts found in scope.";

  const adapter = adapterForStep(step);
  const chunks = chunkFacts(facts, 100_000);
  const allDecisions: PruneDecision[] = [];
  const allMerges: PruneMerge[] = [];
  const summaries: string[] = [];

  for (const chunk of chunks) {
    const prompt = buildPrunePrompt(chunk, recipe.prompt, step.prompt ?? "");
    log.info(`prune: sending ${chunk.length} facts to LLM`);
    const response = await adapter.complete(
      "You are a knowledge base maintenance assistant. Respond only with valid JSON.",
      [{ role: "user", content: prompt }]
    );
    const result = parsePruneResponse(response);
    allDecisions.push(...result.decisions);
    allMerges.push(...result.merges);
    summaries.push(result.summary);
  }

  // Execute decisions
  let forgotten = 0;
  let updated = 0;
  let merged = 0;

  for (const decision of allDecisions) {
    if (decision.action === "forget") {
      try {
        await deleteFact(repo, decision.file, `synthesize-${recipeName}`, searchIndex);
        forgotten++;
      } catch (err) {
        log.warn(`prune: failed to delete ${decision.file}: ${err}`);
      }
    } else if (decision.action === "update" && decision.confidence != null) {
      try {
        await updateFact(repo, decision.file, { confidence: decision.confidence }, searchIndex);
        updated++;
      } catch (err) {
        log.warn(`prune: failed to update ${decision.file}: ${err}`);
      }
    }
  }

  // Execute merges
  for (const merge of allMerges) {
    try {
      await commitFact(repo, {
        path: merge.merged.path,
        title: merge.merged.title,
        body: merge.merged.body,
        domain: merge.merged.domain,
        confidence: merge.merged.confidence,
        sources: 1,
        entities: merge.merged.entities,
        refs: merge.merged.refs,
      }, searchIndex);
      for (const source of merge.sources) {
        await deleteFact(repo, source, `synthesize-${recipeName}`, searchIndex);
      }
      merged++;
    } catch (err) {
      log.warn(`prune: failed to merge: ${err}`);
    }
  }

  await repo.tag(toMomentTag(`synthesize-${recipeName}-prune`));
  const summary = `Prune: ${forgotten} forgotten, ${updated} updated, ${merged} merged. ${summaries.join(" ")}`;
  log.info(summary);
  return summary;
}

async function executeDistillStep(
  repo: GitRepo,
  searchIndex: SearchIndex,
  recipe: Recipe,
  step: RecipeStep,
  recipeName: string
): Promise<string> {
  const facts = recipe.scope
    ? await gatherFacts(searchIndex, recipe.scope)
    : await gatherFactsByDelta(repo, searchIndex, recipeName);
  if (facts.length === 0) return "No facts found in scope.";

  const adapter = adapterForStep(step);
  const chunks = chunkFacts(facts, 100_000);
  const allSynthesized: DistillFact[] = [];
  const allForget: string[] = [];
  const summaries: string[] = [];

  for (const chunk of chunks) {
    const prompt = buildDistillPrompt(chunk, recipe.prompt, step.prompt ?? "");
    log.info(`distill: sending ${chunk.length} facts to LLM`);
    const response = await adapter.complete(
      "You are a knowledge base synthesis assistant. Respond only with valid JSON.",
      [{ role: "user", content: prompt }]
    );
    const result = parseDistillResponse(response);
    allSynthesized.push(...result.synthesize);
    allForget.push(...result.forget);
    summaries.push(result.summary);
  }

  // Cross-chunk pass if multiple chunks
  if (chunks.length > 1 && allSynthesized.length > 0) {
    const crossPrompt = buildDistillPrompt(
      allSynthesized.map((s) => ({
        ...s,
        sources: 1,
      })),
      recipe.prompt,
      "These are synthesized facts from multiple batches. Find cross-cutting patterns and further consolidate if possible."
    );
    const adapter2 = adapterForStep(step);
    const crossResponse = await adapter2.complete(
      "You are a knowledge base synthesis assistant. Respond only with valid JSON.",
      [{ role: "user", content: crossPrompt }]
    );
    const crossResult = parseDistillResponse(crossResponse);
    if (crossResult.synthesize.length > 0) {
      allSynthesized.push(...crossResult.synthesize);
      allForget.push(...crossResult.forget);
      summaries.push(crossResult.summary);
    }
  }

  // Execute
  let learned = 0;
  let forgotten = 0;

  for (const fact of allSynthesized) {
    try {
      await commitFact(repo, {
        path: fact.path,
        title: fact.title,
        body: fact.body,
        domain: fact.domain,
        confidence: fact.confidence,
        sources: 1,
        entities: fact.entities,
        refs: fact.refs,
      }, searchIndex);
      learned++;
    } catch (err) {
      log.warn(`distill: failed to learn ${fact.path}: ${err}`);
    }
  }

  for (const file of allForget) {
    try {
      await deleteFact(repo, file, `synthesize-${recipeName}`, searchIndex);
      forgotten++;
    } catch (err) {
      log.warn(`distill: failed to delete ${file}: ${err}`);
    }
  }

  await repo.tag(toMomentTag(`synthesize-${recipeName}-distill`));
  const summary = `Distill: ${learned} learned, ${forgotten} forgotten. ${summaries.join(" ")}`;
  log.info(summary);
  return summary;
}

// --- Main entry point ---

export interface SynthesizeResult {
  branch: string;
  stepSummaries: string[];
  merged: boolean;
}

export async function synthesize(
  repo: GitRepo,
  searchIndex: SearchIndex,
  recipe: Recipe
): Promise<SynthesizeResult> {
  const branchName = `synthesize/${recipe.name}`;
  log.info(`synthesize: starting recipe "${recipe.name}" on branch ${branchName}`);

  // Create synthesis branch
  await repo.gitOrThrow("checkout", "-b", branchName);

  const stepSummaries: string[] = [];

  try {
    for (const step of recipe.steps) {
      log.info(`synthesize: running step mode=${step.mode}`);

      // Re-index from git before each step so we see previous step's changes
      await searchIndex.reindex(repo);

      let summary: string;
      if (step.mode === "prune") {
        summary = await executePruneStep(repo, searchIndex, recipe, step, recipe.name);
      } else {
        summary = await executeDistillStep(repo, searchIndex, recipe, step, recipe.name);
      }
      stepSummaries.push(summary);
    }
  } catch (err) {
    // On error, switch back to original branch
    await repo.gitOrThrow("checkout", "-");
    throw err;
  }

  // Record synthesis run in log
  const headAfter = await repo.headCommit();
  searchIndex.setSynthesisLog(recipe.name, headAfter, stepSummaries.length);

  // Finalize
  if (recipe.auto_merge) {
    await repo.gitOrThrow("checkout", "-");
    await repo.gitOrThrow("merge", branchName);
    await repo.gitOrThrow("branch", "-d", branchName);
    log.info(`synthesize: auto-merged ${branchName} and deleted branch`);
    return { branch: branchName, stepSummaries, merged: true };
  } else {
    await repo.push();
    await repo.gitOrThrow("checkout", "-");
    log.info(`synthesize: pushed ${branchName} for review`);
    return { branch: branchName, stepSummaries, merged: false };
  }
}
```

**Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test synthesize.test.ts`
Expected: PASS

**Step 5: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test`
Expected: all pass

**Step 6: Commit**

```bash
git add src/synthesize.ts src/synthesize.test.ts
git commit -m "feat: add synthesis engine with prune and distill modes"
```

---

### Task 5: Add `reindex` method and `synthesis_log` table to SearchIndex

The synthesis engine needs to re-index facts between pipeline steps so each step sees changes from the previous one. It also needs a `synthesis_log` table to track when each recipe last ran (for auto-discovery mode).

**Files:**
- Modify: `src/search-index.ts`

**Step 1: Add `synthesis_log` table creation to `init()`**

In the `init()` method, after the `meta` table creation, add:

```ts
    this.db.run(`
      CREATE TABLE IF NOT EXISTS synthesis_log (
        recipe TEXT NOT NULL,
        last_commit TEXT NOT NULL,
        run_at TEXT NOT NULL,
        facts_processed INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY (recipe)
      )
    `);
```

**Step 2: Add `reindex` method**

Add to the `SearchIndex` class:

```ts
  async reindex(repo: GitRepo): Promise<void> {
    if (!this.db) return;
    log.info("search index: reindexing from repo");
    const head = await repo.headCommit();
    this.db.run("BEGIN");
    try {
      this.db.run("DELETE FROM facts");
      this.db.run("INSERT INTO facts_fts(facts_fts) VALUES ('delete-all')");
      this.db.run("COMMIT");
    } catch (err) {
      this.db.run("ROLLBACK");
      throw err;
    }
    await this.indexDir(repo, "worlds", head);
    this.setMeta("last_commit", head);
  }
```

**Step 3: Add synthesis log read/write methods**

```ts
  getSynthesisLog(recipe: string): { lastCommit: string; runAt: string; factsProcessed: number } | null {
    if (!this.db) return null;
    const row = this.db
      .query("SELECT last_commit, run_at, facts_processed FROM synthesis_log WHERE recipe = ?")
      .get(recipe) as { last_commit: string; run_at: string; facts_processed: number } | null;
    if (!row) return null;
    return { lastCommit: row.last_commit, runAt: row.run_at, factsProcessed: row.facts_processed };
  }

  setSynthesisLog(recipe: string, lastCommit: string, factsProcessed: number): void {
    if (!this.db) return;
    this.db.query(
      "INSERT OR REPLACE INTO synthesis_log (recipe, last_commit, run_at, facts_processed) VALUES (?, ?, ?, ?)"
    ).run(recipe, lastCommit, new Date().toISOString(), factsProcessed);
  }
```

**Step 4: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test`
Expected: all pass

**Step 5: Commit**

```bash
git add src/search-index.ts
git commit -m "feat: add reindex method and synthesis_log table to SearchIndex"
```

---

### Task 6: CLI subcommand

**Files:**
- Create: `src/cli/synthesize.ts`
- Modify: `src/index.ts:15-17` — add subcommand registration

**Step 1: Create the synthesize subcommand**

```ts
// src/cli/synthesize.ts
import { defineCommand } from "citty";
import { globalArgs } from "./args";

export default defineCommand({
  meta: {
    name: "synthesize",
    description: "Run synthesis recipes to prune and distill facts",
  },
  args: {
    ...globalArgs,
    recipe: {
      type: "string",
      description: "Recipe name (resolves to .knomit/synthesize/<name>.yml)",
    },
    all: {
      type: "boolean",
      description: "Run all recipes in .knomit/synthesize/",
      default: false,
    },
  },
  async run({ args }) {
    const { join } = await import("node:path");
    const { readdir } = await import("node:fs/promises");
    const { setLogFile } = await import("../logger.js");
    const { bootstrap, resolveCacheDir } = await import("../bootstrap.js");
    const { parseRecipe } = await import("../recipe.js");
    const { synthesize } = await import("../synthesize.js");
    const { log } = await import("../logger.js");

    const cacheDir = resolveCacheDir(args["cache-dir"]);
    setLogFile(join(cacheDir, "synthesize.log"));

    const { repo, searchIndex } = await bootstrap({
      repo: args.repo,
      cacheDir: args["cache-dir"],
    });

    if (!searchIndex) {
      console.error("Search index is required for synthesis. Ensure cache directory is writable.");
      process.exit(1);
    }

    const recipesDir = join(repo.repoPath, ".knomit", "synthesize");

    if (args.all) {
      // Run all recipes
      let entries: string[];
      try {
        const dirEntries = await readdir(recipesDir);
        entries = dirEntries.filter((f) => f.endsWith(".yml") || f.endsWith(".yaml"));
      } catch {
        console.error(`No recipes found at ${recipesDir}`);
        process.exit(1);
      }

      for (const file of entries) {
        const raw = await Bun.file(join(recipesDir, file)).text();
        const recipe = parseRecipe(raw);
        console.log(`Running recipe: ${recipe.name}`);
        try {
          const result = await synthesize(repo, searchIndex, recipe);
          console.log(`  Branch: ${result.branch}`);
          for (const s of result.stepSummaries) console.log(`  ${s}`);
          console.log(`  ${result.merged ? "Auto-merged" : "Pushed for review"}`);
        } catch (err) {
          console.error(`  Failed: ${err}`);
        }
      }
    } else if (args.recipe) {
      // Run single recipe
      const recipePath = join(recipesDir, `${args.recipe}.yml`);
      let raw: string;
      try {
        raw = await Bun.file(recipePath).text();
      } catch {
        console.error(`Recipe not found: ${recipePath}`);
        process.exit(1);
      }
      const recipe = parseRecipe(raw);
      console.log(`Running recipe: ${recipe.name}`);
      const result = await synthesize(repo, searchIndex, recipe);
      console.log(`Branch: ${result.branch}`);
      for (const s of result.stepSummaries) console.log(`  ${s}`);
      console.log(result.merged ? "Auto-merged" : "Pushed for review");
    } else {
      // Default: built-in prune+distill on changes since last run
      const defaultRecipe: import("../recipe.js").Recipe = {
        name: "default",
        prompt: "",
        scope: undefined, // auto-discovery mode
        auto_merge: true,
        steps: [
          { mode: "prune", prompt: "" },
          { mode: "distill", prompt: "" },
        ],
      };
      console.log("Running default synthesis (prune + distill on recent changes)...");
      const result = await synthesize(repo, searchIndex, defaultRecipe);
      console.log(`Branch: ${result.branch}`);
      for (const s of result.stepSummaries) console.log(`  ${s}`);
      console.log(result.merged ? "Auto-merged" : "Pushed for review");
    }
  },
});
```

**Step 2: Register the subcommand in index.ts**

In `src/index.ts`, add to the `subCommands` object (line 17):

```ts
  subCommands: {
    mcp: () => import("./cli/mcp.js").then((m) => m.default),
    reset: () => import("./cli/reset.js").then((m) => m.default),
    synthesize: () => import("./cli/synthesize.js").then((m) => m.default),
  },
```

**Step 3: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test`
Expected: all pass

**Step 4: Verify CLI help**

Run: `cd /Users/knomit/data/mine/knomit && bun src/index.ts synthesize --help`
Expected: shows synthesize subcommand help with --recipe and --all flags

**Step 5: Commit**

```bash
git add src/cli/synthesize.ts src/index.ts
git commit -m "feat: add synthesize CLI subcommand"
```

---

### Task 7: End-to-end test with a mock recipe

**Files:**
- Create: `src/synthesize-e2e.test.ts`

**Step 1: Write an integration test**

This test creates a repo with facts, a recipe file, and runs synthesis with a mocked LLM adapter.

```ts
// src/synthesize-e2e.test.ts
import { describe, it, expect, beforeEach, afterEach, mock } from "bun:test";
import { mkdtemp, rm, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { GitRepo } from "./git";
import { SearchIndex } from "./search-index";
import { commitFact } from "./fact-ops";
import { parseRecipe } from "./recipe";
import { synthesize } from "./synthesize";

// Mock the LLM module to avoid real API calls
import * as llm from "./llm";

describe("synthesize e2e", () => {
  let repoPath: string;
  let repo: GitRepo;
  let searchIndex: SearchIndex;

  beforeEach(async () => {
    repoPath = await mkdtemp(join(tmpdir(), "synth-e2e-"));
    repo = new GitRepo(repoPath, "test-machine");
    await repo.init();

    const cacheDir = join(repoPath, ".cache");
    await mkdir(cacheDir, { recursive: true });
    searchIndex = new SearchIndex(join(cacheDir, "index.db"));
    await searchIndex.init({});

    // Create some facts
    await commitFact(repo, {
      path: "know/security/cve-1.md",
      title: "CVE-2024-001 in libfoo",
      body: "Buffer overflow in libfoo 1.0",
      domain: ["security"],
      confidence: 0.9,
      sources: 1,
      entities: ["libfoo"],
      refs: [],
    }, searchIndex);

    await commitFact(repo, {
      path: "know/security/cve-2.md",
      title: "CVE-2024-002 in libfoo",
      body: "Another buffer overflow in libfoo 1.0",
      domain: ["security"],
      confidence: 0.8,
      sources: 1,
      entities: ["libfoo"],
      refs: [],
    }, searchIndex);

    // Create recipe directory
    const recipesDir = join(repoPath, ".knomit", "synthesize");
    await mkdir(recipesDir, { recursive: true });
  });

  afterEach(async () => {
    searchIndex?.close();
    await rm(repoPath, { recursive: true, force: true });
  });

  it("prune step deletes forgotten facts", async () => {
    // Mock the createAdapter to return a fake LLM
    const originalCreateAdapter = llm.createAdapter;
    (llm as any).createAdapter = () => ({
      complete: async () => JSON.stringify({
        decisions: [
          { file: "know/security/cve-1.md", action: "forget", reason: "Fixed" },
          { file: "know/security/cve-2.md", action: "keep", reason: "Still active" },
        ],
        merges: [],
        summary: "Pruned 1 fixed CVE",
      }),
    });

    try {
      const recipe = parseRecipe(`
name: test-prune
prompt: "Test"
scope:
  domain: [security]
auto_merge: true
steps:
  - mode: prune
`);

      const result = await synthesize(repo, searchIndex, recipe);
      expect(result.merged).toBe(true);
      expect(result.stepSummaries[0]).toContain("1 forgotten");

      // cve-1 should be gone, cve-2 should remain
      const cve1Exists = await repo.fileExists("know/security/cve-1.md");
      const cve2Exists = await repo.fileExists("know/security/cve-2.md");
      expect(cve1Exists).toBe(false);
      expect(cve2Exists).toBe(true);
    } finally {
      (llm as any).createAdapter = originalCreateAdapter;
    }
  });
});
```

**Step 2: Run the test**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test synthesize-e2e.test.ts`
Expected: PASS (may need adjustments based on actual SearchIndex init requirements)

**Step 3: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test`
Expected: all pass

**Step 4: Commit**

```bash
git add src/synthesize-e2e.test.ts
git commit -m "test: add end-to-end synthesis test with mock LLM"
```
