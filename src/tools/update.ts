import { z } from "zod";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { type GitRepo, toMomentTag } from "../git";
import type { SearchIndex } from "../search-index";
import { parseFact, serializeFact, mergeFrontmatter } from "../facts";
import { log } from "../logger";

const UpdateInput = z.object({
  file: z.string(),
  moment_name: z.string(),
  updates: z.object({
    confidence: z.number().min(0).max(1).optional(),
    sources: z.number().int().optional(),
    body: z.string().optional(),
    title: z.string().optional(),
    refs: z.array(z.string()).optional(),
    domain: z.array(z.string()).optional(),
    entities: z.array(z.string()).optional(),
  }),
});

type UpdateInputType = z.infer<typeof UpdateInput>;

interface UpdateResult {
  commit: string;
  moment_tag: string;
}

export async function updateHandler(
  repo: GitRepo,
  input: UpdateInputType,
  searchIndex?: SearchIndex
): Promise<UpdateResult> {
  log.info(`update: file="${input.file}", moment="${input.moment_name}"`);
  const syncResult = await repo.sync();
  if (syncResult.conflict) {
    throw new Error(`Merge conflict from origin/main. Conflicting files: ${syncResult.conflict.files.join(", ")}. Resolve with knomit_update then retry.`);
  }

  const fileExists = await repo.fileExists(input.file);
  if (!fileExists) {
    throw new Error(`File not found: ${input.file}`);
  }

  const content = await repo.readFile(input.file);
  const parsed = parseFact(content);

  // Merge frontmatter updates
  const newFrontmatter = mergeFrontmatter(parsed.frontmatter, input.updates);

  // Apply title/body overrides
  const newTitle = input.updates.title ?? parsed.title;
  const newBody = input.updates.body ?? parsed.body;

  const serialized = serializeFact(newFrontmatter, newTitle, newBody);

  const commit = await repo.commit(
    [{ path: input.file, content: serialized }],
    `update: ${newTitle}`
  );

  await searchIndex?.upsert(input.file, {
    title: newTitle,
    body: newBody,
    domain: newFrontmatter.domain,
    entities: newFrontmatter.entities,
    confidence: newFrontmatter.confidence,
    sources: newFrontmatter.sources,
    refs: newFrontmatter.refs,
    commitHash: commit,
  });

  const moment_tag = await repo.tag(toMomentTag(input.moment_name));

  await repo.push();

  log.info(`update: done, commit=${commit}, tag=${moment_tag}`);
  return { commit, moment_tag };
}

export function registerUpdateTool(server: McpServer, repo: GitRepo, searchIndex?: SearchIndex): void {
  server.tool(
    "knomit_update",
    "Update an existing fact when a previous belief is reinforced or contradicted. Use this to change confidence, add refs, or correct the body of an existing fact.",
    UpdateInput.shape,
    async (input) => {
      const parsed = UpdateInput.parse(input);
      const result = await updateHandler(repo, parsed, searchIndex);
      return {
        content: [{ type: "text" as const, text: JSON.stringify(result) }],
      };
    }
  );
}
