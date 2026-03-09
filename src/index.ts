import { defineCommand, runMain, renderUsage } from "citty";
import { globalArgs } from "./cli/args";

const stripAnsi = (s: string) => s.replace(/\x1b\[[0-9;]*m/g, "");

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
    synthesize: () => import("./cli/synthesize.js").then((m) => m.default),
  },
  async run({ args, rawArgs }) {
    // Skip TUI if a subcommand was invoked
    const subCmds = ["mcp", "reset", "synthesize"];
    if (rawArgs.some((a: string) => subCmds.includes(a))) return;

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

process.on("uncaughtException", (err) => {
  console.error(`Error: ${err.message}`);
  process.exit(1);
});

process.on("unhandledRejection", (err: any) => {
  console.error(`Error: ${err?.message ?? err}`);
  process.exit(1);
});

runMain(main, {
  showUsage: async (cmd, parent) => {
    const text = await renderUsage(cmd, parent);
    console.log(stripAnsi(text));
  },
});
