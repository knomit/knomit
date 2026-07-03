import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { buildToolDefs } from "./tools.mjs";
import { McpStdioClient } from "./mcp-client.mjs";
import { makeCallFn } from "./forward.mjs";

function defaultLoadManifest() {
  const url = new URL("./knomit-tools.json", import.meta.url);
  return JSON.parse(readFileSync(url, "utf8"));
}

function defaultMakeClient(api) {
  return new McpStdioClient({
    spawnFn: () => spawn("knomit-bridge", [], { stdio: ["pipe", "pipe", "inherit"] }),
    logger: api.logger,
  });
}

// registerKnomit is the testable core of register(); deps are injectable.
// It never imports openclaw — it receives `api` from the thin index.mjs entry.
export function registerKnomit(api, deps = {}) {
  const loadManifest = deps.loadManifest ?? defaultLoadManifest;
  const makeClient = deps.makeClient ?? (() => defaultMakeClient(api));

  const manifest = loadManifest();
  const client = makeClient();
  const call = makeCallFn(client);

  // Register synchronously from the local manifest (surfacing bug #47683).
  for (const def of buildToolDefs(manifest, call)) api.registerTool(def);

  api.registerService({
    id: "knomit-bridge",
    start: async () => { await client.start(); api.logger.info("knomit-bridge session ready"); },
    stop: () => { client.stop(); },
  });
}
