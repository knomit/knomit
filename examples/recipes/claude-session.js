// knomit: {"concurrent": 1, "timeout_ms": 1800000}
// Sample recipe (F07 PR 5): one headless Claude Code session per fire, with
// this repo's knomit server as its MCP server. `kb` in mcp.config names no
// address: knomit starts this recipe's programs with KNOMIT_SERVER set to its
// own, and Claude Code passes it on to `kb`. The task is passed BY PATH;
// its text never enters argv or the prompt. Install it by copying it to
// <home>/recipes/claude-session.js, or commit it to .knomit/recipes/ on main.
//
// The session is handed a `trace` (#349) to pass on every knomit write for
// this task, so `git log --all --grep='^Knomit-Run: <run.id>'` finds what it
// wrote. Knomit-Cause and Knomit-Run are knomit-made hex. Knomit-Trace can be
// a task's entity — text a task author chose — so it is included only when
// it is a plain id; the whole object goes in as a JSON literal, as data.
var trace = {"Knomit-Cause": change.commit, "Knomit-Run": run.id};
if (/^[A-Za-z0-9._:-]{1,256}$/.test(change.trace)) {
  trace["Knomit-Trace"] = change.trace;
}
const prompt = "A knomit task needs you: " + task_path +
  ". Read it with knomit_explain, do it, and record the result in knomit." +
  " On every knomit write for this task (knomit_learn, knomit_update, knomit_retract," +
  " and knomit_review or knomit_hypothesize if you run them for it), pass this trace" +
  " argument, unchanged: " + JSON.stringify(trace) + ".";
const r = knomit.exec(["claude", "--model", "sonnet",
  "--mcp-config", mcp.config, "--allowedTools", mcp.tools,
  "-p", prompt]);
({ status: r.exit === 0 ? "done" : "error", message: "exit " + r.exit });
