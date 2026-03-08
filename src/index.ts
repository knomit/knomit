import { defineCommand, runMain } from "citty";
import { globalArgs } from "./cli/args";

const main = defineCommand({
  meta: {
    name: "knomit",
    version: "0.1.0",
    description: "Git-backed knowledge base for AI agents",
  },
  args: {
    ...globalArgs,
  },
  subCommands: {
    mcp: () => import("./cli/mcp.js").then((m) => m.default),
    reset: () => import("./cli/reset.js").then((m) => m.default),
  },
  async run({ args }) {
    // Default (no subcommand) = TUI
    const { join } = await import("node:path");
    const { setLogFile } = await import("./logger.js");
    const { bootstrap, resolveCacheDir } = await import("./bootstrap.js");

    const cacheDir = resolveCacheDir(args["cache-dir"]);
    setLogFile(join(cacheDir, "tui.log"));

    const { repo, searchIndex } = await bootstrap({
      repo: args.repo,
      cacheDir: args["cache-dir"],
    });

    const { startApp } = await import("./tui/App.js");
    startApp(repo, searchIndex);
  },
});

runMain(main);
