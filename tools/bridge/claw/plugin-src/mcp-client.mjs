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

    // If knomit-bridge dies or errors mid-request, any pending promise would
    // otherwise hang forever. Drain and reject them all.
    this.proc.on("exit", () => this.#rejectPending(new Error("knomit-bridge exited")));
    this.proc.on("error", (err) => this.#rejectPending(new Error(`knomit-bridge error: ${err.message}`)));
    if (this.proc.stdin) {
      this.proc.stdin.on("error", (err) => this.#rejectPending(new Error(`knomit-bridge stdin error: ${err.message}`)));
    }

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
      try {
        this.proc.stdin.write(payload);
      } catch (err) {
        // A write to a dead stdin (e.g. EPIPE) would otherwise throw
        // uncaught; reject just this request instead.
        this.pending.delete(id);
        reject(new Error(`knomit-bridge write failed: ${err.message}`));
      }
    });
  }

  async listTools() {
    const result = await this.request("tools/list", {});
    return result.tools ?? [];
  }

  // #rejectPending drains and rejects every in-flight request. Used when the
  // subprocess dies or errors so callers don't hang forever on a promise
  // that will never resolve.
  #rejectPending(err) {
    for (const { reject } of this.pending.values()) reject(err);
    this.pending.clear();
  }

  stop() {
    this.#rejectPending(new Error("knomit-bridge stopped"));
    if (this.proc && !this.proc.killed) this.proc.kill();
  }
}
