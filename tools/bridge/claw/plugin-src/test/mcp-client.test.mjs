import { describe, it, expect } from "vitest";
import { McpStdioClient } from "../mcp-client.mjs";
import { PassThrough } from "node:stream";
import { EventEmitter } from "node:events";

// Fake bridge: echoes a result for each request id, and answers initialize.
// Modeled as an EventEmitter (like a real child_process.ChildProcess) so
// McpStdioClient can register "exit"/"error" listeners on it.
function fakeSpawn() {
  const stdin = new PassThrough();
  const stdout = new PassThrough();
  const proc = new EventEmitter();
  proc.stdin = stdin;
  proc.stdout = stdout;
  proc.killed = false;
  proc.kill = () => { proc.killed = true; stdout.end(); };
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
  return proc;
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
      const proc = new EventEmitter();
      proc.stdin = stdin;
      proc.stdout = stdout;
      proc.killed = false;
      proc.kill = () => { proc.killed = true; };
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
      return proc;
    };
    const client = new McpStdioClient({ spawnFn: spawnErr, logger: { info() {}, error() {} } });
    await client.start();
    await expect(client.request("tools/call", {})).rejects.toThrow("boom");
    client.stop();
  });

  it("rejects pending requests instead of hanging when the subprocess exits", async () => {
    // Fake process that answers `initialize` immediately (so start() resolves)
    // but never answers any subsequent request — simulating knomit-bridge
    // dying mid-request. proc is an EventEmitter so we can emit "exit".
    function fakeSpawnThatHangs() {
      const stdin = new PassThrough();
      const stdout = new PassThrough();
      const proc = new EventEmitter();
      proc.stdin = stdin;
      proc.stdout = stdout;
      proc.killed = false;
      proc.kill = () => { proc.killed = true; };
      stdin.on("data", (buf) => {
        for (const line of buf.toString().split("\n")) {
          if (!line.trim()) continue;
          const msg = JSON.parse(line);
          if (msg.method === "initialize") {
            stdout.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { ok: true } }) + "\n");
          }
          // Any other method (e.g. "tools/call") is left unanswered.
        }
      });
      return proc;
    }

    const client = new McpStdioClient({ spawnFn: fakeSpawnThatHangs, logger: { info() {}, error() {} } });
    await client.start();

    const pending = client.request("tools/call", { name: "knomit_query", arguments: {} });
    client.proc.emit("exit", 1);

    await expect(pending).rejects.toThrow("knomit-bridge exited");
  });
});
