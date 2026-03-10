import { hostname, homedir } from "node:os";
import { exists } from "node:fs/promises";
import { join, resolve } from "node:path";
import { GitRepo } from "./git";
import { SearchIndex } from "./search-index";

export interface BootstrapOptions {
  repo?: string;
  cacheDir?: string;
  embeddings?: boolean;
}

export function resolvePath(raw: string): string {
  return raw.startsWith("~") ? resolve(homedir(), raw.slice(2)) : resolve(raw);
}

export function resolveRepo(override?: string): string {
  return resolvePath(override ?? process.env.KNOMIT_REPO ?? join(homedir(), ".knomit"));
}

export function resolveCacheDir(override?: string): string {
  return resolvePath(override ?? process.env.KNOMIT_CACHE_DIR ?? join(homedir(), ".cache", "knomit"));
}

export async function bootstrap(options?: BootstrapOptions) {
  const agentId = process.env.KNOMIT_AGENT_ID ?? hostname();
  const repoPath = resolveRepo(options?.repo);

  const repo = new GitRepo(repoPath, agentId);
  await repo.init();

  const cacheDir = resolveCacheDir(options?.cacheDir);
  const envEmbeddings = process.env.KNOMIT_EMBEDDINGS;
  const embeddingsEnabled = envEmbeddings !== undefined
    ? (envEmbeddings !== "0" && envEmbeddings !== "false")
    : (options?.embeddings ?? true);
  const searchIndex = new SearchIndex(cacheDir, { embeddings: embeddingsEnabled });
  await searchIndex.init();
  await searchIndex.sync(repo);

  return { repo, searchIndex, repoPath, agentId, cacheDir };
}

export async function reset(repoOverride?: string, cacheDirOverride?: string) {
  const repoPath = resolveRepo(repoOverride);
  const cacheDir = resolveCacheDir(cacheDirOverride);

  const { rmSync } = await import("node:fs");

  if (await exists(repoPath)) {
    rmSync(repoPath, { recursive: true, force: true });
    console.log(`removed repo: ${repoPath}`);
  } else {
    console.log(`repo not found: ${repoPath}`);
  }

  if (await exists(cacheDir)) {
    rmSync(cacheDir, { recursive: true, force: true });
    console.log(`removed cache: ${cacheDir}`);
  } else {
    console.log(`cache not found: ${cacheDir}`);
  }

  console.log("reset complete");
}
