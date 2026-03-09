import { z } from "zod";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { GitRepo } from "../git";
import type { SearchIndex } from "../search-index";
import { deleteFact } from "../fact-ops";
import { log } from "../logger";

const ForgetInput = z.object({
  file: z.string(),
  moment_name: z.string(),
});

type ForgetInputType = z.infer<typeof ForgetInput>;

interface ForgetResult {
  file: string;
  commit: string;
  moment_tag: string;
}

export async function forgetHandler(
  repo: GitRepo,
  input: ForgetInputType,
  searchIndex?: SearchIndex
): Promise<ForgetResult> {
  log.info(`forget: file="${input.file}", moment="${input.moment_name}"`);

  const syncResult = await repo.sync();
  if (syncResult.conflict) {
    throw new Error(
      `Merge conflict from origin/main. Conflicting files: ${syncResult.conflict.files.join(", ")}. Resolve with knomit_update then retry.`
    );
  }

  const commit = await deleteFact(repo, input.file, input.moment_name, searchIndex);

  const safe = input.moment_name.replace(/[^a-zA-Z0-9._/-]/g, "-");
  const moment_tag = await repo.tag(`forget/${safe}`);

  await repo.push();

  log.info(`forget: done, commit=${commit}, tag=${moment_tag}`);
  return { file: input.file, commit, moment_tag };
}

export function registerForgetTool(
  server: McpServer,
  repo: GitRepo,
  searchIndex?: SearchIndex
): void {
  server.tool(
    "knomit_forget",
    "Remove a fact from the knowledge base. The file is deleted from the repo; git history retains provenance. Use when a fact is no longer true, relevant, or was stored in error.",
    ForgetInput.shape,
    async (input) => {
      const parsed = ForgetInput.parse(input);
      const result = await forgetHandler(repo, parsed, searchIndex);
      return {
        content: [{ type: "text" as const, text: JSON.stringify(result) }],
      };
    }
  );
}
