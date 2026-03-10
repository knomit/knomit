import { test, expect, beforeAll, afterAll } from "bun:test";
import { GitRepo } from "./git";
import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";

let repo: GitRepo;
let dir: string;

beforeAll(async () => {
  dir = await mkdtemp(join(tmpdir(), "knomit-branches-"));
  repo = new GitRepo(dir, "test-agent");
  await repo.init();
});

afterAll(async () => {
  await rm(dir, { recursive: true });
});

test("listBranches includes main and agent branch", async () => {
  const branches = await repo.listBranches();
  expect(branches).toContain("main");
  expect(branches).toContain("agent/test-agent");
});
