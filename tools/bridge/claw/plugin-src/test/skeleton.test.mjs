import { describe, it, expect } from "vitest";
import { buildToolDefs } from "../tools.mjs";

describe("buildToolDefs", () => {
  it("maps an MCP manifest entry to an OpenClaw tool def with a raw-JSON-Schema input", () => {
    const manifest = [{
      name: "knomit_query",
      description: "Search the knowledge base.",
      inputSchema: { type: "object", properties: { text: { type: "string" } } },
    }];
    const call = async () => ({ ok: true });
    const defs = buildToolDefs(manifest, call);
    expect(defs).toHaveLength(1);
    expect(defs[0].name).toBe("knomit_query");
    expect(defs[0].description).toBe("Search the knowledge base.");
    // Type.Unsafe passes the JSON Schema through untouched.
    expect(defs[0].input.type).toBe("object");
    expect(defs[0].input.properties.text.type).toBe("string");
    expect(typeof defs[0].execute).toBe("function");
  });

  it("execute forwards {name, arguments} to the injected call fn and returns its result", async () => {
    const seen = [];
    const call = async (name, args) => { seen.push([name, args]); return { rows: [] }; };
    const defs = buildToolDefs(
      [{ name: "knomit_query", description: "d", inputSchema: { type: "object" } }],
      call,
    );
    const out = await defs[0].execute({ text: "hi" });
    expect(seen).toEqual([["knomit_query", { text: "hi" }]]);
    expect(out).toEqual({ rows: [] });
  });
});
