// knomit: {"concurrent": 1, "timeout_ms": 1800000}
// Sample recipe (F07 PR 5): one headless Claude Code session per fire, with
// this repo's knomit server as its MCP server. The task is passed BY PATH;
// its text never enters argv or the prompt. Install it by copying it to
// <home>/recipes/claude-session.js, or commit it to .knomit/recipes/ on main.
const prompt = "A knomit task needs you: " + task_path +
  ". Read it with knomit_explain, do it, and record the result in knomit.";
const r = knomit.exec(["claude", "--model", "sonnet",
  "--mcp-config", mcp.config, "--allowedTools", mcp.tools,
  "-p", prompt]);
({ status: r.exit === 0 ? "done" : "error", message: "exit " + r.exit });
