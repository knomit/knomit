import { Database } from "bun:sqlite";
import { join, dirname } from "node:path";
import { mkdir } from "node:fs/promises";
import { existsSync } from "node:fs";
import { log } from "./logger";
import { parseFact } from "./facts";
import { ensureVecExtension, ensureOnnxModel } from "./assets";
import { bundledLibDir } from "./paths";
import { Embedder } from "./embeddings";
import type { GitRepo } from "./git";
import { ONTOLOGY_DIR } from "./constants.js";

export interface SearchIndexOptions {
  embeddings?: boolean;
}

export interface SearchQuery {
  text?: string;
  entities?: string[];
  domain?: string[];
  path?: string;
  min_confidence?: number;
  limit?: number;
}

export interface SearchResult {
  path: string;
  title: string;
  body: string;
  domain: string[];
  entities: string[];
  confidence: number;
  sources: number;
  refs: string[];
  commitHash: string;
  score: number;
}

export interface IndexedFact {
  title: string;
  body: string;
  domain: string[];
  entities: string[];
  confidence: number;
  sources: number;
  refs: string[];
  commitHash: string;
}

export interface StatsResult {
  totalFacts: number;
  avgConfidence: number;
  domainCounts: Record<string, number>;
  entityCounts: Record<string, number>;
}

/**
 * Replace macOS system SQLite with Homebrew's to get loadExtension support.
 * Called once at module load — must happen before any Database is created.
 */
const extensionSupportAvailable = (() => {
  if (process.platform !== "darwin") return true;
  const candidates = [
    join(bundledLibDir(), "libsqlite3.dylib"),           // bundled distribution
    "/opt/homebrew/opt/sqlite/lib/libsqlite3.dylib",     // Apple Silicon Homebrew
    "/usr/local/opt/sqlite/lib/libsqlite3.dylib",        // Intel Homebrew
  ];
  for (const path of candidates) {
    if (existsSync(path)) {
      try {
        Database.setCustomSQLite(path);
        return true;
      } catch {
        return false;
      }
    }
  }
  return false;
})();

export class SearchIndex {
  private db: Database | null = null;
  private readonly dbPath: string;
  private readonly cacheDir: string;
  private readonly embeddingsEnabled: boolean;
  private embedder: Embedder | null = null;

  constructor(cacheDir: string, options?: SearchIndexOptions) {
    this.dbPath = join(cacheDir, "index.db");
    this.cacheDir = cacheDir;
    this.embeddingsEnabled = options?.embeddings ?? false;
  }

  async init(): Promise<void> {
    await mkdir(dirname(this.dbPath), { recursive: true });

    if (this.embeddingsEnabled && !extensionSupportAvailable) {
      log.warn("search index: could not find a SQLite build with extension support — embeddings disabled");
    }

    this.db = new Database(this.dbPath);
    this.db.run("PRAGMA journal_mode = WAL");
    this.db.run("PRAGMA busy_timeout = 5000");

    this.db.run(`
      CREATE TABLE IF NOT EXISTS meta (
        key TEXT PRIMARY KEY,
        value TEXT NOT NULL
      )
    `);

    this.db.run(`
      CREATE TABLE IF NOT EXISTS synthesis_log (
        recipe TEXT NOT NULL,
        last_commit TEXT NOT NULL,
        run_at TEXT NOT NULL,
        facts_processed INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY (recipe)
      )
    `);

    this.db.run(`
      CREATE TABLE IF NOT EXISTS facts (
        path TEXT PRIMARY KEY,
        title TEXT NOT NULL,
        body TEXT NOT NULL,
        domain TEXT NOT NULL,
        entities TEXT NOT NULL,
        confidence REAL NOT NULL,
        sources INTEGER NOT NULL,
        refs TEXT NOT NULL,
        commit_hash TEXT NOT NULL
      )
    `);

    // FTS5 content-sync'd table
    const ftsExists = this.db
      .query("SELECT name FROM sqlite_master WHERE type='table' AND name='facts_fts'")
      .get();
    if (!ftsExists) {
      this.db.run(`
        CREATE VIRTUAL TABLE facts_fts USING fts5(
          title, body, entities, domain,
          content='facts',
          content_rowid='rowid'
        )
      `);
    }

    if (this.embeddingsEnabled) {
      try {
        const extPath = await ensureVecExtension(this.cacheDir);
        if (extPath) {
          this.db.loadExtension(extPath.replace(/\.(dylib|so|dll)$/, ""));

          const vecExists = this.db
            .query("SELECT name FROM sqlite_master WHERE type='table' AND name='facts_vec'")
            .get();
          if (!vecExists) {
            this.db.run(`
              CREATE VIRTUAL TABLE facts_vec USING vec0(
                path TEXT PRIMARY KEY,
                embedding FLOAT[384]
              )
            `);
          }

          const modelPaths = await ensureOnnxModel(this.cacheDir);
          if (modelPaths) {
            this.embedder = new Embedder();
            await this.embedder.init(modelPaths.model, modelPaths.tokenizer);
            log.info("search index: embeddings enabled");
          }
        }
      } catch (err) {
        log.warn(`search index: embeddings failed to initialize: ${err}`);
      }
    }

    log.info("search index initialized");
  }

