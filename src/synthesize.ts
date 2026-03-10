// src/synthesize.ts
import type { GitRepo } from "./git";
import type { SearchIndex, SearchResult } from "./search-index";
import type { Recipe, RecipeStep } from "./recipe";
import type { LLMAdapter } from "./llm";
import { createAdapter, resolveProvider, configFromEnv } from "./llm";
import { commitFact, deleteFact, updateFact } from "./fact-ops";
import { toMomentTag } from "./git";
import { log } from "./logger";
import { clusterFacts } from "./cluster";

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

// --- Prompt constants ---

const PLACEMENT_RULES = `- The path MUST be placed in an appropriate ontological location based on the source facts' paths, NOT under "know/synthesized/" or any operational directory.
- If all sources share a common parent directory, place the fact there (e.g. sources from know/projects/webapp/* → know/projects/webapp/combined-name.md).
- If sources span different directories, place the fact at the nearest common ancestor (e.g. sources from know/debugging/* → know/debugging/common-patterns.md).
- Keep paths meaningful: the directory structure IS the ontology. Use descriptive filenames.
- The "refs" field MUST list the source file paths exactly as given (e.g. "know/foo/bar.md") — the system will resolve them to knomit: URIs automatically.`;

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

IMPORTANT — merged fact placement:
${PLACEMENT_RULES}

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
1. New higher-order facts that capture patterns
2. Which original facts are fully subsumed and can be forgotten

