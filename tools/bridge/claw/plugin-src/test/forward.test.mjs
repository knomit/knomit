import { describe, it, expect } from "vitest";
import { makeCallFn } from "../forward.mjs";

function fakeClient(script) {
  // script: array of {method, match?, reply}
  const calls = [];
  return {
    calls,
    async request(method, params) {
      calls.push({ method, params });
      const step = script.shift();
      if (!step) throw new Error("unexpected request " + method);
      return step.reply(params);
    },
  };
}

describe("makeCallFn", () => {
  it("forwards a plain tool call once", async () => {
    const client = fakeClient([
      { method: "tools/call", reply: () => ({ content: [{ type: "text", text: "{\"rows\":[]}" }] }) },
    ]);
    const call = makeCallFn(client);
    const out = await call("knomit_query", { text: "x" });
    expect(client.calls[0]).toEqual({ method: "tools/call", params: { name: "knomit_query", arguments: { text: "x" } } });
    expect(out.content[0].text).toContain("rows");
  });

  it("review start requests task mode and returns working+resume", async () => {
    const client = fakeClient([
      { method: "tools/call", reply: () => ({ task: { taskId: "t-1", status: "working" } }) },
    ]);
    const call = makeCallFn(client);
    const out = await call("knomit_review", {});
    // task mode requested
    expect(client.calls[0].params.task).toBeTruthy();
    expect(out).toEqual({ status: "working", resume: "t-1" });
  });

  it("review resume polls tasks/get, then fetches tasks/result when completed", async () => {
    const client = fakeClient([
      { method: "tasks/get", reply: () => ({ taskId: "t-1", status: "completed" }) },
      { method: "tasks/result", reply: () => ({ content: [{ type: "text", text: "done" }] }) },
    ]);
    const call = makeCallFn(client);
    const out = await call("knomit_review", { resume: "t-1" });
    expect(client.calls[0]).toEqual({ method: "tasks/get", params: { taskId: "t-1" } });
    expect(client.calls[1]).toEqual({ method: "tasks/result", params: { taskId: "t-1" } });
    expect(out.content[0].text).toBe("done");
  });

  it("review resume still working returns working+resume again", async () => {
    const client = fakeClient([
      { method: "tasks/get", reply: () => ({ taskId: "t-1", status: "working" }) },
    ]);
    const call = makeCallFn(client);
    const out = await call("knomit_review", { resume: "t-1" });
    expect(out).toEqual({ status: "working", resume: "t-1" });
  });

  it("review resume failed task surfaces the tasks/result error", async () => {
    const client = fakeClient([
      { method: "tasks/get", reply: () => ({ taskId: "t-1", status: "failed" }) },
      { method: "tasks/result", reply: () => { throw new Error("review task failed: boom"); } },
    ]);
    const call = makeCallFn(client);
    await expect(call("knomit_review", { resume: "t-1" })).rejects.toThrow("review task failed");
  });
});