  tableNames(): string[] {
    if (!this.db) throw new Error("SearchIndex not initialized");
    const rows = this.db
      .query("SELECT name FROM sqlite_master WHERE type IN ('table','shadow') ORDER BY name")
      .all() as Array<{ name: string }>;
    return rows.map((r) => r.name);
  }

  async upsert(path: string, fact: IndexedFact): Promise<void> {
    if (!this.db) throw new Error("SearchIndex not initialized");

    const domain = JSON.stringify(fact.domain);
    const entities = JSON.stringify(fact.entities);
    const refs = JSON.stringify(fact.refs);

    this.db.run("BEGIN");
    try {
      // Get existing rowid for FTS delete trigger
      const existing = this.db
        .query("SELECT rowid, title, body, entities, domain FROM facts WHERE path = ?")
        .get(path) as { rowid: number; title: string; body: string; entities: string; domain: string } | null;

      if (existing) {
        // Delete old FTS entry
        this.db.query(
          "INSERT INTO facts_fts(facts_fts, rowid, title, body, entities, domain) VALUES ('delete', ?, ?, ?, ?, ?)"
        ).run(existing.rowid, existing.title, existing.body, existing.entities, existing.domain);
      }

      // Upsert the fact row
      this.db.query(
        `INSERT OR REPLACE INTO facts (path, title, body, domain, entities, confidence, sources, refs, commit_hash)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
      ).run(path, fact.title, fact.body, domain, entities, fact.confidence, fact.sources, refs, fact.commitHash);

      // Get new rowid and insert into FTS
      const newRow = this.db
        .query("SELECT rowid FROM facts WHERE path = ?")
        .get(path) as { rowid: number };
      this.db.query(
        "INSERT INTO facts_fts(rowid, title, body, entities, domain) VALUES (?, ?, ?, ?, ?)"
      ).run(newRow.rowid, fact.title, fact.body, entities, domain);

      this.db.run("COMMIT");
    } catch (err) {
      this.db.run("ROLLBACK");
      throw err;
    }

    // Vector embedding (outside transaction — async and non-critical)
    if (this.embedder) {
      try {
        await this.embedFact(path, fact);
      } catch (err) {
        log.warn(`search index: embedding failed for ${path}: ${err}`);
      }
    }
  }

  private async embedFact(path: string, fact: IndexedFact): Promise<void> {
    if (!this.embedder || !this.db) return;
    const embeddingText = `${fact.title} ${fact.body} ${fact.entities.join(" ")} ${fact.domain.join(" ")}`;
    const vec = await this.embedder.embed(embeddingText);
    this.db.query("DELETE FROM facts_vec WHERE path = ?").run(path);
    this.db.query("INSERT INTO facts_vec (path, embedding) VALUES (?, ?)").run(
      path,
      new Float32Array(vec.buffer)
    );
  }

  remove(path: string): void {
    if (!this.db) throw new Error("SearchIndex not initialized");

    const existing = this.db
      .query("SELECT rowid, title, body, entities, domain FROM facts WHERE path = ?")
      .get(path) as { rowid: number; title: string; body: string; entities: string; domain: string } | null;

    if (existing) {
      this.db.run("BEGIN");
      try {
        this.db.query(
          "INSERT INTO facts_fts(facts_fts, rowid, title, body, entities, domain) VALUES ('delete', ?, ?, ?, ?, ?)"
        ).run(existing.rowid, existing.title, existing.body, existing.entities, existing.domain);
        this.db.query("DELETE FROM facts WHERE path = ?").run(path);
        this.db.run("COMMIT");
      } catch (err) {
        this.db.run("ROLLBACK");
        throw err;
      }
      // Clean up vector embedding (outside transaction — non-critical)
      if (this.embedder) {
        try { this.db.query("DELETE FROM facts_vec WHERE path = ?").run(path); } catch { /* table may not exist */ }
      }
    }
  }

  getFact(path: string): { title: string; confidence: number } | null {
    if (!this.db) throw new Error("SearchIndex not initialized");
    const row = this.db
      .query("SELECT title, confidence FROM facts WHERE path = ?")
      .get(path) as { title: string; confidence: number } | null;
    return row ?? null;
  }

  stats(pathPrefix?: string): StatsResult {
    if (!this.db) throw new Error("SearchIndex not initialized");

    const whereClause = pathPrefix ? "WHERE path LIKE ?" : "";
    const params = pathPrefix ? [`${pathPrefix}%`] : [];

    const agg = this.db
      .query(`SELECT COUNT(*) as total, AVG(confidence) as avg_conf FROM facts ${whereClause}`)
      .get(...params) as { total: number; avg_conf: number | null };

    const rows = this.db
      .query(`SELECT domain, entities FROM facts ${whereClause}`)
      .all(...params) as Array<{ domain: string; entities: string }>;

    const domainCounts: Record<string, number> = {};
    const entityCounts: Record<string, number> = {};
    for (const row of rows) {
      const domains: string[] = JSON.parse(row.domain);
      for (const d of domains) {
        domainCounts[d] = (domainCounts[d] ?? 0) + 1;
      }
      const entities: string[] = JSON.parse(row.entities);
      for (const e of entities) {
        entityCounts[e] = (entityCounts[e] ?? 0) + 1;
      }
    }

    return {
      totalFacts: agg.total,
      avgConfidence: agg.avg_conf ?? 0,
      domainCounts,
      entityCounts,
    };
  }

  async search(query: SearchQuery): Promise<SearchResult[]> {
    if (!this.db) throw new Error("SearchIndex not initialized");

    const limit = query.limit ?? 20;
    let rows: Array<Record<string, unknown>>;

    if (query.text) {
      // Quote each token to prevent FTS5 syntax interpretation (hyphens, colons, etc.)
      const ftsQuery = query.text
        .split(/\s+/)
        .filter(Boolean)
        .map((t) => `"${t.replace(/"/g, '""')}"`)
        .join(" ");
      // FTS5 search with BM25 ranking
      rows = this.db.query(`
        SELECT f.*, fts.rank as score
        FROM facts_fts fts
        JOIN facts f ON f.rowid = fts.rowid
        WHERE facts_fts MATCH ?
        ORDER BY fts.rank
        LIMIT ?
      `).all(ftsQuery, limit * 5) as Array<Record<string, unknown>>;
    } else {
      // No text search — scan facts table
      rows = this.db.query(`
        SELECT *, 0 as score FROM facts LIMIT ?
      `).all(limit * 5) as Array<Record<string, unknown>>;
    }

