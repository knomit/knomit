import readline from "node:readline";

// McpStdioClient speaks newline-delimited JSON-RPC to a subprocess whose
// stdio behaves like an MCP server (knomit-bridge in its default proxy mode).
export class McpStdioClient {
  constructor({ spawnFn, logger }) {
    this.spawnFn = spawnFn;
    this.logger = logger;
    this.proc = null;
    this.nextId = 1;
    this.pending = new Map(); // id -> {resolve, reject}
  }

  async start() {
    this.proc = this.spawnFn();
    const rl = readline.createInterface({ input: this.proc.stdout });
    rl.on("line", (line) => this.#onLine(line));
    await this.request("initialize", {
      protocolVersion: "2025-06-18",
      capabilities: {},
      clientInfo: { name: "knomit-openclaw-plugin", version: "0.1.0" },
    });
  }

  #onLine(line) {
    const trimmed = line.trim();
    if (!trimmed) return;
    let msg;
    try { msg = JSON.parse(trimmed); } catch { return; }
    if (msg.id == null) return; // notification — ignore for now
    const p = this.pending.get(msg.id);
    if (!p) return;
    this.pending.delete(msg.id);
    if (msg.error) p.reject(new Error(msg.error.message ?? "mcp error"));
    else p.resolve(msg.result);
  }

  request(method, params) {
    const id = this.nextId++;
    const payload = JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n";
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.proc.stdin.write(payload);
    });
  }

  async listTools() {
    const result = await this.request("tools/list", {});
    return result.tools ?? [];
  }

  stop() {
    if (this.proc && !this.proc.killed) this.proc.kill();
  }
}
