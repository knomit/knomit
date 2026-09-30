// knomit: {"concurrent": 1, "timeout_ms": 1800000}
// The mission's sample recipe: the `wake` trigger runs it on the machine a
// working copy belongs to (inbox/{agent}/working/**). It starts one headless
// Claude Code session with this repo's knomit server as its MCP server, and
// tells it to fetch the work-task skill with the knomit_skill tool.
//
// The working copy is passed BY PATH (task_path). Its text never enters argv
// or the prompt: the session reads it through knomit, where it is data.
//
// Recipes are read from the tip of the repo's consensus branch, so this file
// takes effect once it has been merged there. Edit the argv for another
// harness; keep the task out of it.
const prompt = "A mission task is yours. Its working copy is " + task_path +
  ". Call the knomit_skill tool with name \"work-task\" and follow that skill for this working copy.";
const r = knomit.exec(["claude", "--model", "sonnet",
  "--mcp-config", mcp.config, "--allowedTools", mcp.tools,
  "-p", prompt]);
({ status: r.exit === 0 ? "done" : "error", message: "exit " + r.exit });
