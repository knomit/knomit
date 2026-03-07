import { z } from "zod";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { GitRepo } from "../git";
import { parseFact } from "../facts";
import type { Frontmatter } from "../facts";
import { log } from "../logger";

const WhyInput = z.object({
  file: z.string(),
});

type WhyInputType = z.infer<typeof WhyInput>;

interface WhyResult {
  fact: {
    file: string;
    frontmatter: Frontmatter;
    title: string;
    body: string;
  };
  learning_moment: {
    tag: string;
    date: string;
    siblings: Array<{ file: string; title: string; commit: string }>;
  };
  refs: string[];
  history: Array<{ commit: string; date: string; message: string }>;
}

export async function whyHandler(
  repo: GitRepo,
  input: WhyInputType
): Promise<WhyResult> {
  log.info(`why: file="${input.file}"`);
  const syncResult = await repo.sync();
  if (syncResult.conflict) {
    throw new Error(`Merge conflict from origin/main. Conflicting files: ${syncResult.conflict.files.join(", ")}. Resolve with knomit_update then retry.`);
  }

  const history = await repo.log(input.file);

  if (history.length === 0) {
    return {
      fact: {
        file: input.file,
        frontmatter: {
          domain: [],
          confidence: 0,
          sources: 0,
          entities: [],
          refs: [],
        },
        title: "",
        body: "",
      },
      learning_moment: { tag: "", date: "", siblings: [] },
      refs: [],
      history: [],
    };
  }

  // Read and parse the fact
  const raw = await repo.readFile(input.file);
  const parsed = parseFact(raw);

  // Find the learning moment: get the earliest commit, find tags containing it
  const earliestCommit = history[history.length - 1]!;
  const tags = await repo.tagsContaining(earliestCommit.commit);
  const learnTag = tags.find((t) => t.startsWith("learn/")) ?? "";

  // Find siblings from the same learning moment
  let siblings: Array<{ file: string; title: string; commit: string }> = [];
  let tagDate = earliestCommit.date;

  if (learnTag) {
    const commits = await repo.commitsBetweenTags(learnTag);
    siblings = commits
      .filter((c) => c.file !== input.file)
      .map((c) => ({
        file: c.file,
        title: c.message.replace(/^learn: /, ""),
        commit: c.commit,
      }));
  }

  log.info(`why: tag="${learnTag}", ${history.length} history entries, ${siblings.length} sibling(s)`);
  return {
    fact: {
      file: input.file,
      frontmatter: parsed.frontmatter,
      title: parsed.title,
      body: parsed.body,
    },
    learning_moment: {
      tag: learnTag,
      date: tagDate,
      siblings,
    },
    refs: parsed.frontmatter.refs,
    history,
  };
}

export function registerWhyTool(server: McpServer, repo: GitRepo): void {
  server.tool(
    "knomit_why",
    "Explain the provenance of a fact: when it was learned, what learning moment it belongs to, and what sibling facts were learned at the same time.",
    WhyInput.shape,
    async (input) => {
      const parsed = WhyInput.parse(input);
      const result = await whyHandler(repo, parsed);
      return {
        content: [{ type: "text" as const, text: JSON.stringify(result) }],
      };
    }
  );
}
