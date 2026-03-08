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
        "path": "worlds/...",
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
      "path": "worlds/...",
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
  const match = text.match(/```(?:json)?\s*\n?([\s\S]*?)\n?```/);
  return match ? match[1].trim() : text.trim();
}

export function parsePruneResponse(text: string): PruneResult {
  const json = extractJson(text);
  try {
    const parsed = JSON.parse(json);
    return {
      decisions: parsed.decisions ?? [],
      merges: parsed.merges ?? [],
      summary: parsed.summary ?? "",
    };
  } catch (err) {
    throw new Error(`Failed to parse prune response: ${err}\nRaw: ${json.slice(0, 200)}`);
  }
}

export function parseDistillResponse(text: string): DistillResult {
  const json = extractJson(text);
  try {
    const parsed = JSON.parse(json);
    return {
      synthesize: parsed.synthesize ?? [],
      forget: parsed.forget ?? [],
      summary: parsed.summary ?? "",
    };
  } catch (err) {
    throw new Error(`Failed to parse distill response: ${err}\nRaw: ${json.slice(0, 200)}`);
  }
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

  const baseResults = await searchIndex.search({
    domain: scope.domain.length > 0 ? scope.domain : undefined,
    entities: scope.entities.length > 0 ? scope.entities : undefined,
    path: scope.path || undefined,
    limit: 10_000,
  });
  for (const r of baseResults) {
    allFacts.set(r.path, searchResultToFact(r));
  }

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
  // getSynthesisLog is added in Task 5 — will be undefined until then
  const lastRun = (searchIndex as any).getSynthesisLog?.(recipeName);
  if (!lastRun) {
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
        try {
          await deleteFact(repo, source, `synthesize-${recipeName}`, searchIndex);
        } catch (err) {
          log.warn(`prune: failed to delete merge source ${source}: ${err}`);
        }
      }
      merged++;
    } catch (err) {
      log.warn(`prune: failed to commit merged fact ${merge.merged.path}: ${err}`);
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

  if (chunks.length > 1 && allSynthesized.length > 0) {
    const crossPrompt = buildDistillPrompt(
      allSynthesized.map((s) => ({ ...s, sources: 1 })),
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
      // Replace per-chunk facts with consolidated cross-batch output
      allSynthesized.length = 0;
      allSynthesized.push(...crossResult.synthesize);
      allForget.push(...crossResult.forget);
      summaries.push(crossResult.summary);
    }
  }

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

  // Create synthesis branch — use public methods added to GitRepo
  await repo.checkoutBranch(branchName, true);

  const stepSummaries: string[] = [];

  try {
    for (const step of recipe.steps) {
      log.info(`synthesize: running step mode=${step.mode}`);

      // Re-index from git before each step so we see previous step's changes
      // reindex() is added in Task 5
      await (searchIndex as any).reindex?.(repo);

      let summary: string;
      if (step.mode === "prune") {
        summary = await executePruneStep(repo, searchIndex, recipe, step, recipe.name);
      } else {
        summary = await executeDistillStep(repo, searchIndex, recipe, step, recipe.name);
      }
      stepSummaries.push(summary);
    }
  } catch (err) {
    await repo.checkoutPrevious();
    throw err;
  }

  // Record synthesis run in log — setSynthesisLog added in Task 5
  const headAfter = await repo.headCommit();
  (searchIndex as any).setSynthesisLog?.(recipe.name, headAfter, stepSummaries.length);

  if (recipe.auto_merge) {
    const currentBranch = branchName;
    await repo.checkoutPrevious();
    await repo.mergeBranch(currentBranch);
    await repo.deleteBranch(currentBranch);
    log.info(`synthesize: auto-merged ${branchName} and deleted branch`);
    return { branch: branchName, stepSummaries, merged: true };
  } else {
    await repo.pushBranch(branchName);
    await repo.checkoutPrevious();
    log.info(`synthesize: pushed ${branchName} for review`);
    return { branch: branchName, stepSummaries, merged: false };
  }
}
