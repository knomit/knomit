// makeCallFn returns the execute() forwarder used by every registered tool.
// Most tools do a single tools/call. knomit_review is long-running (60-120s)
// and is the only tool that opts into MCP task mode; we surface the wait to
// the agent ("working" + resume token) so execute() always returns fast.
//
// Wire shapes below are verified against github.com/mark3labs/mcp-go v0.45.0
// (mcp/types.go), not assumed:
//   - tools/call request augmented with task mode: params.task = { ttl }
//     (mcp.TaskParams, field name "ttl").
//   - task-start response: { task: { taskId, status, ... } }
//     (mcp.CreateTaskResult embeds Task under json tag "task").
//   - tasks/get request: { taskId }; response is the Task fields flattened
//     at the top level — { taskId, status, statusMessage, ... } — there is
//     NO result field here (mcp.GetTaskResult embeds Task directly).
//   - the actual result is a SEPARATE call, tasks/result, params { taskId },
//     which blocks server-side until the task is terminal and returns
//     { content, structuredContent, isError, _meta } (mcp.TaskResultResult),
//     or a JSON-RPC error if the task failed. McpStdioClient.request()
//     already rejects the promise on a JSON-RPC error, so a failed task
//     surfaces as a normal thrown Error from tasks/result.
export function makeCallFn(client) {
  return async function call(name, args) {
    if (name === "knomit_review") return reviewCall(client, args ?? {});
    return client.request("tools/call", { name, arguments: args ?? {} });
  };
}

async function reviewCall(client, args) {
  // Resume: the agent is polling a previously started task.
  if (args.resume) {
    const taskId = args.resume;
    const status = await client.request("tasks/get", { taskId });
    // TODO: input_required is treated the same as working (just keep polling)
    // because knomit_review has no elicitation today. If a future task-mode
    // tool needs mid-task input from the agent, input_required needs real
    // handling here (surfacing the elicitation request) instead of blind
    // polling.
    if (status.status === "working" || status.status === "input_required") {
      return { status: "working", resume: taskId };
    }
    // completed, failed, or cancelled: fetch the result. A failed/cancelled
    // task makes tasks/result reject (JSON-RPC error surfaced by the client
    // as a thrown Error), which we let propagate to the caller.
    return client.request("tasks/result", { taskId });
  }
  // Fresh start: request task mode, return the handle immediately.
  const { resume, ...rest } = args;
  const res = await client.request("tools/call", {
    name: "knomit_review",
    arguments: rest,
    task: { ttl: 300000 },
  });
  if (res.task?.taskId) return { status: "working", resume: res.task.taskId };
  return res; // server ran it synchronously (no task support) — pass through
}
