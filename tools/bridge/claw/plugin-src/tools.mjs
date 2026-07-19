import { Type } from "@sinclair/typebox";

// buildToolDefs turns an MCP tools/list manifest into OpenClaw tool defs.
// `call(name, args)` is the forwarder to the bridge; injected for testability.
// Type.Unsafe wraps the raw MCP JSON Schema as a TypeBox schema untouched.
export function buildToolDefs(manifest, call) {
  return manifest.map((t) => ({
    name: t.name,
    description: t.description ?? "",
    input: Type.Unsafe(t.inputSchema ?? { type: "object" }),
    execute: async (args) => call(t.name, args ?? {}),
  }));
}
