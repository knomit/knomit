import { describe, it, expect } from "vitest";
import { McpStdioClient } from "../mcp-client.mjs";
import { PassThrough } from "node:stream";

// Fake bridge: echoes a result for each request id, and answers initialize.
function fakeSpawn() {
  const stdin = new PassThrough();
  const stdout = new PassThrough();
  stdin.on("data", (buf) => {
    for (const line of buf.toString().split("\n")) {
      if (!line.trim()) continue;
      const msg = JSON.parse(line);
      let result = { echoed: msg.method };
      if (msg.method === "tools/list") result = { tools: [{ name: "knomit_query" }] };
      if (msg.method === "initialize") result = { serverInfo: { name: "knomit" } };
      stdout.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result }) + "\n");
    }
  });
  return { stdin, stdout, kill() { stdout.end(); }, killed: false };
}

describe("McpStdioClient", () => {
  it("correlates responses by id and lists tools after start", async () => {
    const client = new McpStdioClient({ spawnFn: fakeSpawn, logger: { info() {}, error() {} } });
    await client.start();
    const tools = await client.listTools();
    expect(tools).toEqual([{ name: "knomit_query" }]);
    client.stop();
  });

  it("rejects when the server returns a JSON-RPC error", async () => {
    const spawnErr = () => {
      const stdin = new PassThrough(); const stdout = new PassThrough();
      stdin.on("data", (buf) => {
        for (const line of buf.toString().split("\n")) {
          if (!line.trim()) continue;
          const msg = JSON.parse(line);
          const body = msg.method === "initialize"
            ? { result: { ok: true } }
            : { error: { code: -32603, message: "boom" } };
          stdout.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, ...body }) + "\n");
        }
      });
      return { stdin, stdout, kill() {}, killed: false };
    };
    const client = new McpStdioClient({ spawnFn: spawnErr, logger: { info() {}, error() {} } });
    await client.start();
    await expect(client.request("tools/call", {})).rejects.toThrow("boom");
    client.stop();
  });
});
