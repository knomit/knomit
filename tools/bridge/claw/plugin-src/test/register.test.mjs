import { describe, it, expect } from "vitest";
import { registerKnomit } from "../register.mjs";

it("registers one tool per manifest entry and wires the service", async () => {
  const registered = [];
  let service = null;
  const api = {
    logger: { info() {}, error() {} },
    registerTool: (t) => registered.push(t.name),
    registerService: (s) => { service = s; },
  };
  const manifest = [
    { name: "knomit_query", description: "q", inputSchema: { type: "object" } },
    { name: "knomit_review", description: "r", inputSchema: { type: "object" } },
  ];
  // Inject a no-op client factory so no real subprocess spawns.
  registerKnomit(api, {
    loadManifest: () => manifest,
    makeClient: () => ({ start: async () => {}, stop() {}, request: async () => ({}) }),
  });
  expect(registered).toEqual(["knomit_query", "knomit_review"]);
  expect(service).toBeTruthy();
  await service.start();
  service.stop();
});
