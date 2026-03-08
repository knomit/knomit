import { z } from "zod";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { type GitRepo, toMomentTag } from "../git";
import type { SearchIndex } from "../search-index";
import { serializeFact } from "../facts";
import { log } from "../logger";

const LearnInput = z.object({
  moment_name: z.string(),
  facts: z.array(
    z.object({
      path: z.string(),
      domain: z.array(z.string()),
      confidence: z.number().min(0).max(1),
      sources: z.number().int(),
      entities: z.array(z.string()),
      refs: z.array(z.string()).optional().default([]),
      title: z.string(),
      body: z.string(),
    })
  ),
});

type LearnInputType = z.infer<typeof LearnInput>;

interface LearnResult {
  moment_tag: string;
  commits: Array<{ file: string; hash: string }>;
}

export async function learnHandler(
  repo: GitRepo,
  input: LearnInputType,
  searchIndex?: SearchIndex
): Promise<LearnResult> {
  log.info(`learn: moment="${input.moment_name}", ${input.facts.length} fact(s)`);
  const syncResult = await repo.sync();
  if (syncResult.conflict) {
    throw new Error(`Merge conflict from origin/main. Conflicting files: ${syncResult.conflict.files.join(", ")}. Resolve with knomit_update then retry.`);
  }

  const commits: Array<{ file: string; hash: string }> = [];

  for (const fact of input.facts) {
    // Ensure path ends with .md and starts with worlds/
    let factPath = fact.path;
    if (!factPath.startsWith("worlds/")) {
      factPath = `worlds/${factPath}`;
    }
    if (!factPath.endsWith(".md")) {
      factPath = `${factPath}.md`;
    }

    const content = serializeFact(
      {
        domain: fact.domain,
        confidence: fact.confidence,
        sources: fact.sources,
        entities: fact.entities,
        refs: fact.refs ?? [],
      },
      fact.title,
      fact.body
    );

    const hash = await repo.commit(
      [{ path: factPath, content }],
      `learn: ${fact.title}`
    );

    // Index the fact
    await searchIndex?.upsert(factPath, {
      title: fact.title,
      body: fact.body,
      domain: fact.domain,
      entities: fact.entities,
      confidence: fact.confidence,
      sources: fact.sources,
      refs: fact.refs ?? [],
      commitHash: hash,
    });

    log.debug(`learn: committed ${factPath} as ${hash}`);
    commits.push({ file: factPath, hash });
  }

  const moment_tag = await repo.tag(toMomentTag(input.moment_name));

  await repo.push();

  log.info(`learn: done, tag=${moment_tag}`);
  return { moment_tag, commits };
}

export function registerLearnTool(server: McpServer, repo: GitRepo, searchIndex?: SearchIndex): void {
  server.tool(
    "knomit_learn",
    `Persist knowledge to a Git-backed knowledge base. Call this AUTOMATICALLY whenever the user states a preference, makes a decision, or you jointly arrive at a conclusion worth remembering across sessions. Creates one or more facts as a learning moment.

WHEN TO CALL: Decisions, preferences, architectural choices, debugging insights, conclusions. NOT transient discussion, obvious facts, or things easily re-derived.

FACT QUALITY:
- path: organize under worlds/ by domain (e.g. worlds/projects/myapp/conventions.md). Durable facts at higher levels, ephemeral facts in sub-directories.
- title: concise and descriptive — this is the primary search surface.
- body: include reasoning, not just conclusions.
- confidence: 0.9+ for explicit user statements, 0.7–0.8 for inferred conclusions, 0.5–0.6 for tentative observations.
- entities: people, projects, tools — anything worth querying by.
- domain: topic tags like "architecture", "testing", "workflow".
- refs: anchor to source. For code facts, use "origin-url@commit-hash" (e.g. "github.com/org/repo@abc123"). Also URLs, file paths, issue numbers.
- sources: set to 1 for new facts; increment via knomit_update when multiple sessions confirm the same thing.

Before learning, query first to avoid duplicating an existing fact — use knomit_update instead if one exists.`,
    LearnInput.shape,
    async (input) => {
      const parsed = LearnInput.parse(input);
      const result = await learnHandler(repo, parsed, searchIndex);
      return {
        content: [{ type: "text" as const, text: JSON.stringify(result) }],
      };
    }
  );
}
