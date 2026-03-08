import { exists, mkdir, readdir } from "node:fs/promises";
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
  episode?: string;
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
    const maxRetries = 5;

    for (let attempt = 0; attempt <= maxRetries; attempt++) {
      log.debug(`git ${args.join(" ")}${attempt > 0 ? ` (retry ${attempt})` : ""}`);
      const proc = Bun.spawnSync([bin, "-C", this.repoPath, ...args]);
      const result = {
        stdout: new TextDecoder().decode(proc.stdout).trim(),
        stderr: new TextDecoder().decode(proc.stderr).trim(),
        exitCode: proc.exitCode,
      };

      if (result.exitCode !== 0) {
        // Retry on lock contention
        if (attempt < maxRetries && (result.stderr.includes("could not lock") || result.stderr.includes(".lock"))) {
          const delay = 50 * Math.pow(2, attempt) + Math.random() * 50;
          log.debug(`git lock contention, retrying in ${Math.round(delay)}ms`);
          await Bun.sleep(delay);
          continue;
        }
        log.debug(`git ${args[0]} exited ${result.exitCode}: ${result.stderr}`);
      }
      return result;
    }

    // Unreachable, but satisfies TypeScript
    throw new Error(`git ${args.join(" ")} failed after ${maxRetries} retries`);
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
    await Bun.write(join(this.repoPath, "worlds.md"), worldsMd);
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
      await Bun.write(fullPath, file.content);
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
      "--decorate-refs=refs/tags/learn/",
      "--format=%H|%aI|%s|%D",
      "--",
      file
    );
    if (!stdout) return [];
    const entries = stdout.split("\n").map((line) => {
      const [commit, date, message, decor] = line.split("|", 4);
      const tag = decor?.match(/tag: learn\/([^\s,)]+)/)?.[1];
      return { commit: commit!, date: date!, message: message!, episode: tag };
    });
    // Walk newest-first: a learn/ tag marks the episode for itself and
    // all older commits until the next tag.
    let currentEpisode: string | undefined;
    for (const entry of entries) {
      if (entry.episode) {
        currentEpisode = entry.episode;
      } else {
        entry.episode = currentEpisode;
      }
    }
    return entries;
  }

  async readFile(path: string): Promise<string> {
    this.validatePath(path);
    const fullPath = join(this.repoPath, path);
    return Bun.file(fullPath).text();
  }

  async readFileAtCommit(path: string, commit: string): Promise<string> {
    return this.gitOrThrow("show", `${commit}:${path}`);
  }

  async diffFileAtCommit(path: string, commit: string): Promise<Set<number>> {
    const result = await this.git("diff", "-U0", `${commit}^`, commit, "--", path);
    if (result.exitCode !== 0 || !result.stdout) return new Set();
    const added = new Set<number>();
    let newLineNum = 0;
    for (const line of result.stdout.split("\n")) {
      if (line.startsWith("@@")) {
        const match = line.match(/\+(\d+)/);
        if (match) newLineNum = parseInt(match[1]!, 10);
        continue;
      }
      if (line.startsWith("+") && !line.startsWith("+++")) {
        added.add(newLineNum);
        newLineNum++;
      } else if (!line.startsWith("-") || line.startsWith("---")) {
        newLineNum++;
      }
    }
    return added;
  }

  private parseDiffNameStatus(stdout: string, stripPrefix?: string): { added: string[]; modified: string[]; deleted: string[] } {
    const added: string[] = [];
    const modified: string[] = [];
    const deleted: string[] = [];
    for (const line of stdout.split("\n")) {
      const [status, file] = line.split("\t", 2);
      if (!file) continue;
      const name = stripPrefix && file.startsWith(stripPrefix) ? file.slice(stripPrefix.length) : file;
      if (status === "A") added.push(name);
      else if (status === "M") modified.push(name);
      else if (status === "D") deleted.push(name);
    }
    return { added, modified, deleted };
  }

  private dirPrefix(path: string): string {
    return path.endsWith("/") ? path : `${path}/`;
  }

  async diffAtCommit(commit: string, path: string): Promise<{ added: string[]; modified: string[]; deleted: string[] }> {
    const prefix = this.dirPrefix(path);
    const result = await this.git("diff", "--name-status", `${commit}^`, commit, "--", prefix);
    if (result.exitCode !== 0 || !result.stdout) {
      return { added: [], modified: [], deleted: [] };
    }
    return this.parseDiffNameStatus(result.stdout, prefix);
  }

  async listDirAtCommit(path: string, commit: string): Promise<DirEntry[]> {
    const prefix = this.dirPrefix(path);
    const result = await this.git("ls-tree", commit, prefix);
    if (result.exitCode !== 0 || !result.stdout) return [];
    return result.stdout.split("\n").filter(Boolean).map((line) => {
      // format: <mode> <type> <hash>\t<path>
      const [meta, fullPath] = line.split("\t", 2);
      const type = meta!.split(" ")[1];
      const name = fullPath!.startsWith(prefix) ? fullPath!.slice(prefix.length) : fullPath!;
      return { name, isDirectory: type === "tree" };
    });
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
    return this.parseDiffNameStatus(result.stdout);
  }

  async tagsContaining(commit: string): Promise<string[]> {
    const result = await this.git("tag", "--contains", commit);
    if (result.exitCode !== 0) return [];
    return result.stdout ? result.stdout.split("\n") : [];
  }

  async tagsAt(commit: string): Promise<string[]> {
    const result = await this.git("tag", "--points-at", commit);
    if (result.exitCode !== 0) return [];
    return result.stdout ? result.stdout.split("\n").filter(Boolean) : [];
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
