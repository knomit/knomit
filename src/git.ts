import { exists, mkdir, readdir, readFile as fsReadFile, writeFile } from "node:fs/promises";
import { join, dirname, resolve } from "node:path";
import { hostname } from "node:os";
import { log } from "./logger";

export function toMomentTag(momentName: string): string {
  const safe = momentName.replace(/[^a-zA-Z0-9._/-]/g, "-");
  return `learn/${safe}`;
}

export interface CommitFile {
  path: string;
  content: string;
}

export interface LogEntry {
  commit: string;
  date: string;
  message: string;
}

export interface DirEntry {
  name: string;
  isDirectory: boolean;
}

export interface SyncResult {
  synced: boolean;
  conflict?: { files: string[]; message: string };
}

export class GitRepo {
  readonly repoPath: string;
  readonly machineId: string;
  private gitBin: string | null = null;

  constructor(repoPath: string, machineId?: string) {
    this.repoPath = repoPath;
    this.machineId = machineId ?? hostname();
  }

  get branchName(): string {
    return `machine/${this.machineId}`;
  }

  private async resolveGitBin(): Promise<string> {
    if (this.gitBin) return this.gitBin;

    // Try system git
    const which = Bun.spawnSync(["which", "git"]);
    if (which.exitCode === 0) {
      this.gitBin = new TextDecoder().decode(which.stdout).trim();
      return this.gitBin;
    }

    // Try vendored git
    const execDir = dirname(Bun.execPath);
    const vendored = join(execDir, "vendor", "git");
    if (await exists(vendored)) {
      this.gitBin = vendored;
      return this.gitBin;
    }

    throw new Error(
      "Git binary not found. Install git or place a static binary at <exec_dir>/vendor/git"
    );
  }

  private async git(
    ...args: string[]
  ): Promise<{ stdout: string; stderr: string; exitCode: number }> {
    const bin = await this.resolveGitBin();
    log.debug(`git ${args.join(" ")}`);
    const proc = Bun.spawnSync([bin, "-C", this.repoPath, ...args]);
    const result = {
      stdout: new TextDecoder().decode(proc.stdout).trim(),
      stderr: new TextDecoder().decode(proc.stderr).trim(),
      exitCode: proc.exitCode,
    };
    if (result.exitCode !== 0) {
      log.debug(`git ${args[0]} exited ${result.exitCode}: ${result.stderr}`);
    }
    return result;
  }

  private async gitOrThrow(...args: string[]): Promise<string> {
    const result = await this.git(...args);
    if (result.exitCode !== 0) {
      throw new Error(`git ${args.join(" ")} failed: ${result.stderr}`);
    }
    return result.stdout;
  }

  async init(): Promise<void> {
    const repoExists = await exists(join(this.repoPath, ".git"));
    if (repoExists) {
      log.debug(`repo exists at ${this.repoPath}`);
      // Ensure we're on the machine branch
      const branch = await this.currentBranch();
      if (branch !== this.branchName) {
        const { exitCode } = await this.git(
          "rev-parse",
          "--verify",
          this.branchName
        );
        if (exitCode === 0) {
          await this.gitOrThrow("checkout", this.branchName);
        } else {
          await this.gitOrThrow("checkout", "-b", this.branchName);
        }
      }
      return;
    }

    // Create repo
    log.info(`initializing new repo at ${this.repoPath}`);
    await mkdir(this.repoPath, { recursive: true });
    const bin = await this.resolveGitBin();
    Bun.spawnSync([bin, "init", this.repoPath]);

    // Configure for commits
    await this.gitOrThrow("config", "user.email", "knomit@local");
    await this.gitOrThrow("config", "user.name", "knomit");

    // Create root manifest
    const worldsMd = `---
domain: []
confidence: 1.0
sources: 1
entities: []
refs: []
---
# Knowledge Base

Root of the Knomit knowledge graph.
`;
    await writeFile(join(this.repoPath, "worlds.md"), worldsMd);
    await this.gitOrThrow("add", "worlds.md");
    await this.gitOrThrow("commit", "-m", "init: create knowledge base");

    // Rename default branch to main, then create machine branch
    await this.gitOrThrow("branch", "-M", "main");
    await this.gitOrThrow("checkout", "-b", this.branchName);
  }

