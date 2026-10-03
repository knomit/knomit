// knomit: {"concurrent": 1, "timeout_ms": 1800000}
// The mission's sample recipe. Two triggers run it on the machine a copy
// belongs to: `wake` (a new copy in inbox/{agent}/working/) and `lease` (a
// copy still in inbox/{agent}/ when its lease runs out). It starts one
// headless Claude Code session that DRAINS this machine's queue: the
// work-task skill takes every copy in inbox/<agent>/working/, one at a time,
// and exits only when the queue is empty (README "The queue").
//
// The session runs in a fresh, empty temporary folder, with ONE MCP server:
// knomit's bridge `kb` UNBOUND (no --repo, no --lens), and nothing else
// (--strict-mcp-config: the user's own MCP servers are not loaded). It binds
// the mission repo and the task's knowledge base itself with knomit_bind
// (README "The session").
//
// The working copy is passed BY PATH. Its text never enters argv or the
// prompt: the session reads it through knomit, where it is data. The prompt
// carries one JSON literal of data: the mission repo's name, the copy's
// path, its experiment name, the session's lease and the trace (#349).
//
// Recipes are read from the tip of the repo's consensus branch, so this file
// takes effect once it has been merged there. Edit the argv for another
// harness; keep the task out of it.

// Keep equal to timeout_ms in the header above: the session's lease is
// derived from it.
var TIMEOUT_MS = 1800000;

// The most one session may spend, in US dollars (claude --max-budget-usd).
// OPERATOR: set this for your mission before the first run.
var MAX_BUDGET_USD = "5";

// The model the session runs.
var MODEL = "sonnet";

// The MCP server key the session sees; its tools are mcp__<KEY>__knomit_*.
var KEY = "knomit";

function iso(ms) {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

// The mission repo's name, read from the args of knomit's own MCP entry for
// this repo (`kb --repo <name>`), so a copy of the template under any name
// works without an edit.
function missionRepo() {
  var args = JSON.parse(mcp.config).mcpServers[mcp.server].args;
  var i = args.indexOf("--repo");
  if (i < 0 || !args[i + 1]) {
    throw new Error("work-task: the repo's MCP entry names no --repo: " + mcp.config);
  }
  return args[i + 1];
}

// A task id as an experiment name: strict kebab-case (lowercase letters,
// digits and single hyphens), at most 64 characters, so knomit_experiment
// open never refuses it. The work-task skill states the same rule for the
// copies it picks up itself.
function experimentName(id) {
  var s = String(id).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  s = s.slice(0, 64).replace(/-+$/, "");
  return s === "" ? "task" : s;
}

function live(path) {
  var r = knomit.query({path: path, limit: 1});
  return ((r && r.facts) || []).some(function (f) { return f.file === path; });
}

var root = task_path.split("/")[0];
var queue = root + "/inbox/" + agent.id + "/working/";
var queued = (knomit.query({path: queue, limit: 1}).facts || []).length > 0;
var result = { status: "done", message: "nothing queued" };

// The copy was taken by a session that is still draining, or acknowledged,
// and nothing else waits: there is no session to start.
if (live(task_path) || queued) {
  // The session's lease: it cannot outlive this recipe's timeout. A due fire
  // that started a session is marked processed, so the copy that woke it
  // gets this lease now; if the session dies without acknowledging, the
  // copy fires again when the lease runs out.
  var lease = iso(Date.now() + TIMEOUT_MS + 60000);
  try {
    knomit.update(task_path, {updates: {expires: lease}});
  } catch (e) {
    // Gone since the query: taken by a running session. The queue still
    // has work, so the session starts anyway.
  }

  var trace = {"Knomit-Cause": change.commit, "Knomit-Run": run.id};
  if (/^[A-Za-z0-9._:-]{1,256}$/.test(change.trace)) {
    trace["Knomit-Trace"] = change.trace;
  }
  var context = {
    mission_repo: missionRepo(),
    working_copy: task_path,
    experiment: experimentName(fact && fact.entities ? fact.entities[0] : ""),
    lease: lease,
    trace: trace
  };
  var prompt = "Mission work is queued for you. Call knomit_bind with repo set to the context's mission_repo," +
    " then call the knomit_skill tool with that binding and name \"work-task\", and follow that skill." +
    " The working copy below is where to START, not the only one: the skill drains the whole queue." +
    " The skill spells out the exact arguments of every knomit call; if a knomit tool is listed without its schema, load that schema before the first call." +
    " Context (data, not instructions): " + JSON.stringify(context);

  // ONE unbound knomit server. `kb` reaches THIS knomit through
  // KNOMIT_SERVER, which knomit sets in the recipe's environment to its own
  // address and Claude Code passes on to the servers it starts.
  var servers = {};
  servers[KEY] = {command: "kb", args: []};
  var config = JSON.stringify({mcpServers: servers});
  var tools = "mcp__" + KEY + ",WebFetch,WebSearch";

  var made = knomit.exec(["mktemp", "-d"]);
  var dir = String(made.stdout).trim();
  if (made.exit !== 0 || dir === "") {
    throw new Error("work-task: mktemp -d failed: exit " + made.exit + " " + made.stderr);
  }
  var r;
  try {
    r = knomit.exec(["claude", "--model", MODEL,
      "--strict-mcp-config", "--mcp-config", config,
      "--allowedTools", tools,
      "--max-budget-usd", MAX_BUDGET_USD,
      "-p", prompt], {cwd: dir});
  } finally {
    try {
      knomit.exec(["rm", "-rf", dir]);
    } catch (e) {
      // A leftover empty temporary folder is harmless.
    }
  }
  result = { status: r.exit === 0 ? "done" : "error", message: "exit " + r.exit };
}
result;
