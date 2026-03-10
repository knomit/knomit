import type { GitRepo } from "../git.js";
import { ONTOLOGY_DIR } from "../constants.js";
import type { SearchIndex } from "../search-index.js";
import { queryHandler } from "../tools/query.js";
import { whyHandler } from "../tools/why.js";
import { exploreHandler } from "../tools/explore.js";
import { forgetHandler } from "../tools/forget.js";
import { parseFact, type Frontmatter } from "../facts.js";

export interface SessionState {
  currentFact: string | null;
  lastResults: Array<{ file: string; title: string }>;
}

export interface CommandContext {
  repo: GitRepo;
  searchIndex: SearchIndex;
  session: SessionState;
}

export type CommandResult = {
  updateSession?: Partial<SessionState>;
} & (
  | { type: "query"; facts: Array<{ file: string; title: string; body: string }> }
  | { type: "show"; file: string; title: string; body: string; frontmatter: Frontmatter }
  | { type: "history"; file: string; entries: Array<{ commit: string; date: string; message: string }> }
  | { type: "why"; file: string; data: Awaited<ReturnType<typeof whyHandler>> }
  | { type: "explore"; path: string; children: Array<{ name: string; type: string; summary?: string }>; inherited: Array<{ file: string; title: string }> }
  | { type: "forget"; file: string }
  | { type: "help" }
  | { type: "error"; message: string }
);

function resolveFile(
  arg: string | undefined,
  session: SessionState
): string | null {
  if (!arg) return session.currentFact;
  const num = parseInt(arg, 10);
  if (!isNaN(num) && num >= 1 && num <= session.lastResults.length) {
    return session.lastResults[num - 1]!.file;
  }
  return arg;
}

export async function dispatch(
  input: string,
  ctx: CommandContext
): Promise<CommandResult> {
  const { repo, searchIndex, session } = ctx;

  if (input.startsWith("/")) {
    const [cmd, ...args] = input.slice(1).split(/\s+/);
    const arg = args.join(" ") || undefined;

    switch (cmd) {
      case "show": {
        const file = resolveFile(arg, session);
        if (!file) return { type: "error", message: "No fact selected. Search for something first." };
        try {
          const raw = await repo.readFile(file);
          const parsed = parseFact(raw);
          return {
            type: "show",
            file,
            title: parsed.title,
            body: parsed.body,
            frontmatter: parsed.frontmatter,
            updateSession: { currentFact: file },
          };
        } catch {
          return { type: "error", message: `Could not read fact: ${file}` };
        }
      }

      case "history": {
        const file = resolveFile(arg, session);
        if (!file) return { type: "error", message: "No fact selected. Search for something first." };
        try {
          const entries = await repo.log(file);
          return { type: "history", file, entries };
        } catch {
          return { type: "error", message: `Could not load history for: ${file}` };
        }
      }

      case "why": {
        const file = resolveFile(arg, session);
        if (!file) return { type: "error", message: "No fact selected. Search for something first." };
        try {
          const data = await whyHandler(repo, { file });
          return {
            type: "why",
            file,
            data,
            updateSession: { currentFact: file },
          };
        } catch {
          return { type: "error", message: `Could not load provenance for: ${file}` };
        }
      }

      case "explore": {
        const path = arg ?? ONTOLOGY_DIR;
        try {
          const result = await exploreHandler(repo, { path });
          return {
            type: "explore",
            path,
            children: result.children,
            inherited: result.inherited_facts.map((f) => ({ file: f.file, title: f.title })),
          };
        } catch {
          return { type: "error", message: `Could not explore path: ${path}` };
        }
      }

      case "forget": {
        const file = resolveFile(arg, session);
        if (!file) return { type: "error", message: "No fact selected. Search for something first." };
        try {
          await forgetHandler(repo, { file, moment_name: "shell-delete" }, searchIndex);
          return {
            type: "forget",
            file,
            updateSession: { currentFact: null },
          };
        } catch {
          return { type: "error", message: `Could not delete fact: ${file}` };
        }
      }

      case "help":
        return { type: "help" };

      case "quit":
      case "exit":
        process.exit(0);

      default:
        return { type: "error", message: `Unknown command: /${cmd}. Type /help for available commands.` };
    }
  }

  // Bare text = natural language query
  try {
    const result = await queryHandler(repo, { text: input, min_confidence: 0 }, searchIndex);
    const facts = result.facts.map((f) => ({ file: f.file, title: f.title, body: f.body }));
    return {
      type: "query",
      facts,
      updateSession: {
        lastResults: facts.map((f) => ({ file: f.file, title: f.title })),
      },
    };
  } catch {
    return { type: "error", message: "Search failed. Try a different query." };
  }
}
