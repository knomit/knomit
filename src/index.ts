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
import { getInstructions, MCP_PROFILES, type McpProfile } from "./instructions";

interface BootstrapOptions {
  embeddings?: boolean;
  repo?: string;
  cacheDir?: string;
}

function resolvePath(raw: string): string {
  return raw.startsWith("~") ? resolve(homedir(), raw.slice(2)) : resolve(raw);
}

async function bootstrap(options?: BootstrapOptions) {
  const repoPathRaw = options?.repo ?? process.env.KNOMIT_REPO ?? join(homedir(), ".knomit");
  const machineId = process.env.KNOMIT_MACHINE_ID ?? hostname();
  const repoPath = resolvePath(repoPathRaw);

  const repo = new GitRepo(repoPath, machineId);
  await repo.init();

  const cacheDir = resolvePath(options?.cacheDir ?? process.env.KNOMIT_CACHE_DIR ?? join(homedir(), ".cache", "knomit"));
  const envEmbeddings = process.env.KNOMIT_EMBEDDINGS;
  const embeddingsEnabled = envEmbeddings !== undefined
    ? (envEmbeddings !== "0" && envEmbeddings !== "false")
    : (options?.embeddings ?? true);
  const searchIndex = new SearchIndex(cacheDir, { embeddings: embeddingsEnabled });
  await searchIndex.init();
  await searchIndex.sync(repo);

  return { repo, searchIndex, repoPath, machineId, cacheDir };
}

async function startMcp(repo: GitRepo, searchIndex: SearchIndex, profile: McpProfile) {
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

  log.info(`running on machine/${repo.machineId}, repo=${repo.repoPath}`);
}

async function startTui(repo: GitRepo, searchIndex: SearchIndex) {
  const { startApp } = await import("./tui/App.js");
  startApp(repo, searchIndex);
}

async function reset(repoOverride?: string, cacheDirOverride?: string) {
  const repoPath = resolvePath(repoOverride ?? process.env.KNOMIT_REPO ?? join(homedir(), ".knomit"));
  const cacheDir = resolvePath(cacheDirOverride ?? process.env.KNOMIT_CACHE_DIR ?? join(homedir(), ".cache", "knomit"));

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

const KNOWN_FLAGS = ["--mcp", "--reset", "--help"];
const KNOWN_VALUE_FLAGS = ["--repo", "--cache-dir"];

function isKnownFlag(flag: string): boolean {
  if (KNOWN_FLAGS.includes(flag)) return true;
  if (flag.startsWith("--mcp=")) {
    const profile = flag.slice(6);
    return MCP_PROFILES.includes(profile as McpProfile);
  }
  for (const vf of KNOWN_VALUE_FLAGS) {
    if (flag === vf || flag.startsWith(`${vf}=`)) return true;
  }
  return false;
}

function parseValueFlag(name: string): string | undefined {
  for (const arg of process.argv.slice(2)) {
    if (arg.startsWith(`${name}=`)) return arg.slice(name.length + 1);
    if (arg === name) {
      const idx = process.argv.indexOf(arg);
      const next = process.argv[idx + 1];
      if (next && !next.startsWith("--")) return next;
    }
  }
  return undefined;
}

function parseMcpFlag(): McpProfile | null {
  for (const arg of process.argv.slice(2)) {
    if (arg === "--mcp") return "code";
    if (arg.startsWith("--mcp=")) return arg.slice(6) as McpProfile;
  }
  return null;
}

function printHelp() {
  console.log(`knomit - Git-backed knowledge base for AI agents

Usage:
  knomit                  Launch the TUI browser
  knomit --mcp[=profile]  Run as an MCP server
  knomit --reset          Wipe the repo and search index
  knomit --help           Show this help

Options:
  --repo=<path>           Override the git repository path
  --cache-dir=<path>      Override the SQLite index and cache path

MCP profiles:
  code      Code editors (default) — anchors facts to git commits
  chat      Conversational tools — anchors facts to URLs, documents
  generic   Minimal instructions for any integration

Environment variables:
  KNOMIT_REPO         Path to the git repository (default: ~/.knomit)
  KNOMIT_CACHE_DIR    Path to the SQLite index and cache (default: ~/.cache/knomit)
  KNOMIT_MACHINE_ID   Branch name: machine/<id> (default: system hostname)
  KNOMIT_EMBEDDINGS   Vector similarity search, on by default (0 or false to disable)
  KNOMIT_POLL_INTERVAL  TUI remote poll interval in ms (default: 5000)`);
}

async function main() {
  const flags = process.argv.slice(2).filter((a) => a.startsWith("-"));
  const unknown = flags.filter((f) => !isKnownFlag(f));
  if (unknown.length > 0) {
    console.error(`Unknown option: ${unknown[0]}\n`);
    printHelp();
    process.exit(1);
  }

  if (process.argv.includes("--help")) {
    printHelp();
    return;
  }

  const repoOverride = parseValueFlag("--repo");
  const cacheDirOverride = parseValueFlag("--cache-dir");

  if (process.argv.includes("--reset")) {
    await reset(repoOverride, cacheDirOverride);
    return;
  }

  const mcpProfile = parseMcpFlag();

  if (!mcpProfile) {
    const { setLogFile } = await import("./logger.js");
    const cacheDir = resolvePath(cacheDirOverride ?? process.env.KNOMIT_CACHE_DIR ?? join(homedir(), ".cache", "knomit"));
    setLogFile(join(cacheDir, "tui.log"));
  }

  const { repo, searchIndex } = await bootstrap({ repo: repoOverride, cacheDir: cacheDirOverride });

  if (mcpProfile) {
    await startMcp(repo, searchIndex, mcpProfile);
  } else {
    await startTui(repo, searchIndex);
  }
}

main().catch((err) => {
  log.error(`fatal: ${err}`);
  process.exit(1);
});