  async currentBranch(): Promise<string> {
    return this.gitOrThrow("rev-parse", "--abbrev-ref", "HEAD");
  }

  async listBranches(): Promise<string[]> {
    const result = await this.gitOrThrow("branch", "--list", "--format=%(refname:short)");
    if (!result) return [];
    return result.split("\n").filter(Boolean);
  }

  async hasRemote(): Promise<boolean> {
    const { exitCode } = await this.git("remote", "get-url", "origin");
    return exitCode === 0;
  }

  async sync(): Promise<SyncResult> {
    const hasRemote = await this.hasRemote();
    if (!hasRemote) return { synced: false };

    const fetch = await this.git("fetch", "origin");
    if (fetch.exitCode !== 0) {
      log.warn(`fetch failed (proceeding offline): ${fetch.stderr}`);
      return { synced: false };
    }

    const { exitCode: mainExists } = await this.git(
      "rev-parse",
      "--verify",
      "origin/main"
    );
    if (mainExists !== 0) return { synced: false };

    const { stdout: behind } = await this.git(
      "rev-list",
      "--count",
      `${this.branchName}..origin/main`
    );
    if (behind === "0") return { synced: false };

    log.info(`syncing: ${behind} new commit(s) from origin/main`);
    const merge = await this.git("merge", "origin/main", "--no-edit");
    if (merge.exitCode !== 0) {
      log.warn("merge conflict detected, aborting merge");
      await this.git("merge", "--abort");
      const files = merge.stderr
        .split("\n")
        .filter((l) => l.includes("CONFLICT"))
        .map((l) => l.replace(/.*CONFLICT.*: /, "").replace(/ in /, ""));
      return {
        synced: false,
        conflict: { files, message: merge.stderr },
      };
    }

    return { synced: true };
  }

  async push(): Promise<void> {
    const hasRemote = await this.hasRemote();
    if (!hasRemote) return;

    const result = await this.git("push", "origin", this.branchName);
    if (result.exitCode !== 0) {
      log.warn(`push failed: ${result.stderr}`);
    }
  }

  validatePath(path: string): void {
    const full = resolve(this.repoPath, path);
    if (!full.startsWith(resolve(this.repoPath))) {
      throw new Error(`Path escapes repository: ${path}`);
    }
  }

  async commit(files: CommitFile[], message: string): Promise<string> {
    for (const file of files) {
      this.validatePath(file.path);
      const fullPath = join(this.repoPath, file.path);
      await mkdir(dirname(fullPath), { recursive: true });
      await writeFile(fullPath, file.content);
    }

    await this.gitOrThrow("add", ...files.map((f) => f.path));
    await this.gitOrThrow("commit", "-m", message);
    return this.gitOrThrow("rev-parse", "--short", "HEAD");
  }

  async deleteFile(path: string, message: string): Promise<string> {
    this.validatePath(path);
    if (!(await this.fileExists(path))) {
      throw new Error(`File not found: ${path}`);
    }
    await this.gitOrThrow("rm", path);
    await this.gitOrThrow("commit", "-m", message);
    return this.gitOrThrow("rev-parse", "--short", "HEAD");
  }

  async tag(name: string): Promise<string> {
    const { exitCode } = await this.git("tag", name);
    if (exitCode !== 0) {
      const suffixed = `${name}-${Math.floor(Date.now() / 1000)}`;
      await this.gitOrThrow("tag", suffixed);
      return suffixed;
    }
    return name;
  }

  async listTags(): Promise<string[]> {
    const stdout = await this.gitOrThrow("tag", "--list");
    return stdout ? stdout.split("\n") : [];
  }

