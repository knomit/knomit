import { defineCommand } from "citty";
import { globalArgs } from "./args";

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
  },
  async run({ args }) {
    const { join } = await import("node:path");
    const { readdir } = await import("node:fs/promises");
    const { setLogFile } = await import("../logger.js");
    const { bootstrap, resolveCacheDir } = await import("../bootstrap.js");
    const { parseRecipe } = await import("../recipe.js");
    const { synthesize } = await import("../synthesize.js");
    const { log } = await import("../logger.js");

    const cacheDir = resolveCacheDir(args["cache-dir"]);
    setLogFile(join(cacheDir, "synthesize.log"));

    const { repo, searchIndex } = await bootstrap({
      repo: args.repo,
      cacheDir: args["cache-dir"],
    });

    if (!searchIndex) {
      console.error("Search index is required for synthesis. Ensure cache directory is writable.");
      process.exit(1);
    }

    const recipesDir = join(repo.repoPath, ".knomit", "synthesize");

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
        try {
          const result = await synthesize(repo, searchIndex, recipe);
          console.log(`  Branch: ${result.branch}`);
          for (const s of result.stepSummaries) console.log(`  ${s}`);
          console.log(`  ${result.merged ? "Auto-merged" : "Pushed for review"}`);
        } catch (err) {
          console.error(`  Failed: ${err}`);
        }
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
      const result = await synthesize(repo, searchIndex, recipe);
      console.log(`Branch: ${result.branch}`);
      for (const s of result.stepSummaries) console.log(`  ${s}`);
      console.log(result.merged ? "Auto-merged" : "Pushed for review");
    } else {
      // Default: built-in prune+distill on changes since last run
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
      const result = await synthesize(repo, searchIndex, defaultRecipe);
      console.log(`Branch: ${result.branch}`);
      for (const s of result.stepSummaries) console.log(`  ${s}`);
      console.log(result.merged ? "Auto-merged" : "Pushed for review");
    }
  },
});
