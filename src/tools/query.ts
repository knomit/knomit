import { z } from "zod";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { GitRepo } from "../git";
import type { SearchIndex } from "../search-index";
import { parseFact, type Frontmatter } from "../facts";
import { log } from "../logger";

const QueryInput = z.object({
  text: z.string().optional(),
  entities: z.array(z.string()).optional(),
  domain: z.array(z.string()).optional(),
  path: z.string().optional(),
  min_confidence: z.number().min(0).max(1).optional().default(0),
});

type QueryInputType = z.infer<typeof QueryInput>;

interface QueryResultFact {
  file: string;
  frontmatter: Frontmatter;
  body: string;
  title: string;
  last_modified: string;
  commit: string;
}

interface QueryResult {
  facts: QueryResultFact[];
}

export async function queryHandler(
  repo: GitRepo,
  input: QueryInputType,
  searchIndex?: SearchIndex
): Promise<QueryResult> {
  log.info(`query: text=${input.text ?? ""}, entities=${JSON.stringify(input.entities ?? [])}, domain=${JSON.stringify(input.domain ?? [])}, path=${input.path ?? ""}`);
  const syncResult = await repo.sync();
  if (syncResult.conflict) {
    throw new Error(`Merge conflict from origin/main. Conflicting files: ${syncResult.conflict.files.join(", ")}. Resolve with knomit_update then retry.`);
  }

  if (!input.text && !input.entities?.length && !input.domain?.length && !input.path) {
    throw new Error("At least one of text, entities, domain, or path must be provided.");
  }

  // Use search index if available
  if (searchIndex) {
    await searchIndex.sync(repo);
    const results = await searchIndex.search({
      text: input.text,
      entities: input.entities,
      domain: input.domain,
      path: input.path,
      min_confidence: input.min_confidence,
    });

    const logResults = await Promise.all(
      results.map((r) => repo.log(r.path).catch(() => []))
    );
    const facts: QueryResultFact[] = results.map((r, i) => {
      const logEntry = logResults[i]?.[0];
      return {
        file: r.path,
        frontmatter: {
          domain: r.domain,
          confidence: r.confidence,
          sources: r.sources,
          entities: r.entities,
          refs: r.refs,
        },
        body: r.body,
        title: r.title,
        last_modified: logEntry?.date ?? "",
        commit: logEntry?.commit ?? "",
      };
    });
    log.info(`query: found ${facts.length} fact(s) via search index`);
    return { facts };
  }

  const candidateFiles = new Set<string>();

  // Gather candidates by entity grep
  if (input.entities && input.entities.length > 0) {
    for (const entity of input.entities) {
      const files = await repo.grep(entity, "worlds/");
      for (const f of files) candidateFiles.add(f);
    }
  }

  // Gather candidates by domain grep
  if (input.domain && input.domain.length > 0) {
    for (const d of input.domain) {
      const files = await repo.grep(d, "worlds/");
      for (const f of files) candidateFiles.add(f);
    }
  }

  // Gather candidates by path prefix (directory walk)
  if (input.path) {
    await walkDir(repo, input.path, candidateFiles);
  }

  const facts: QueryResultFact[] = [];

  for (const file of candidateFiles) {
    if (!file.endsWith(".md")) continue;

    let content: string;
    try {
      content = await repo.readFile(file);
    } catch {
      continue;
    }

    let parsed;
    try {
      parsed = parseFact(content);
    } catch {
      continue;
    }

    // Verify entity match (grep may match body text)
    if (input.entities && input.entities.length > 0) {
      const hasEntity = input.entities.some((e) =>
        parsed.frontmatter.entities.includes(e)
      );
      if (!hasEntity) continue;
    }

    // Verify domain match
    if (input.domain && input.domain.length > 0) {
      const hasDomain = input.domain.some((d) =>
        parsed.frontmatter.domain.includes(d)
      );
      if (!hasDomain) continue;
    }

    // Verify path prefix
    if (input.path && !file.startsWith(input.path)) {
      continue;
    }

    // Filter by min_confidence
    if (parsed.frontmatter.confidence < (input.min_confidence ?? 0)) {
      continue;
    }

    // Get last commit info
    let last_modified = "";
    let commit = "";
    try {
      const logEntries = await repo.log(file);
      if (logEntries.length > 0) {
        last_modified = logEntries[0]!.date;
        commit = logEntries[0]!.commit;
      }
    } catch {
      // ignore log errors
    }

    facts.push({
      file,
      frontmatter: parsed.frontmatter,
      body: parsed.body,
      title: parsed.title,
      last_modified,
      commit,
    });
  }

  log.info(`query: found ${facts.length} fact(s)`);
  return { facts };
}

async function walkDir(
  repo: GitRepo,
  path: string,
  files: Set<string>
): Promise<void> {
  const entries = await repo.listDir(path);
  for (const entry of entries) {
    const fullPath = path.endsWith("/")
      ? `${path}${entry.name}`
      : `${path}/${entry.name}`;
    if (entry.isDirectory) {
      await walkDir(repo, fullPath, files);
    } else {
      files.add(fullPath);
    }
  }
}

export function registerQueryTool(server: McpServer, repo: GitRepo, searchIndex?: SearchIndex): void {
  server.tool(
    "knomit_query",
    `Search the persistent knowledge base. Query by free text, entity names, domain tags, or path prefix.

USE PROACTIVELY: When starting work on a project or topic, query by project name, entity, or domain to load relevant context from previous sessions before responding. Start broad, then narrow by path if needed. Check refs against current state — a fact anchored to an old commit may be outdated.`,
    QueryInput.shape,
    async (input) => {
      const parsed = QueryInput.parse(input);
      const result = await queryHandler(repo, parsed, searchIndex);
      return {
        content: [{ type: "text" as const, text: JSON.stringify(result) }],
      };
    }
  );
}