    let results: SearchResult[] = rows.map((r) => this.rowToResult(r, r.score as number));

    // Post-filters
    if (query.entities?.length) {
      results = results.filter((r) =>
        query.entities!.some((e) => r.entities.includes(e))
      );
    }
    if (query.domain?.length) {
      results = results.filter((r) =>
        query.domain!.some((d) => r.domain.includes(d))
      );
    }
    if (query.path) {
      results = results.filter((r) => r.path.startsWith(query.path!));
    }
    if (query.min_confidence != null) {
      results = results.filter((r) => r.confidence >= query.min_confidence!);
    }

    // Hybrid vector re-ranking when embeddings available
    if (this.embedder && query.text) {
      try {
        const queryVec = await this.embedder.embed(query.text);
        const vecRows = this.db.query(`
          SELECT path, distance
          FROM facts_vec
          WHERE embedding MATCH ?
          ORDER BY distance
          LIMIT ?
        `).all(new Float32Array(queryVec.buffer), limit * 5) as Array<{ path: string; distance: number }>;

        const vecMap = new Map(vecRows.map((r) => [r.path, r.distance]));

        // Normalize BM25 scores (negative, closer to 0 = better match)
        const minBm25 = Math.min(...results.map((r) => Math.abs(r.score)));
        const maxBm25 = Math.max(...results.map((r) => Math.abs(r.score)));
        const bm25Range = maxBm25 - minBm25;
        for (const r of results) {
          const normBm25 = bm25Range > 0 ? (maxBm25 - Math.abs(r.score)) / bm25Range : 1;
          const vecDist = vecMap.get(r.path) ?? 1.0;
          r.score = 0.6 * normBm25 + 0.4 * (1 - vecDist);
        }

        // Add vec-only results not in FTS results, but only if sufficiently similar
        const seenPaths = new Set(results.map((r) => r.path));
        for (const vr of vecRows) {
          if (!seenPaths.has(vr.path) && vr.distance < 0.8) {
            const fullRow = this.db!.query("SELECT * FROM facts WHERE path = ?").get(vr.path) as Record<string, unknown> | undefined;
            if (fullRow) {
              results.push(this.rowToResult(fullRow, 0.4 * (1 - vr.distance)));
            }
          }
        }

        results.sort((a, b) => b.score - a.score);
      } catch (err) {
        log.warn(`search index: vector search failed, using FTS only: ${err}`);
      }
    }

