import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import { buildToolDefs } from "./tools.mjs";

export default definePluginEntry({
  id: "knomit",
  name: "knomit",
  description: "knomit knowledge base tools, proxied through knomit-bridge.",
  register(api) {
    // Skeleton: register a single hard-coded tool to confirm surfacing.
    const defs = buildToolDefs(
      [{ name: "knomit_ping", description: "ping", inputSchema: { type: "object" } }],
      async () => ({ pong: true }),
    );
    for (const d of defs) api.registerTool(d);
    api.registerService({
      id: "knomit-bridge",
      start: () => api.logger.info("knomit skeleton service start"),
      stop: () => api.logger.info("knomit skeleton service stop"),
    });
  },
});
