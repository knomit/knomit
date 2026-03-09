import { defineCommand } from "citty";
import { globalArgs } from "./args";
import type { ProgressEvent } from "../synthesize";

const DIM = "\x1b[2m";
const RESET = "\x1b[0m";
const BOLD = "\x1b[1m";
const GREEN = "\x1b[32m";
const YELLOW = "\x1b[33m";
const CYAN = "\x1b[36m";
const RED = "\x1b[31m";

function formatMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = (ms / 1000).toFixed(1);
  return `${s}s`;
}

function shortPath(path: string): string {
  return path.replace(/^worlds\//, "");
}

function createProgressRenderer(verbose: boolean) {
  return (event: ProgressEvent): void => {
    switch (event.phase) {
      case "step-start": {
        const label = event.mode === "prune" ? "Prune" : "Distill";
        console.log(`\n${BOLD}Step ${event.step + 1}/${event.totalSteps}: ${label}${RESET}`);
        break;
      }

      case "gather":
        if (event.facts === 0) {
          console.log(`${DIM}  No facts found${event.mode === "delta" ? " (no changes since last run)" : ""}${RESET}`);
        } else {
          const source = event.mode === "delta"
            ? (event.firstRun ? "all facts (first run)" : "changed since last run")
            : "by scope";
          console.log(`${CYAN}  Gathered ${BOLD}${event.facts}${RESET}${CYAN} facts ${DIM}(${source})${RESET}`);
        }
        break;

      case "llm": {
        const chunkInfo = event.totalChunks > 1 ? ` chunk ${event.chunk}/${event.totalChunks}` : "";
        console.log(`${DIM}  Sending ${event.facts} facts to LLM${chunkInfo} [${event.mode}]...${RESET}`);
        break;
      }

      case "llm-done":
        console.log(`${GREEN}  LLM responded${RESET} ${DIM}(${formatMs(event.elapsed)})${RESET}`);
        break;

      case "detail-keep":
        if (verbose) console.log(`${DIM}    keep  ${shortPath(event.path)}  ${event.reason}${RESET}`);
        break;

      case "detail-forget":
        if (verbose) console.log(`${RED}    forget  ${shortPath(event.path)}${RESET}  ${DIM}${event.reason}${RESET}`);
        break;

      case "detail-update":
        if (verbose) console.log(`${YELLOW}    update  ${shortPath(event.path)}${RESET}  confidence=${event.confidence}  ${DIM}${event.reason}${RESET}`);
        break;

      case "detail-merge":
        if (verbose) {
          const sources = event.sources.map(shortPath).join(", ");
          console.log(`${GREEN}    merge  ${sources}${RESET}  ${DIM}-> ${shortPath(event.target)}${RESET}`);
        }
        break;

      case "detail-learn":
        if (verbose) {
          const refs = event.refs.length > 0 ? `  ${DIM}from: ${event.refs.map(shortPath).join(", ")}${RESET}` : "";
          console.log(`${GREEN}    learn  ${shortPath(event.path)}${RESET}  ${event.body.slice(0, 80)}${event.body.length > 80 ? "..." : ""}${refs}`);
        }
        break;

      case "detail-distill-forget":
        if (verbose) console.log(`${RED}    forget  ${shortPath(event.path)}${RESET}  ${DIM}(subsumed)${RESET}`);
        break;

      case "apply":
        if (event.mode === "prune") {
          const parts: string[] = [];
          if (event.kept) parts.push(`${event.kept} kept`);
          if (event.forgotten) parts.push(`${YELLOW}${event.forgotten} forgotten${RESET}`);
          if (event.updated) parts.push(`${event.updated} updated`);
          if (event.merged) parts.push(`${GREEN}${event.merged} merged${RESET}`);
          console.log(`  ${BOLD}Prune:${RESET} ${parts.join(", ")}`);
        } else {
          const parts: string[] = [];
          if (event.learned) parts.push(`${GREEN}${event.learned} learned${RESET}`);
          if (event.forgotten) parts.push(`${YELLOW}${event.forgotten} forgotten${RESET}`);
          console.log(`  ${BOLD}Distill:${RESET} ${parts.join(", ")}`);
        }
        break;

      case "cross-chunk":
        console.log(`${DIM}  Cross-chunk consolidation (${event.facts} synthesized facts)...${RESET}`);
        break;

      case "reindex":
        console.log(`${DIM}  Re-indexing...${RESET}`);
        break;

      case "merge":
        console.log(`${DIM}  Auto-merging...${RESET}`);
        break;

      case "push":
        console.log(`${DIM}  Pushing branch for review...${RESET}`);
        break;

      case "done":
        console.log(`\n${GREEN}${BOLD}Done${RESET} ${DIM}(${formatMs(event.elapsed)})${RESET}`);
        break;
    }
  };
}

export default defineCommand({
  meta: {
    name: "synthesize",
    description: "Run synthesis recipes to prune and distill facts",
  },
  args: {
    ...globalArgs,
    recipe: {
      type: "string",
      description: "Recipe name (resolves to .knomit/synthesize/<name>.yml)",
    },
    all: {
      type: "boolean",
      description: "Run all recipes in .knomit/synthesize/",
      default: false,
    },
    verbose: {
      type: "boolean",
      alias: "v",
      description: "Show detailed per-fact decisions and reasons",
      default: false,
    },
  },
  async run({ args }) {
    const { join } = await import("node:path");
    const { readdir } = await import("node:fs/promises");
    const { setLogFile } = await import("../logger.js");
    const { bootstrap, resolveCacheDir } = await import("../bootstrap.js");
    const { parseRecipe } = await import("../recipe.js");
    const { synthesize } = await import("../synthesize.js");
    const { configFromEnv, resolveProvider } = await import("../llm.js");

    const cacheDir = resolveCacheDir(args["cache-dir"]);
    setLogFile(join(cacheDir, "synthesize.log"));

    // Validate LLM config early so we fail with a clear message
    try {
      const config = configFromEnv();
      resolveProvider(config.model, config.provider);
      const provider = config.provider ?? resolveProvider(config.model);
      if (provider === "gemini" && !config.apiKey && !process.env.GOOGLE_AI_API_KEY) {
        console.error("Error: GOOGLE_AI_API_KEY is required. Set it in your environment.");
        process.exit(1);
      }
      if (provider === "anthropic" && !config.apiKey && !process.env.ANTHROPIC_API_KEY) {
        console.error("Error: ANTHROPIC_API_KEY is required. Set it in your environment.");
        process.exit(1);
      }
      if (provider === "bedrock" && (!process.env.AWS_ACCESS_KEY_ID || !process.env.AWS_SECRET_ACCESS_KEY)) {
        console.error("Error: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required. Set them in your environment.");
        process.exit(1);
      }
    } catch (err: any) {
      console.error(`Error: ${err.message}`);
      process.exit(1);
    }

    const { repo, searchIndex } = await bootstrap({
      repo: args.repo,
      cacheDir: args["cache-dir"],
    });

    if (!searchIndex) {
      console.error("Error: Search index is required for synthesis. Ensure cache directory is writable.");
      process.exit(1);
    }

    const recipesDir = join(repo.repoPath, ".knomit", "synthesize");
    const onProgress = createProgressRenderer(!!args.verbose);

    const runRecipe = async (recipe: import("../recipe.js").Recipe) => {
      try {
        const result = await synthesize(repo, searchIndex, recipe, onProgress);
        if (!result.merged) {
          console.log(`Branch: ${result.branch}`);
        }
      } catch (err: any) {
        console.error(`Failed: ${err.message}`);
        process.exit(1);
      }
    };

    if (args.all) {
      let entries: string[];
      try {
        const dirEntries = await readdir(recipesDir);
        entries = dirEntries.filter((f) => f.endsWith(".yml") || f.endsWith(".yaml"));
      } catch {
        console.error(`No recipes found at ${recipesDir}`);
        process.exit(1);
      }

      for (const file of entries) {
        const raw = await Bun.file(join(recipesDir, file)).text();
        const recipe = parseRecipe(raw);
        console.log(`Running recipe: ${recipe.name}`);
        await runRecipe(recipe);
      }
    } else if (args.recipe) {
      const recipePath = join(recipesDir, `${args.recipe}.yml`);
      let raw: string;
      try {
        raw = await Bun.file(recipePath).text();
      } catch {
        console.error(`Recipe not found: ${recipePath}`);
        process.exit(1);
      }
      const recipe = parseRecipe(raw);
      console.log(`Running recipe: ${recipe.name}`);
      await runRecipe(recipe);
    } else {
      const defaultRecipe: import("../recipe.js").Recipe = {
        name: "default",
        prompt: "",
        scope: undefined,
        auto_merge: true,
        steps: [
          { mode: "prune", prompt: "" },
          { mode: "distill", prompt: "" },
        ],
      };
      console.log("Running default synthesis (prune + distill on recent changes)...");
      await runRecipe(defaultRecipe);
    }
  },
});
