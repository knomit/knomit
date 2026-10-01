// knomit: {"concurrent": 1, "timeout_ms": 1800000}
// The mission's sample recipe: the `wake` trigger runs it on the machine a
// working copy belongs to (inbox/{agent}/working/**). It starts one headless
// Claude Code session with this repo's knomit server as its MCP server, and
// tells it to fetch the work-task skill with the knomit_skill tool.
//
// The working copy is passed BY PATH (task_path). Its text never enters argv
// or the prompt: the session reads it through knomit, where it is data.
//
// The session is also handed a `trace` to pass on every knomit write for this
// task (#349), so the task's results are found with
// `git log --all --grep='^Knomit-Trace: <task id>'`. Knomit-Cause and
// Knomit-Run are knomit-made hex; Knomit-Trace is usually the task id, which a
// task author chose, so it is included only when it is a plain id. The object
// goes in as a JSON literal: data, not instructions.
//
// Recipes are read from the tip of the repo's consensus branch, so this file
// takes effect once it has been merged there. Edit the argv for another
// harness; keep the task out of it.
var trace = {"Knomit-Cause": change.commit, "Knomit-Run": run.id};
if (/^[A-Za-z0-9._:-]{1,256}$/.test(change.trace)) {
  trace["Knomit-Trace"] = change.trace;
}
const prompt = "A mission task is yours. Its working copy is " + task_path +
  ". Call the knomit_skill tool with name \"work-task\" and follow that skill for this working copy." +
  " Its trace is " + JSON.stringify(trace) + ".";
const r = knomit.exec(["claude", "--model", "sonnet",
  "--mcp-config", mcp.config, "--allowedTools", mcp.tools,
  "-p", prompt]);
({ status: r.exit === 0 ? "done" : "error", message: "exit " + r.exit });
