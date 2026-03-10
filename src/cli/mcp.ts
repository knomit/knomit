import { defineCommand } from "citty";
import { globalArgs } from "./args";
import { BRANCH_PREFIX } from "../constants.js";

export default defineCommand({
  meta: {
    name: "mcp",
    description: "Run as an MCP server on stdio",
  },
  args: {
    ...globalArgs,
    profile: {
      type: "enum",
      description: "MCP profile",
      options: ["code", "chat", "generic"],
      default: "code",
    },
  },
  async run({ args }) {
    const { McpServer } = await import(
      "@modelcontextprotocol/sdk/server/mcp.js"
    );
    const { StdioServerTransport } = await import(
      "@modelcontextprotocol/sdk/server/stdio.js"
    );
    const { bootstrap } = await import("../bootstrap.js");
    const { getInstructions } = await import("../instructions.js");
    const { registerLearnTool } = await import("../tools/learn.js");
    const { registerQueryTool } = await import("../tools/query.js");
    const { registerWhyTool } = await import("../tools/why.js");
    const { registerUpdateTool } = await import("../tools/update.js");
    const { registerExploreTool } = await import("../tools/explore.js");
    const { registerForgetTool } = await import("../tools/forget.js");
    const { log } = await import("../logger.js");

    const profile = args.profile as "code" | "chat" | "generic";
    const { repo, searchIndex } = await bootstrap({
      repo: args.repo,
      cacheDir: args["cache-dir"],
    });

    const instructions = getInstructions(profile);
    const server = new McpServer({ name: "knomit", version: "0.1.0" });

    server.resource("instructions", "knomit://instructions", async () => ({
      contents: [
        {
          uri: "knomit://instructions",
          mimeType: "text/plain",
          text: instructions,
        },
      ],
    }));

    registerLearnTool(server, repo, searchIndex);
    registerQueryTool(server, repo, searchIndex);
    registerWhyTool(server, repo);
    registerUpdateTool(server, repo, searchIndex);
    registerExploreTool(server, repo);
    registerForgetTool(server, repo, searchIndex);

    server.prompt(
      "knomit-save",
      "Save decisions, preferences, and conclusions from this conversation.",
      async () => ({
        messages: [
          {
            role: "user" as const,
            content: {
              type: "text" as const,
              text: `Review our conversation and identify decisions, preferences, architectural choices, or conclusions worth remembering across sessions.

Before persisting, query knomit for existing facts on each topic to avoid duplicates. If a fact already exists and just needs updating, use knomit_update instead of knomit_learn.

For each new fact, call knomit_learn with:
- Confidence: 0.9+ for explicit user statements, 0.7–0.8 for inferred conclusions. Skip anything below 0.6.
- Refs: include source URLs, commit hashes (as origin-url@hash), or file paths when available.
- Entities and domain tags for discoverability.

Do NOT persist: transient discussion, obvious facts, things easily re-derived, or anything already captured.`,
            },
          },
        ],
      })
    );

    const transport = new StdioServerTransport();
    await server.connect(transport);

    log.info(`running on ${BRANCH_PREFIX}/${repo.agentId}, repo=${repo.repoPath}`);
  },
});
