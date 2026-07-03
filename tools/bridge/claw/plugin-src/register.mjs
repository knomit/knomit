import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { buildToolDefs } from "./tools.mjs";
import { McpStdioClient } from "./mcp-client.mjs";
import { makeCallFn } from "./forward.mjs";

function defaultLoadManifest() {
  const url = new URL("./knomit-tools.json", import.meta.url);
  return JSON.parse(readFileSync(url, "utf8"));
}

// bridgeArgs builds the knomit-bridge CLI args that scope the spawned
// process to the repo/source/profile this plugin was scaffolded for.
// Extracted so arg order can be unit-tested without spawning anything or
// importing openclaw.
export function bridgeArgs(cfg) {
  return ["--repo", cfg.repo, "--source", cfg.source, "--profile", cfg.profile];
}

function defaultMakeClient(api) {
  const cfgUrl = new URL("./bridge-config.json", import.meta.url);
  const cfg = JSON.parse(readFileSync(cfgUrl, "utf8"));
  const args = bridgeArgs(cfg);
  return new McpStdioClient({
    spawnFn: () => spawn("knomit-bridge", args, { stdio: ["pipe", "pipe", "inherit"] }),
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
