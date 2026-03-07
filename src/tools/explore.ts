import { z } from "zod";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { GitRepo } from "../git";
import { parseFact } from "../facts";
import { dirname, join } from "node:path";
import { log } from "../logger";

const ExploreInput = z.object({
  path: z.string().optional().default("worlds"),
});

type ExploreInputType = z.infer<typeof ExploreInput>;

interface ChildEntry {
  name: string;
  type: "world" | "fact";
  summary?: string;
}

interface ManifestInfo {
  file: string;
  title: string;
  body: string;
}

interface InheritedFact {
  file: string;
  title: string;
  confidence: number;
  from_level: string;
}

interface ExploreResult {
  manifest: ManifestInfo | null;
  children: ChildEntry[];
  inherited_facts: InheritedFact[];
}

export async function exploreHandler(
  repo: GitRepo,
  input: ExploreInputType,
  options?: { skipSync?: boolean }
): Promise<ExploreResult> {
  log.info(`explore: path="${input.path ?? "worlds"}"`);
  if (!options?.skipSync) {
    const syncResult = await repo.sync();
    if (syncResult.conflict) {
      throw new Error(`Merge conflict from origin/main. Conflicting files: ${syncResult.conflict.files.join(", ")}. Resolve with knomit_update then retry.`);
    }
  }

  const path = input.path ?? "worlds";

  // Read manifest: look for <path>.md sibling file
  let manifest: ManifestInfo | null = null;
  const manifestPath = `${path}.md`;
  if (await repo.fileExists(manifestPath)) {
    try {
      const raw = await repo.readFile(manifestPath);
      const parsed = parseFact(raw);
      manifest = {
        file: manifestPath,
        title: parsed.title,
        body: parsed.body,
      };
    } catch {
      // ignore parse errors
    }
  }

  // List children of the directory
  const entries = await repo.listDir(path);
  const dirNames = new Set(
    entries.filter((e) => e.isDirectory).map((e) => e.name)
  );

  const children: ChildEntry[] = [];

  for (const entry of entries) {
    if (entry.isDirectory) {
      // It's a world (subdirectory)
      const child: ChildEntry = { name: entry.name, type: "world" };
      // Try to read its manifest for a summary
      const childManifestPath = join(path, `${entry.name}.md`);
      if (await repo.fileExists(childManifestPath)) {
        try {
          const raw = await repo.readFile(childManifestPath);
          const parsed = parseFact(raw);
          child.summary = parsed.title;
        } catch {
          // ignore
        }
      }
      children.push(child);
    } else if (entry.name.endsWith(".md")) {
      // It's a fact
      const child: ChildEntry = { name: entry.name, type: "fact" };
      try {
        const raw = await repo.readFile(join(path, entry.name));
        const parsed = parseFact(raw);
        child.summary = parsed.title;
      } catch {
        // ignore
      }
      children.push(child);
    }
  }

  // Inherited facts: walk up parent directories
  const inherited_facts: InheritedFact[] = [];
  let currentPath = path;

  while (currentPath !== "worlds" && currentPath !== ".") {
    const parentDir = dirname(currentPath);
    if (parentDir === currentPath) break;

    const parentEntries = await repo.listDir(parentDir);
    const parentDirNames = new Set(
      parentEntries.filter((e) => e.isDirectory).map((e) => e.name)
    );

    for (const entry of parentEntries) {
      if (!entry.isDirectory && entry.name.endsWith(".md")) {
        const nameWithoutExt = entry.name.replace(/\.md$/, "");
        // Skip manifests (files matching a sibling directory name)
        if (parentDirNames.has(nameWithoutExt)) {
          continue;
        }
        // Also skip if this is the manifest for the current path's parent
        // (e.g., worlds.md is the manifest for "worlds" directory)
        const filePath = join(parentDir, entry.name);
        try {
          const raw = await repo.readFile(filePath);
          const parsed = parseFact(raw);
          inherited_facts.push({
            file: filePath,
            title: parsed.title,
            confidence: parsed.frontmatter.confidence,
            from_level: parentDir,
          });
        } catch {
          // ignore
        }
      }
    }

    currentPath = parentDir;
  }

  log.info(`explore: ${children.length} children, ${inherited_facts.length} inherited`);
  return { manifest, children, inherited_facts };
}

export function registerExploreTool(server: McpServer, repo: GitRepo): void {
  server.tool(
    "knomit_explore",
    "Browse the knowledge graph hierarchy. Lists worlds (categories) and facts at a given path. Start with 'worlds' to see top-level categories.",
    ExploreInput.shape,
    async (input) => {
      const parsed = ExploreInput.parse(input);
      const result = await exploreHandler(repo, parsed);
      return {
        content: [{ type: "text" as const, text: JSON.stringify(result) }],
      };
    }
  );
}
