/**
 * Shared E2E test helpers: creates isolated repos + search indexes per test.
 */
import { mkdtemp, rm, mkdir } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { GitRepo } from "../git";
import { SearchIndex } from "../search-index";

export interface TestEnv {
  repoPath: string;
  repo: GitRepo;
  cacheDir: string;
  searchIndex: SearchIndex;
}

/** Create an isolated repo + search index in a temp directory. */
export async function createTestEnv(agentId = "test-agent"): Promise<TestEnv> {
  const base = await mkdtemp(join(tmpdir(), "knomit-e2e-"));
  const repoPath = join(base, "repo");
  const cacheDir = join(base, "cache");
  await mkdir(cacheDir, { recursive: true });

  const repo = new GitRepo(repoPath, agentId);
  await repo.init();

  const searchIndex = new SearchIndex(cacheDir);
  await searchIndex.init();
  await searchIndex.sync(repo);

  return { repoPath, repo, cacheDir, searchIndex };
}

/** Clean up a test environment. */
export async function destroyTestEnv(env: TestEnv): Promise<void> {
  env.searchIndex.close();
  // repoPath parent is the base temp dir
  const base = join(env.repoPath, "..");
  await rm(base, { recursive: true, force: true });
}