IMPORTANT — synthesized fact placement:
${PLACEMENT_RULES}

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
  const lastRun = searchIndex.getSynthesisLog(recipeName);
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

  const { parseFact } = await import("./facts.js");
  const facts: FactForLLM[] = [];
  for (const path of changedPaths) {
    try {
      const content = await repo.readFile(path);
      const parsed = parseFact(content);
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

// --- Ref resolution ---

/**
 * Resolve file-path refs to knomit: URIs.
 *
 * Local refs (no authority) → knomit:blob/<commit>/<path>
 * External refs would be    → knomit://host/repo/blob/<commit>/<path>
 *
 * Refs that already have a protocol scheme are passed through unchanged.
 */
async function resolveRefs(repo: GitRepo, refs: string[]): Promise<string[]> {
  return Promise.all(refs.map(async (ref) => {
    // Already a URI — pass through
    if (ref.includes("://") || ref.startsWith("knomit:")) return ref;
    const commit = await repo.lastCommitForFile(ref);
    return commit ? `knomit:blob/${commit.slice(0, 7)}/${ref}` : ref;
  }));
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

async function gatherStepFacts(
  repo: GitRepo,
  searchIndex: SearchIndex,
  recipe: Recipe,
  recipeName: string,
  onProgress?: OnProgress
): Promise<FactForLLM[] | null> {
  const isAutoDiscovery = !recipe.scope;
  const facts = recipe.scope
    ? await gatherFacts(searchIndex, recipe.scope)
    : await gatherFactsByDelta(repo, searchIndex, recipeName);
  onProgress?.({ phase: "gather", facts: facts.length, mode: isAutoDiscovery ? "delta" : "scope", firstRun: isAutoDiscovery && !searchIndex.getSynthesisLog(recipeName) });
  return facts.length === 0 ? null : facts;
}

async function executePruneStep(
  repo: GitRepo,
  searchIndex: SearchIndex,
  recipe: Recipe,
  step: RecipeStep,
  recipeName: string,
  stepIdx: number,
  totalSteps: number,
  onProgress?: OnProgress
): Promise<string> {
  const facts = await gatherStepFacts(repo, searchIndex, recipe, recipeName, onProgress);
  if (!facts) return "No facts found in scope.";

  const adapter = adapterForStep(step);
  const chunks = chunkFacts(facts, 100_000);
  const allDecisions: PruneDecision[] = [];
  const allMerges: PruneMerge[] = [];
  const summaries: string[] = [];

  for (let i = 0; i < chunks.length; i++) {
    const chunk = chunks[i]!;
    onProgress?.({ phase: "llm", step: stepIdx, totalSteps, mode: "prune", chunk: i + 1, totalChunks: chunks.length, facts: chunk.length });
    const prompt = buildPrunePrompt(chunk, recipe.prompt, step.prompt ?? "");
    log.info(`prune: sending ${chunk.length} facts to LLM`);
    const t0 = Date.now();
    let receivedBytes = 0;
    let response: string;
    try {
      response = await adapter.complete(
        "You are a knowledge base maintenance assistant. Respond only with valid JSON.",
        [{ role: "user", content: prompt }],
        onProgress ? (text: string) => {
          receivedBytes += text.length;
          onProgress({ phase: "llm-stream", step: stepIdx, totalSteps, bytes: receivedBytes });
        } : undefined
      );
    } finally {
      onProgress?.({ phase: "llm-done", step: stepIdx, totalSteps, mode: "prune", elapsed: Date.now() - t0 });
    }
    const result = parsePruneResponse(response);
    allDecisions.push(...result.decisions);
    allMerges.push(...result.merges);
    summaries.push(result.summary);
  }

  let forgotten = 0;
  let updated = 0;
  let merged = 0;
  const kept = allDecisions.filter((d) => d.action === "keep").length;
  const deletedPaths = new Set<string>();

  for (const decision of allDecisions) {
    if (decision.action === "keep") {
      log.info(`prune: keep ${decision.file} — ${decision.reason}`);
      onProgress?.({ phase: "detail-keep", path: decision.file, reason: decision.reason });
    } else if (decision.action === "forget") {
      try {
        await deleteFact(repo, decision.file, `synthesize-${recipeName}`, searchIndex, decision.reason);
        forgotten++;
        deletedPaths.add(decision.file);
        log.info(`prune: forget ${decision.file} — ${decision.reason}`);
        onProgress?.({ phase: "detail-forget", path: decision.file, reason: decision.reason });
      } catch (err) {
        log.warn(`prune: failed to delete ${decision.file}: ${err}`);
      }
    } else if (decision.action === "update" && decision.confidence != null) {
      try {
        await updateFact(repo, decision.file, { confidence: decision.confidence }, searchIndex, decision.reason);
        updated++;
        log.info(`prune: update ${decision.file} confidence=${decision.confidence} — ${decision.reason}`);
        onProgress?.({ phase: "detail-update", path: decision.file, confidence: decision.confidence, reason: decision.reason });
      } catch (err) {
        log.warn(`prune: failed to update ${decision.file}: ${err}`);
      }
    }
  }

  for (const merge of allMerges) {
    try {
      const mergeReason = `Merged ${merge.sources.length} facts: ${merge.sources.join(", ")}`;
      const resolvedRefs = await resolveRefs(repo, merge.merged.refs);
      await commitFact(repo, {
        path: merge.merged.path,
        title: merge.merged.title,
        body: merge.merged.body,
        domain: merge.merged.domain,
        confidence: merge.merged.confidence,
        sources: 1,
        entities: merge.merged.entities,
        refs: resolvedRefs,
      }, searchIndex, mergeReason);
      for (const source of merge.sources) {
        if (deletedPaths.has(source)) continue; // already forgotten above
        try {
          await deleteFact(repo, source, `synthesize-${recipeName}`, searchIndex, `Subsumed by merged fact: ${merge.merged.path}`);
          deletedPaths.add(source);
        } catch (err) {
          log.warn(`prune: failed to delete merge source ${source}: ${err}`);
        }
      }
      merged++;
      log.info(`prune: merge ${merge.sources.join(", ")} -> ${merge.merged.path}`);
      onProgress?.({ phase: "detail-merge", sources: merge.sources, target: merge.merged.path, reason: mergeReason });
    } catch (err) {
      log.warn(`prune: failed to commit merged fact ${merge.merged.path}: ${err}`);
    }
  }

  onProgress?.({ phase: "apply", mode: "prune", kept, forgotten, updated, merged });

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
  recipeName: string,
  stepIdx: number,
  totalSteps: number,
  onProgress?: OnProgress
): Promise<string> {
  if (!searchIndex.hasEmbeddings) {
    throw new Error("Distill mode requires embeddings. Enable embeddings in your SearchIndex configuration.");
  }

  const facts = await gatherStepFacts(repo, searchIndex, recipe, recipeName, onProgress);
  if (!facts) return "No facts found in scope.";

  const adapter = adapterForStep(step);
  const allSynthesized: DistillFact[] = [];
  const allForget: string[] = [];
  const summaries: string[] = [];

  const maxDepth = step.max_depth ?? 1;
  let currentFacts = facts;
  let currentEmbeddings: Map<string, Float32Array> | null = null;

  for (let depth = 0; depth < maxDepth; depth++) {
    onProgress?.({ phase: "raptor-depth", depth: depth + 1, maxDepth });

    // Get embeddings: from previous RAPTOR iteration or from search index
    const embeddings = currentEmbeddings ?? searchIndex.getEmbeddings(currentFacts.map(f => f.path));
    currentEmbeddings = null;

    // Cluster facts by semantic similarity
    const clusterResult = clusterFacts(currentFacts, embeddings, {
      umapDimensions: step.umap_dimensions,
      minClusterSize: step.min_cluster_size,
    });

    log.info(`distill: ${clusterResult.noise.length} noise facts skipped`);
    onProgress?.({ phase: "cluster", clusters: clusterResult.clusters.size, noise: clusterResult.noise.length });

    if (clusterResult.clusters.size === 0) {
      log.info(`distill: no clusters formed at depth ${depth + 1}, stopping`);
      break;
    }

    const depthSynthesized: DistillFact[] = [];
    let clusterIdx = 0;
    const totalClusters = clusterResult.clusters.size;

    for (const [_label, clusterFacts] of clusterResult.clusters) {
      clusterIdx++;

      // If a single cluster's JSON exceeds 100KB, fall back to chunkFacts within it
      const clusterJson = JSON.stringify(clusterFacts);
      const groups = clusterJson.length > 100_000
        ? chunkFacts(clusterFacts, 100_000)
        : [clusterFacts];

      for (let i = 0; i < groups.length; i++) {
        const group = groups[i]!;
        onProgress?.({ phase: "llm", step: stepIdx, totalSteps, mode: "distill", chunk: clusterIdx, totalChunks: totalClusters, facts: group.length });
        const prompt = buildDistillPrompt(group, recipe.prompt, step.prompt ?? "");
        log.info(`distill: sending ${group.length} facts (cluster ${clusterIdx}/${totalClusters}) to LLM`);
        const t0 = Date.now();
        let receivedBytes = 0;
        let response: string;
        try {
          response = await adapter.complete(
            "You are a knowledge base synthesis assistant. Respond only with valid JSON.",
            [{ role: "user", content: prompt }],
            onProgress ? (text: string) => {
              receivedBytes += text.length;
              onProgress({ phase: "llm-stream", step: stepIdx, totalSteps, bytes: receivedBytes });
            } : undefined
          );
        } finally {
          onProgress?.({ phase: "llm-done", step: stepIdx, totalSteps, mode: "distill", elapsed: Date.now() - t0 });
        }
        const result = parseDistillResponse(response);
        depthSynthesized.push(...result.synthesize);
        allForget.push(...result.forget);
        summaries.push(result.summary);
      }
    }

    allSynthesized.push(...depthSynthesized);

    // RAPTOR recursion: if we have new facts and more depth to go, re-embed and cluster again
    if (depth + 1 < maxDepth && depthSynthesized.length > 0) {
      const embedder = searchIndex.getEmbedder()!;
      const newEmbeddings = new Map<string, Float32Array>();
      const newFacts: FactForLLM[] = [];
      for (const fact of depthSynthesized) {
        const embeddingText = `${fact.title} ${fact.body} ${fact.entities.join(" ")} ${fact.domain.join(" ")}`;
        const vec = await embedder.embed(embeddingText);
        newEmbeddings.set(fact.path, vec);
        newFacts.push({ ...fact, sources: 1 });
      }
      currentFacts = newFacts;
      currentEmbeddings = newEmbeddings;
    }
  }

  let learned = 0;
  let forgotten = 0;

  for (const fact of allSynthesized) {
    try {
      const resolvedRefs = await resolveRefs(repo, fact.refs);
      const refsNote = fact.refs.length > 0 ? `Distilled from: ${fact.refs.join(", ")}` : "Distilled from analysis of related facts";
      await commitFact(repo, {
        path: fact.path,
        title: fact.title,
        body: fact.body,
        domain: fact.domain,
        confidence: fact.confidence,
        sources: 1,
        entities: fact.entities,
        refs: resolvedRefs,
      }, searchIndex, refsNote);
      learned++;
      const refsInfo = fact.refs.length > 0 ? ` from: ${fact.refs.join(", ")}` : "";
      log.info(`distill: learn ${fact.path} — ${fact.body.slice(0, 120)}${refsInfo}`);
      onProgress?.({ phase: "detail-learn", path: fact.path, body: fact.body, refs: fact.refs });
    } catch (err) {
      log.warn(`distill: failed to learn ${fact.path}: ${err}`);
    }
  }

  const uniqueForget = [...new Set(allForget)];
  for (const file of uniqueForget) {
    try {
      await deleteFact(repo, file, `synthesize-${recipeName}`, searchIndex, "Subsumed by higher-order distilled fact");
      forgotten++;
      log.info(`distill: forget ${file} (subsumed)`);
      onProgress?.({ phase: "detail-distill-forget", path: file });
    } catch (err) {
      log.warn(`distill: failed to delete ${file}: ${err}`);
    }
  }

  onProgress?.({ phase: "apply", mode: "distill", learned, forgotten });

  await repo.tag(toMomentTag(`synthesize-${recipeName}-distill`));
  const summary = `Distill: ${learned} learned, ${forgotten} forgotten. ${summaries.join(" ")}`;
  log.info(summary);
  return summary;
}

// --- Main entry point ---

export type ProgressEvent =
  | { phase: "step-start"; step: number; totalSteps: number; mode: string }
  | { phase: "gather"; facts: number; mode: "scope" | "delta"; firstRun?: boolean }
  | { phase: "llm"; step: number; totalSteps: number; mode: string; chunk: number; totalChunks: number; facts: number }
  | { phase: "llm-done"; step: number; totalSteps: number; mode: string; elapsed: number }
  | { phase: "llm-stream"; step: number; totalSteps: number; bytes: number }
  | { phase: "apply"; mode: "prune"; kept: number; forgotten: number; updated: number; merged: number }
  | { phase: "apply"; mode: "distill"; learned: number; forgotten: number }
  | { phase: "detail-keep"; path: string; reason: string }
  | { phase: "detail-forget"; path: string; reason: string }
  | { phase: "detail-update"; path: string; confidence: number; reason: string }
  | { phase: "detail-merge"; sources: string[]; target: string; reason: string }
  | { phase: "detail-learn"; path: string; body: string; refs: string[] }
  | { phase: "detail-distill-forget"; path: string }
  | { phase: "cross-chunk"; facts: number }
  | { phase: "cluster"; clusters: number; noise: number }
  | { phase: "raptor-depth"; depth: number; maxDepth: number }
  | { phase: "reindex" }
  | { phase: "merge" }
  | { phase: "push" }
  | { phase: "done"; stepSummaries: string[]; elapsed: number };

export type OnProgress = (event: ProgressEvent) => void;

export interface SynthesizeResult {
  branch: string;
  stepSummaries: string[];
  merged: boolean;
}

export async function synthesize(
  repo: GitRepo,
  searchIndex: SearchIndex,
  recipe: Recipe,
  onProgress?: OnProgress
): Promise<SynthesizeResult> {
  const branchName = `synthesize/${recipe.name}`;
  const t0 = Date.now();
  log.info(`synthesize: starting recipe "${recipe.name}" on branch ${branchName}`);

  // Delete stale synthesis branch from a previous failed/interrupted run
  try { await repo.deleteBranch(branchName); } catch { /* doesn't exist — fine */ }

  await repo.checkoutBranch(branchName, true);

  const stepSummaries: string[] = [];

  try {
    for (let i = 0; i < recipe.steps.length; i++) {
      const step = recipe.steps[i]!;
      log.info(`synthesize: running step mode=${step.mode}`);
      onProgress?.({ phase: "step-start", step: i, totalSteps: recipe.steps.length, mode: step.mode });

      // Re-index from git before each step so we see previous step's changes
      onProgress?.({ phase: "reindex" });
      await searchIndex.reindex(repo);

      let summary: string;
      if (step.mode === "prune") {
        summary = await executePruneStep(repo, searchIndex, recipe, step, recipe.name, i, recipe.steps.length, onProgress);
      } else {
        summary = await executeDistillStep(repo, searchIndex, recipe, step, recipe.name, i, recipe.steps.length, onProgress);
      }
      stepSummaries.push(summary);
    }
  } catch (err) {
    await repo.checkoutPrevious();
    throw err;
  }

  // Record synthesis run in log — setSynthesisLog added in Task 5
  const headAfter = await repo.headCommit();
  searchIndex.setSynthesisLog(recipe.name, headAfter, stepSummaries.length);

  if (recipe.auto_merge) {
    onProgress?.({ phase: "merge" });
    const currentBranch = branchName;
    await repo.checkoutPrevious();
    await repo.mergeBranch(currentBranch);
    await repo.deleteBranch(currentBranch);
    log.info(`synthesize: auto-merged ${branchName} and deleted branch`);
    onProgress?.({ phase: "done", stepSummaries, elapsed: Date.now() - t0 });
    return { branch: branchName, stepSummaries, merged: true };
  } else {
    onProgress?.({ phase: "push" });
    await repo.pushBranch(branchName);
    await repo.checkoutPrevious();
    log.info(`synthesize: pushed ${branchName} for review`);
    onProgress?.({ phase: "done", stepSummaries, elapsed: Date.now() - t0 });
    return { branch: branchName, stepSummaries, merged: false };
  }
}
