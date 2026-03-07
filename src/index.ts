import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { hostname, homedir } from "node:os";
import { exists } from "node:fs/promises";
import { join, resolve } from "node:path";
import { GitRepo } from "./git";
import { SearchIndex } from "./search-index";
import { registerLearnTool } from "./tools/learn";
import { registerQueryTool } from "./tools/query";
import { registerWhyTool } from "./tools/why";
import { registerUpdateTool } from "./tools/update";
import { registerExploreTool } from "./tools/explore";
import { registerForgetTool } from "./tools/forget";
import { log } from "./logger";

const INSTRUCTIONS = `You have access to Knomit, a persistent knowledge base that survives
across sessions. It stores structured facts as markdown files in a
Git repository, organized by an ontological hierarchy (worlds/).

Your knowledge base operates on a machine-specific branch. Other
machines may contribute knowledge that arrives via merges from main.
If a merge conflict occurs, you will be notified and should resolve
it using knomit_update.

AT SESSION START:
- Call knomit_query with relevant entities or domains to load context
  from previous sessions before responding to the user.

DURING CONVERSATION:
- When the user states a preference, makes a decision, or you jointly
  arrive at a conclusion — call knomit_learn to persist it.
- When you need deeper context on a fact — call knomit_why.
- When a previous fact is reinforced or contradicted — call knomit_update.

AT SESSION END:
- Review what was decided or learned during this session.
- Call knomit_learn for anything worth remembering.

GUIDELINES:
- Not everything needs to be saved. Persist decisions, preferences,
  architectural choices, and conclusions — not transient discussion.
- Use the ontology (worlds/) to organize facts by where they belong,
  not just what they're about.
- When querying, start broad (domain or entity) then narrow down.`;

async function bootstrap(options?: { embeddings?: boolean }) {
  const repoPathRaw = process.env.KNOMIT_REPO ?? join(homedir(), ".knomit");
  const machineId = process.env.KNOMIT_MACHINE_ID ?? hostname();

  const repoPath = repoPathRaw.startsWith("~")
    ? resolve(homedir(), repoPathRaw.slice(2))
    : resolve(repoPathRaw);

  const repo = new GitRepo(repoPath, machineId);
  await repo.init();

  const cacheDir = process.env.KNOMIT_CACHE_DIR ?? join(homedir(), ".cache", "knomit");
  const envEmbeddings = process.env.KNOMIT_EMBEDDINGS;
  const embeddingsEnabled = envEmbeddings !== undefined
    ? (envEmbeddings === "1" || envEmbeddings === "true")
    : (options?.embeddings ?? false);
  const searchIndex = new SearchIndex(cacheDir, { embeddings: embeddingsEnabled });
  await searchIndex.init();
  await searchIndex.sync(repo);

  return { repo, searchIndex, repoPath, machineId };
}

async function startMcp(repo: GitRepo, searchIndex: SearchIndex) {
  const server = new McpServer({ name: "knomit", version: "0.1.0" });

  server.resource("instructions", "knomit://instructions", async () => ({
    contents: [
      {
        uri: "knomit://instructions",
        mimeType: "text/plain",
        text: INSTRUCTIONS,
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
    "knomit-start",
    "Make the assistant aware of your persistent knowledge base. Use at the start of a conversation.",
    {},
    async () => ({
      messages: [
        {
          role: "user" as const,
          content: {
            type: "text" as const,
            text: `${INSTRUCTIONS}\n\nYou now know about my knowledge base. Do NOT query or explore it right now. Just keep it in mind and use the knomit tools naturally when relevant during our conversation — for example, query when you need context about my preferences or past decisions, and learn when we arrive at something worth remembering.`,
          },
        },
      ],
    })
  );

  server.prompt(
    "knomit-save",
    "Save decisions, preferences, and conclusions from this conversation.",
    {},
    async () => ({
      messages: [
        {
          role: "user" as const,
          content: {
            type: "text" as const,
            text: "Review our conversation and identify any decisions, preferences, architectural choices, or conclusions worth remembering across sessions. For each one, call knomit_learn to persist it. Organize facts into appropriate worlds/ paths based on their domain.",
          },
        },
      ],
    })
  );

  const transport = new StdioServerTransport();
  await server.connect(transport);

  log.info(`running on machine/${repo.machineId}, repo=${repo.repoPath}`);
}

async function startTui(repo: GitRepo, searchIndex: SearchIndex) {
  const { startApp } = await import("./tui/App.js");
  startApp(repo, searchIndex);
}

async function reset() {
  const repoPathRaw = process.env.KNOMIT_REPO ?? join(homedir(), ".knomit");
  const repoPath = repoPathRaw.startsWith("~")
    ? resolve(homedir(), repoPathRaw.slice(2))
    : resolve(repoPathRaw);
  const cacheDir = process.env.KNOMIT_CACHE_DIR ?? join(homedir(), ".cache", "knomit");

  const { rmSync } = await import("node:fs");

  if (await exists(repoPath)) {
    rmSync(repoPath, { recursive: true, force: true });
    console.log(`removed repo: ${repoPath}`);
  } else {
    console.log(`repo not found: ${repoPath}`);
  }

  if (await exists(cacheDir)) {
    rmSync(cacheDir, { recursive: true, force: true });
    console.log(`removed cache: ${cacheDir}`);
  } else {
    console.log(`cache not found: ${cacheDir}`);
  }

  console.log("reset complete");
}

async function main() {
  if (process.argv.includes("--reset")) {
    await reset();
    return;
  }

  const isMcp = process.argv.includes("--mcp");

  if (!isMcp) {
    const { setLogFile } = await import("./logger.js");
    const cacheDir = process.env.KNOMIT_CACHE_DIR ?? join(homedir(), ".cache", "knomit");
    setLogFile(join(cacheDir, "tui.log"));
  }

  const { repo, searchIndex } = await bootstrap({ embeddings: !isMcp });

  if (isMcp) {
    await startMcp(repo, searchIndex);
  } else {
    await startTui(repo, searchIndex);
  }
}

main().catch((err) => {
  log.error(`fatal: ${err}`);
  process.exit(1);
});