    // Normalize scores to 0-100
    const finalResults = results.slice(0, limit);
    if (finalResults.length > 0) {
      // BM25 scores are negative (closer to 0 = better); hybrid scores are positive (higher = better)
      const allNonPositive = finalResults.every((r) => r.score <= 0);
      if (allNonPositive) {
        const minAbs = Math.min(...finalResults.map((r) => Math.abs(r.score)));
        const maxAbs = Math.max(...finalResults.map((r) => Math.abs(r.score)));
        const range = maxAbs - minAbs;
        for (const r of finalResults) {
          r.score = range > 0
            ? Math.round(((maxAbs - Math.abs(r.score)) / range) * 100)
            : 100;
        }
      } else {
        const maxScore = Math.max(...finalResults.map((r) => r.score), 0.001);
        for (const r of finalResults) {
          r.score = Math.round((Math.max(0, r.score) / maxScore) * 100);
        }
      }
    }
    return finalResults.filter((r) => r.score >= 10);
  }

  private rowToResult(r: Record<string, unknown>, score: number): SearchResult {
    return {
      path: r.path as string,
      title: r.title as string,
      body: r.body as string,
      domain: JSON.parse(r.domain as string),
      entities: JSON.parse(r.entities as string),
      confidence: r.confidence as number,
      sources: r.sources as number,
      refs: JSON.parse(r.refs as string),
      commitHash: r.commit_hash as string,
      score,
    };
  }

  async rebuild(repo: GitRepo): Promise<void> {
    if (!this.db) throw new Error("SearchIndex not initialized");
    log.info("search index: full rebuild starting");

    this.db.run("BEGIN");
    try {
      this.db.run("DELETE FROM facts");
      this.db.run("INSERT INTO facts_fts(facts_fts) VALUES ('delete-all')");
      if (this.embedder) {
        try { this.db.run("DELETE FROM facts_vec"); } catch { /* table may not exist */ }
      }
      this.db.run("COMMIT");
    } catch (err) {
      this.db.run("ROLLBACK");
      throw err;
    }

    const head = await repo.headCommit();
    await this.indexDir(repo, ONTOLOGY_DIR, head);

    this.setMeta("last_commit", head);
    log.info("search index: rebuild complete");
  }

  private lastSeenHead: string | null = null;

  async sync(repo: GitRepo): Promise<boolean> {
    if (!this.db) throw new Error("SearchIndex not initialized");

    const lastCommit = this.getMeta("last_commit");
    const head = await repo.headCommit();

    if (!lastCommit) {
      await this.rebuild(repo);
      this.lastSeenHead = head;
      return true;
    }

    // Initialize in-memory tracker from DB on first call
    if (this.lastSeenHead === null) {
      this.lastSeenHead = lastCommit;
    }

    // Another process (e.g. MCP) already indexed this commit,
    // but we haven't seen it yet — signal data changed for UI refresh.
    if (lastCommit === head) {
      if (this.lastSeenHead !== head) {
        this.lastSeenHead = head;
        return true;
      }
      return false;
    }

    log.info(`search index: syncing from ${lastCommit.slice(0, 7)} to ${head.slice(0, 7)}`);
    const diff = await repo.diffFiles(lastCommit);

    for (const file of [...diff.added, ...diff.modified]) {
      if (file.endsWith(".md")) {
        await this.indexFile(repo, file, head);
      }
    }
    for (const file of diff.deleted) {
      this.remove(file);
    }

    this.setMeta("last_commit", head);
    this.lastSeenHead = head;
    return true;
  }

  private async indexDir(repo: GitRepo, path: string, commitHash: string): Promise<void> {
    const entries = await repo.listDir(path);
    for (const entry of entries) {
      const fullPath = `${path}/${entry.name}`;
      if (entry.isDirectory) {
        await this.indexDir(repo, fullPath, commitHash);
      } else if (entry.name.endsWith(".md")) {
        await this.indexFile(repo, fullPath, commitHash);
      }
    }
  }

  private async indexFile(repo: GitRepo, path: string, fallbackCommit: string): Promise<void> {
    try {
      const content = await repo.readFile(path);
      const parsed = parseFact(content);
      const commitHash = await repo.lastCommitForFile(path) ?? fallbackCommit;
      await this.upsert(path, {
        title: parsed.title,
        body: parsed.body,
        domain: parsed.frontmatter.domain,
        entities: parsed.frontmatter.entities,
        confidence: parsed.frontmatter.confidence,
        sources: parsed.frontmatter.sources,
        refs: parsed.frontmatter.refs,
        commitHash,
      });
    } catch {
      // Skip files that fail to parse
    }
  }

  private getMeta(key: string): string | null {
    if (!this.db) return null;
    const row = this.db.query("SELECT value FROM meta WHERE key = ?").get(key) as { value: string } | null;
    return row?.value ?? null;
  }

  private setMeta(key: string, value: string): void {
    if (!this.db) return;
    this.db.query("INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)").run(key, value);
  }

  get hasEmbeddings(): boolean {
    return this.embedder !== null;
  }

  getEmbedder(): Embedder | null {
    return this.embedder;
  }

  getEmbeddings(paths: string[]): Map<string, Float32Array> {
    if (!this.db || !this.embedder) return new Map();
    const result = new Map<string, Float32Array>();
    const stmt = this.db.query("SELECT path, embedding FROM facts_vec WHERE path = ?");
    for (const p of paths) {
      const row = stmt.get(p) as { path: string; embedding: Float32Array } | null;
      if (row) result.set(row.path, new Float32Array(row.embedding));
    }
    return result;
  }

  async reindex(repo: GitRepo): Promise<void> {
    if (!this.db) return;
    log.info("search index: reindexing from repo");
    const head = await repo.headCommit();
    this.db.run("BEGIN");
    try {
      this.db.run("DELETE FROM facts");
      this.db.run("INSERT INTO facts_fts(facts_fts) VALUES ('delete-all')");
      if (this.embedder) {
        try { this.db.run("DELETE FROM facts_vec"); } catch { /* table may not exist */ }
      }
      this.db.run("COMMIT");
    } catch (err) {
      this.db.run("ROLLBACK");
      throw err;
    }
    await this.indexDir(repo, ONTOLOGY_DIR, head);
    this.setMeta("last_commit", head);
  }

  getSynthesisLog(recipe: string): { lastCommit: string; runAt: string; factsProcessed: number } | null {
    if (!this.db) return null;
    const row = this.db
      .query("SELECT last_commit, run_at, facts_processed FROM synthesis_log WHERE recipe = ?")
      .get(recipe) as { last_commit: string; run_at: string; facts_processed: number } | null;
    if (!row) return null;
    return { lastCommit: row.last_commit, runAt: row.run_at, factsProcessed: row.facts_processed };
  }

  setSynthesisLog(recipe: string, lastCommit: string, factsProcessed: number): void {
    if (!this.db) return;
    this.db.query(
      "INSERT OR REPLACE INTO synthesis_log (recipe, last_commit, run_at, facts_processed) VALUES (?, ?, ?, ?)"
    ).run(recipe, lastCommit, new Date().toISOString(), factsProcessed);
  }

  close(): void {
    this.db?.close();
    this.db = null;
  }
}