  async log(file: string): Promise<LogEntry[]> {
    const stdout = await this.gitOrThrow(
      "log",
      "--follow",
      "--format=%H|%aI|%s",
      "--",
      file
    );
    if (!stdout) return [];
    return stdout.split("\n").map((line) => {
      const [commit, date, message] = line.split("|", 3);
      return { commit: commit!, date: date!, message: message! };
    });
  }

  async readFile(path: string): Promise<string> {
    this.validatePath(path);
    const fullPath = join(this.repoPath, path);
    return fsReadFile(fullPath, "utf-8");
  }

  async fileExists(path: string): Promise<boolean> {
    this.validatePath(path);
    return exists(join(this.repoPath, path));
  }

  async listDir(path: string): Promise<DirEntry[]> {
    const fullPath = join(this.repoPath, path);
    if (!(await exists(fullPath))) return [];
    const entries = await readdir(fullPath, { withFileTypes: true });
    return entries
      .filter((e) => !e.name.startsWith("."))
      .map((e) => ({ name: e.name, isDirectory: e.isDirectory() }));
  }

  async grep(pattern: string, path?: string): Promise<string[]> {
    const searchPath = path ?? "worlds/";
    const result = await this.git("grep", "-rl", "--", pattern, searchPath);
    if (result.exitCode !== 0) return [];
    return result.stdout ? result.stdout.split("\n") : [];
  }

  async headCommit(): Promise<string> {
    return this.gitOrThrow("rev-parse", "HEAD");
  }

  async diffFiles(fromCommit: string): Promise<{ added: string[]; modified: string[]; deleted: string[] }> {
    const result = await this.git("diff", "--name-status", fromCommit, "HEAD", "--", "worlds/");
    if (result.exitCode !== 0 || !result.stdout) {
      return { added: [], modified: [], deleted: [] };
    }
    const added: string[] = [];
    const modified: string[] = [];
    const deleted: string[] = [];
    for (const line of result.stdout.split("\n")) {
      const [status, file] = line.split("\t", 2);
      if (!file) continue;
      if (status === "A") added.push(file);
      else if (status === "M") modified.push(file);
      else if (status === "D") deleted.push(file);
    }
    return { added, modified, deleted };
  }

  async tagsContaining(commit: string): Promise<string[]> {
    const result = await this.git("tag", "--contains", commit);
    if (result.exitCode !== 0) return [];
    return result.stdout ? result.stdout.split("\n") : [];
  }

  async commitsBetweenTags(
    tag: string
  ): Promise<Array<{ commit: string; file: string; message: string }>> {
    // Find the previous learn/ tag to establish the range
    const allTags = await this.listTags();
    const learnTags = allTags.filter((t) => t.startsWith("learn/"));

    // Get tag dates to sort them
    const tagDates: Array<{ tag: string; date: string }> = [];
    for (const t of learnTags) {
      const result = await this.git("log", "-1", "--format=%aI", t);
      if (result.exitCode === 0 && result.stdout) {
        tagDates.push({ tag: t, date: result.stdout });
      }
    }
    tagDates.sort((a, b) => a.date.localeCompare(b.date));

    const tagIndex = tagDates.findIndex((t) => t.tag === tag);
    const prevTag = tagIndex > 0 ? tagDates[tagIndex - 1]!.tag : null;

    // Build the range: from previous tag to this tag
    const range = prevTag ? `${prevTag}..${tag}` : tag;

    const stdout = await this.gitOrThrow(
      "log",
      "--format=%H|%s",
      "--name-only",
      range
    );
    if (!stdout) return [];

    const results: Array<{
      commit: string;
      file: string;
      message: string;
    }> = [];
    const lines = stdout.split("\n");
    let current: { commit: string; message: string } | null = null;

    for (const line of lines) {
      if (line.includes("|")) {
        const [commit, message] = line.split("|", 2);
        current = { commit: commit!, message: message! };
      } else if (line.trim() && current) {
        results.push({ ...current, file: line.trim() });
      }
    }
    return results;
  }
}
