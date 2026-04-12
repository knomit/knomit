/**
 * MCP JSON-RPC client over HTTP streamable transport.
 *
 * Speaks to mcp-go's StreamableHTTPServer which may respond with either
 * plain JSON or SSE (text/event-stream) containing JSON-RPC messages.
 */

export interface McpTool {
  name: string;
  description?: string;
  inputSchema?: Record<string, unknown>;
}

export interface McpToolResult {
  content: Array<{ type: string; text?: string }>;
  isError?: boolean;
}

interface JsonRpcRequest {
  jsonrpc: "2.0";
  id?: number;
  method: string;
  params?: Record<string, unknown>;
}

interface JsonRpcResponse {
  jsonrpc: "2.0";
  id: number;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
}

export class McpClient {
  private readonly url: string;
  private sessionId: string | null = null;
  private nextId = 1;

  constructor(
    baseURL: string,
    repo = "knomit",
    profile = "code",
    branch?: string,
  ) {
    const base = baseURL.replace(/\/$/, "");
    const encodedBranch = (branch ?? "agent").replaceAll("/", ":");
    this.url = `${base}/api/v1/repos/${repo}/branches/${encodedBranch}/mcp?profile=${profile}`;
  }

  /** Send initialize, capture session ID, then send initialized notification. */
  async initialize(): Promise<void> {
    const resp = await this.send("initialize", {
      protocolVersion: "2025-03-26",
      capabilities: {},
      clientInfo: { name: "knomit-e2e", version: "0.1.0" },
    });

    if (resp.error) {
      throw new Error(`initialize failed: ${resp.error.message}`);
    }

    // Fire-and-forget initialized notification (no id, no response expected).
    await this.notify("notifications/initialized");
  }

  /** List available tools from the server. */
  async listTools(): Promise<McpTool[]> {
    const resp = await this.send("tools/list", {});
    if (resp.error) {
      throw new Error(`tools/list failed: ${resp.error.message}`);
    }
    const result = resp.result as { tools?: McpTool[] } | undefined;
    return result?.tools ?? [];
  }

  /** Call a single tool by name with the given arguments. */
  async callTool(
    name: string,
    args: Record<string, unknown>,
  ): Promise<McpToolResult> {
    const resp = await this.send("tools/call", { name, arguments: args });
    if (resp.error) {
      throw new Error(`tools/call ${name} failed: ${resp.error.message}`);
    }
    return resp.result as McpToolResult;
  }

  /** Best-effort session teardown via DELETE. */
  async close(): Promise<void> {
    if (!this.sessionId) return;
    try {
      await fetch(this.url, {
        method: "DELETE",
        headers: this.sessionHeaders(),
      });
    } catch {
      // best effort — ignore errors
    }
  }

  // -- internals ----------------------------------------------------------

  /** Send a JSON-RPC request (has id) and return the parsed response. */
  private async send(
    method: string,
    params: Record<string, unknown>,
  ): Promise<JsonRpcResponse> {
    const id = this.nextId++;
    const body: JsonRpcRequest = { jsonrpc: "2.0", id, method, params };

    const res = await fetch(this.url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        ...this.sessionHeaders(),
      },
      body: JSON.stringify(body),
    });

    if (!res.ok) {
      throw new Error(`HTTP ${res.status} ${res.statusText} on ${method}`);
    }

    // Capture session ID from response headers.
    const sid = res.headers.get("Mcp-Session-Id");
    if (sid) {
      this.sessionId = sid;
    }

    const contentType = res.headers.get("Content-Type") ?? "";

    if (contentType.includes("text/event-stream")) {
      return this.parseSSE(res, id);
    }

    // Plain JSON response.
    return (await res.json()) as JsonRpcResponse;
  }

  /** Send a JSON-RPC notification (no id, no response expected). */
  private async notify(method: string): Promise<void> {
    const body: JsonRpcRequest = { jsonrpc: "2.0", method };

    await fetch(this.url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        ...this.sessionHeaders(),
      },
      body: JSON.stringify(body),
    });
  }

  /** Parse an SSE stream and return the JSON-RPC response matching `id`. */
  private async parseSSE(
    res: Response,
    id: number,
  ): Promise<JsonRpcResponse> {
    const text = await res.text();
    for (const line of text.split("\n")) {
      const trimmed = line.trim();
      if (!trimmed.startsWith("data:")) continue;
      const payload = trimmed.slice("data:".length).trim();
      if (!payload || payload === "[DONE]") continue;
      try {
        const msg = JSON.parse(payload) as JsonRpcResponse;
        if (msg.id === id) return msg;
      } catch {
        // skip non-JSON data lines
      }
    }
    throw new Error(`No JSON-RPC response with id=${id} found in SSE stream`);
  }

  private sessionHeaders(): Record<string, string> {
    if (!this.sessionId) return {};
    return { "Mcp-Session-Id": this.sessionId };
  }
}
