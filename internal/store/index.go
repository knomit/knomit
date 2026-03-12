// Package store implements the knomit search index backed by SQLite FTS5.
// Build with -tags fts5 to enable FTS5 support (required).
package store

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog/log"
)

var vecOnce sync.Once

func registerVec() {
	vecOnce.Do(func() { sqlite_vec.Auto() })
}

type indexConfig struct {
	vecDim int
}

// Option configures an Index.
type Option func(*indexConfig)

// WithVecDimension sets the dimension of the facts_vec embedding column.
func WithVecDimension(d int) Option {
	return func(c *indexConfig) { c.vecDim = d }
}

// Sync brings the index up to date with the git store.
//
// Algorithm:
//  1. Read meta.last_commit.
//  2. If missing → full rebuild (ListAll, index everything).
//  3. If last_commit == HEAD → no-op.
//  4. Else → DiffFiles(last_commit), upsert added+modified, delete removed.
//  5. Update meta.last_commit = HEAD.
func (idx *Index) Sync(git GitReader) error {
	head, err := git.HeadCommit()
	if err != nil {
		return fmt.Errorf("sync: head commit: %w", err)
	}

	last, err := idx.GetLastCommit()
	if err != nil {
		return fmt.Errorf("sync: get last commit: %w", err)
	}

	if last == head {
		log.Debug().Str("head", head[:8]).Msg("index sync: already at HEAD, skipping")
		return nil
	}

	if last == "" {
		// Full rebuild.
		log.Info().Str("head", head[:8]).Msg("index sync: full rebuild (no previous commit)")
		paths, err := git.ListAll()
		if err != nil {
			return fmt.Errorf("sync: list all: %w", err)
		}
		for _, path := range paths {
			if err := idx.indexFile(git, path, head); err != nil {
				return err
			}
		}
		log.Info().Int("files", len(paths)).Msg("index sync: full rebuild complete")
	} else {
		// Incremental update.
		added, modified, deleted, err := git.DiffFiles(last)
		if err != nil {
			return fmt.Errorf("sync: diff files: %w", err)
		}
		log.Debug().
			Str("from", last[:8]).Str("to", head[:8]).
			Int("added", len(added)).Int("modified", len(modified)).Int("deleted", len(deleted)).
			Msg("index sync: incremental update")
		for _, path := range append(added, modified...) {
			if err := idx.indexFile(git, path, head); err != nil {
				return err
			}
		}
		for _, path := range deleted {
			if err := idx.Delete(path); err != nil {
				return fmt.Errorf("sync: delete %q: %w", path, err)
			}
		}
	}

	return idx.SetLastCommit(head)
}

// indexFile reads path from git, parses the frontmatter, and upserts into the index.
func (idx *Index) indexFile(git GitReader, path, commitHash string) error {
	content, err := git.ReadFile(path)
	if err != nil {
		return fmt.Errorf("sync: read %q: %w", path, err)
	}

	rec, err := parseFact(path, content, commitHash)
	if err != nil {
		// Skip files that cannot be parsed as facts (e.g. know.md manifest).
		return nil
	}

	return idx.Upsert(rec)
}

// parseFact parses a knomit fact markdown file into a FactRecord.
// Expected format:
//
//	---
//	domain: [databases, sql]
//	confidence: 0.9
//	sources: 2
//	entities: [postgres, mysql]
//	refs: []
//	---
//	# Title of the fact
//
//	Body content.
func parseFact(path, content, commitHash string) (FactRecord, error) {
	// Split on "---" delimiters.
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return FactRecord{}, fmt.Errorf("parseFact: no frontmatter in %q", path)
	}

	frontmatter := parts[1]
	body := strings.TrimSpace(parts[2])

	// Parse frontmatter lines.
	var domain []string
	var entities []string
	var refs []string
	var confidence float64
	var sources int

	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		switch k {
		case "domain":
			domain = parseYAMLList(v)
		case "entities":
			entities = parseYAMLList(v)
		case "refs":
			refs = parseYAMLList(v)
		case "confidence":
			fmt.Sscanf(v, "%f", &confidence)
		case "sources":
			fmt.Sscanf(v, "%d", &sources)
		}
	}

	// Extract title from the first heading line.
	title := ""
	rest := body
	if strings.HasPrefix(body, "#") {
		nl := strings.IndexByte(body, '\n')
		if nl < 0 {
			title = strings.TrimSpace(strings.TrimLeft(body, "#"))
			rest = ""
		} else {
			title = strings.TrimSpace(body[:nl])
			title = strings.TrimSpace(strings.TrimLeft(title, "#"))
			rest = strings.TrimSpace(body[nl+1:])
		}
	}

	if title == "" {
		return FactRecord{}, fmt.Errorf("parseFact: no title heading in %q", path)
	}

	return FactRecord{
		Path:       path,
		Title:      title,
		Body:       rest,
		Domain:     domain,
		Entities:   entities,
		Confidence: confidence,
		Sources:    sources,
		Refs:       refs,
		CommitHash: commitHash,
	}, nil
}

// parseYAMLList parses a simple YAML inline list like "[a, b, c]" or "[]".
func parseYAMLList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	v = strings.TrimSpace(v)
	if v == "" {
		return []string{}
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GitReader is the interface that SearchIndex.Sync requires from the git store.
type GitReader interface {
	// DiffFiles returns paths added, modified, and deleted between fromCommit and HEAD.
	DiffFiles(fromCommit string) (added, modified, deleted []string, err error)
	// ReadFile reads the content of path from the HEAD commit.
	ReadFile(path string) (string, error)
	// HeadCommit returns the hash of the current HEAD commit as a hex string.
	HeadCommit() (string, error)
	// ListAll returns paths of all .md files from HEAD.
	ListAll() ([]string, error)
}

func schemaSQL(vecDim int) string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS synthesis_log (
    recipe          TEXT PRIMARY KEY,
    last_commit     TEXT NOT NULL,
    run_at          TEXT NOT NULL,
    facts_processed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS facts (
    path        TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,
    domain      TEXT NOT NULL,
    entities    TEXT NOT NULL,
    confidence  REAL NOT NULL,
    sources     INTEGER NOT NULL,
    refs        TEXT NOT NULL,
    commit_hash TEXT NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS facts_fts USING fts5(
    title, body, entities, domain,
    content='facts', content_rowid='rowid'
);
CREATE VIRTUAL TABLE IF NOT EXISTS facts_vec USING vec0(
    embedding FLOAT[%d] distance_metric=cosine
);`, vecDim)
}

// FactRecord represents a single fact stored in the index.
type FactRecord struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Refs       []string `json:"refs"`
	CommitHash string   `json:"commit_hash,omitempty"`
}

// Embedder is the interface used by Index to compute embedding vectors.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// Index is the search index backed by SQLite with FTS5.
type Index struct {
	db      *sql.DB
	embedder Embedder
}

// SetEmbedder attaches an Embedder to the index. When set, Upsert will call
// Embed on each record's Body and persist the result as vec_data.
func (idx *Index) SetEmbedder(e Embedder) {
	idx.embedder = e
}

// New opens (or creates) a SQLite search index at path.
// Use ":memory:" for an in-memory database (useful in tests).
func New(path string, opts ...Option) (*Index, error) {
	registerVec()

	cfg := indexConfig{vecDim: 768}
	for _, o := range opts {
		o(&cfg)
	}

	dsn := path
	if path != ":memory:" {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL(cfg.vecDim)); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if _, err = db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('schema_version', '3')`); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema_version: %w", err)
	}

	return &Index{db: db}, nil
}

// DB returns the underlying *sql.DB handle.
func (idx *Index) DB() *sql.DB { return idx.db }

// Close closes the underlying database connection.
func (idx *Index) Close() error {
	return idx.db.Close()
}

// Upsert inserts or replaces a FactRecord, keeping the FTS5 index in sync.
func (idx *Index) Upsert(rec FactRecord) error {
	domainJSON, err := json.Marshal(rec.Domain)
	if err != nil {
		return fmt.Errorf("marshal domain: %w", err)
	}
	entitiesJSON, err := json.Marshal(rec.Entities)
	if err != nil {
		return fmt.Errorf("marshal entities: %w", err)
	}
	refsJSON, err := json.Marshal(rec.Refs)
	if err != nil {
		return fmt.Errorf("marshal refs: %w", err)
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Step 1: Read old row values (if any) so we can issue the FTS5 explicit delete command.
	var oldRowid int64
	var oldTitle, oldBody, oldEntities, oldDomain string
	var hasOld bool
	err = tx.QueryRow(
		`SELECT rowid, title, body, entities, domain FROM facts WHERE path=?`,
		rec.Path,
	).Scan(&oldRowid, &oldTitle, &oldBody, &oldEntities, &oldDomain)
	if err == nil {
		hasOld = true
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("read old fact: %w", err)
	}

	// Compute embedding vector if an embedder is configured.
	var vecData []byte
	if idx.embedder != nil {
		vec, err := idx.embedder.Embed(rec.Body)
		if err == nil && len(vec) > 0 {
			vecData = float32SliceToBytes(vec)
		}
	}

	// Step 2: Insert or replace into facts
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO facts(path, title, body, domain, entities, confidence, sources, refs, commit_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.Title, rec.Body,
		string(domainJSON), string(entitiesJSON),
		rec.Confidence, rec.Sources,
		string(refsJSON), rec.CommitHash,
	); err != nil {
		return fmt.Errorf("upsert fact: %w", err)
	}

	// Step 3: If an old row existed, remove it from FTS5 using the explicit 'delete' command.
	if hasOld {
		if _, err := tx.Exec(
			`INSERT INTO facts_fts(facts_fts, rowid, title, body, entities, domain)
			 VALUES('delete', ?, ?, ?, ?, ?)`,
			oldRowid, oldTitle, oldBody, oldEntities, oldDomain,
		); err != nil {
			return fmt.Errorf("delete old fts row: %w", err)
		}
	}

	// Step 4: Insert new row into FTS using the new rowid
	if _, err := tx.Exec(
		`INSERT INTO facts_fts(rowid, title, body, entities, domain)
		 VALUES ((SELECT rowid FROM facts WHERE path=?), ?, ?, ?, ?)`,
		rec.Path, rec.Title, rec.Body, string(entitiesJSON), string(domainJSON),
	); err != nil {
		return fmt.Errorf("insert fts row: %w", err)
	}

	// Step 5: Insert embedding into facts_vec.
	if vecData != nil {
		newRowid := int64(0)
		_ = tx.QueryRow(`SELECT rowid FROM facts WHERE path=?`, rec.Path).Scan(&newRowid)
		if newRowid > 0 {
			if _, err := tx.Exec(`DELETE FROM facts_vec WHERE rowid = ?`, newRowid); err != nil {
				return fmt.Errorf("delete old vec row: %w", err)
			}
			if _, err := tx.Exec(
				`INSERT INTO facts_vec(rowid, embedding) VALUES (?, ?)`,
				newRowid, vecData,
			); err != nil {
				return fmt.Errorf("insert vec row: %w", err)
			}
		}
	}

	return tx.Commit()
}

// Delete removes a fact and its FTS entry by path.
func (idx *Index) Delete(path string) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Read old values to issue the FTS5 explicit 'delete' command.
	var oldRowid int64
	var oldTitle, oldBody, oldEntities, oldDomain string
	err = tx.QueryRow(
		`SELECT rowid, title, body, entities, domain FROM facts WHERE path=?`,
		path,
	).Scan(&oldRowid, &oldTitle, &oldBody, &oldEntities, &oldDomain)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read fact for delete: %w", err)
	}
	if err == nil {
		// Delete from facts_vec first (referential integrity).
		if _, err := tx.Exec(`DELETE FROM facts_vec WHERE rowid = ?`, oldRowid); err != nil {
			return fmt.Errorf("delete vec row: %w", err)
		}
		// Delete from FTS5.
		if _, err := tx.Exec(
			`INSERT INTO facts_fts(facts_fts, rowid, title, body, entities, domain)
			 VALUES('delete', ?, ?, ?, ?, ?)`,
			oldRowid, oldTitle, oldBody, oldEntities, oldDomain,
		); err != nil {
			return fmt.Errorf("delete fts row: %w", err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM facts WHERE path=?`, path); err != nil {
		return fmt.Errorf("delete fact: %w", err)
	}

	return tx.Commit()
}

// GetByPath retrieves a FactRecord by its path. Returns nil, nil if not found.
func (idx *Index) GetByPath(path string) (*FactRecord, error) {
	row := idx.db.QueryRow(
		`SELECT path, title, body, domain, entities, confidence, sources, refs, commit_hash
		 FROM facts WHERE path=?`,
		path,
	)
	return scanFactRecord(row)
}

// GetEmbedding returns the stored embedding vector for a fact.
// Returns nil, nil if not found.
func (idx *Index) GetEmbedding(path string) ([]float32, error) {
	var blob []byte
	err := idx.db.QueryRow(
		`SELECT fv.embedding FROM facts_vec fv JOIN facts f ON fv.rowid = f.rowid WHERE f.path = ?`,
		path,
	).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	if len(blob) == 0 {
		return nil, nil
	}
	return bytesToFloat32Slice(blob)
}

// float32SliceToBytes encodes a []float32 as little-endian bytes.
func float32SliceToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// bytesToFloat32Slice decodes little-endian bytes into a []float32.
func bytesToFloat32Slice(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("vec_data length %d is not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

// SetLastCommit stores the last processed commit hash in the meta table.
func (idx *Index) SetLastCommit(hash string) error {
	_, err := idx.db.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('last_commit', ?)`,
		hash,
	)
	return err
}

// GetLastCommit returns the last processed commit hash, or "" if not set.
func (idx *Index) GetLastCommit() (string, error) {
	var hash string
	err := idx.db.QueryRow(`SELECT value FROM meta WHERE key='last_commit'`).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return hash, nil
}

// SearchText queries the FTS5 index and returns matching FactRecords.
func (idx *Index) SearchText(query string, limit int) ([]FactRecord, error) {
	rows, err := idx.db.Query(
		`SELECT f.path, f.title, f.body, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash
		 FROM facts_fts
		 JOIN facts f ON facts_fts.rowid = f.rowid
		 WHERE facts_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		sanitizeFTSQuery(query), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	var results []FactRecord
	for rows.Next() {
		rec, err := scanFactRecordFromRows(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, *rec)
	}
	return results, rows.Err()
}

// ftsResult holds a FactRecord together with its raw BM25 rank from FTS5.
type ftsResult struct {
	rec  FactRecord
	rank float64 // negative (lower = better match)
}

// sanitizeFTSQuery converts a user query string into a safe FTS5 query by quoting
// each whitespace-separated term. This prevents FTS5 syntax errors caused by
// characters that have special meaning in FTS5 query syntax (e.g. hyphens, which
// are interpreted as NOT operators when they precede a token).
//
// Example: "ml-pipeline pytorch" → `"ml-pipeline" "pytorch"`
func sanitizeFTSQuery(q string) string {
	terms := strings.Fields(q)
	if len(terms) == 0 {
		return q
	}
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.ReplaceAll(t, `"`, `""`) // escape embedded double-quotes
		quoted = append(quoted, `"`+t+`"`)
	}
	return strings.Join(quoted, " ")
}

// searchTextWithRanks queries the FTS5 index and returns records with raw BM25 ranks.
func (idx *Index) searchTextWithRanks(query string, limit int) ([]ftsResult, error) {
	rows, err := idx.db.Query(
		`SELECT f.path, f.title, f.body, f.domain, f.entities, f.confidence, f.sources, f.refs, f.commit_hash, rank
		 FROM facts_fts
		 JOIN facts f ON facts_fts.rowid = f.rowid
		 WHERE facts_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		sanitizeFTSQuery(query), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("fts query with ranks: %w", err)
	}
	defer rows.Close()

	var results []ftsResult
	for rows.Next() {
		var rec FactRecord
		var domainJSON, entitiesJSON, refsJSON string
		var rank float64
		err := rows.Scan(
			&rec.Path, &rec.Title, &rec.Body,
			&domainJSON, &entitiesJSON,
			&rec.Confidence, &rec.Sources,
			&refsJSON, &rec.CommitHash,
			&rank,
		)
		if err != nil {
			return nil, fmt.Errorf("scan fts row with rank: %w", err)
		}
		if err := json.Unmarshal([]byte(domainJSON), &rec.Domain); err != nil {
			return nil, fmt.Errorf("unmarshal domain: %w", err)
		}
		if err := json.Unmarshal([]byte(entitiesJSON), &rec.Entities); err != nil {
			return nil, fmt.Errorf("unmarshal entities: %w", err)
		}
		if err := json.Unmarshal([]byte(refsJSON), &rec.Refs); err != nil {
			return nil, fmt.Errorf("unmarshal refs: %w", err)
		}
		results = append(results, ftsResult{rec: rec, rank: rank})
	}
	return results, rows.Err()
}

// SearchQuery describes a hybrid search request.
type SearchQuery struct {
	Text          string
	Entities      []string
	Domain        []string
	Path          string
	MinConfidence float64
	Limit         int
}

// SearchResult is a FactRecord paired with a relevance score in [0, 100].
type SearchResult struct {
	FactRecord
	Score float64 `json:"score"`
}

// dotProduct computes the dot product of two float32 slices.
// For L2-normalised vectors this equals cosine similarity.
func dotProduct(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

// containsAll reports whether haystack contains all elements of needles
// (case-insensitive substring match).
func containsAll(haystack []string, needles []string) bool {
	for _, needle := range needles {
		found := false
		for _, h := range haystack {
			if strings.EqualFold(h, needle) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// matchesFilters reports whether rec satisfies the non-text filter fields in q.
func matchesFilters(rec FactRecord, q SearchQuery) bool {
	if len(q.Entities) > 0 && !containsAll(rec.Entities, q.Entities) {
		return false
	}
	if len(q.Domain) > 0 && !containsAll(rec.Domain, q.Domain) {
		return false
	}
	if q.Path != "" && !strings.HasPrefix(rec.Path, q.Path) {
		return false
	}
	if q.MinConfidence > 0 && rec.Confidence < q.MinConfidence {
		return false
	}
	return true
}

// Search performs a hybrid FTS5 + optional vector search over the index.
//
// Algorithm:
//  1. If Text is present → FTS5 BM25 search; normalise BM25 ranks to [0,1].
//  2. If embedder is available and Text is present → embed query, compute cosine
//     similarity against stored vectors; combined score = 0.6*bm25 + 0.4*cosine.
//  3. Apply Entities / Domain / Path / MinConfidence filters post-retrieval.
//  4. Normalise top-N scores to [0,100]; drop scores < 10.
//  5. Return sorted by score descending, capped at Limit.
//
// If Text is empty, all facts matching the non-text filters are returned with
// score 100.
func (idx *Index) Search(q SearchQuery) ([]SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	// ── Text-less path ────────────────────────────────────────────────────────
	if q.Text == "" {
		rows, err := idx.db.Query(
			`SELECT path, title, body, domain, entities, confidence, sources, refs, commit_hash
			 FROM facts`)
		if err != nil {
			return nil, fmt.Errorf("search: list all: %w", err)
		}
		defer rows.Close()

		var out []SearchResult
		for rows.Next() {
			rec, err := scanFactRecordFromRows(rows)
			if err != nil {
				return nil, err
			}
			if !matchesFilters(*rec, q) {
				continue
			}
			out = append(out, SearchResult{FactRecord: *rec, Score: 100})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(out) > limit {
			out = out[:limit]
		}
		return out, nil
	}

	// ── FTS BM25 path ─────────────────────────────────────────────────────────
	ftsResults, err := idx.searchTextWithRanks(q.Text, limit*5) // over-fetch for filtering
	if err != nil {
		return nil, fmt.Errorf("search: fts: %w", err)
	}
	if len(ftsResults) == 0 {
		return nil, nil
	}

	// Normalise BM25 ranks to [0, 1].
	// FTS5 rank is negative; the most negative value is the best match.
	// minRank is the best (most negative) rank; maxRank is the least negative.
	minRank := ftsResults[0].rank // ORDER BY rank ASC = best first
	maxRank := ftsResults[len(ftsResults)-1].rank
	rankRange := maxRank - minRank
	bm25Scores := make([]float64, len(ftsResults))
	for i, r := range ftsResults {
		if rankRange == 0 {
			bm25Scores[i] = 1.0
		} else {
			// Map [minRank, maxRank] → [1, 0] (best rank → 1).
			bm25Scores[i] = (maxRank - r.rank) / rankRange
		}
	}

	// ── Optional vector augmentation ──────────────────────────────────────────
	var queryVec []float32
	if idx.embedder != nil {
		queryVec, err = idx.embedder.Embed(q.Text)
		if err != nil {
			// Non-fatal: fall back to BM25 only.
			queryVec = nil
		}
	}

	type candidate struct {
		rec   FactRecord
		score float64
	}

	candidates := make([]candidate, 0, len(ftsResults))
	for i, fr := range ftsResults {
		score := bm25Scores[i]
		if queryVec != nil {
			storedVec, err := idx.GetEmbedding(fr.rec.Path)
			if err == nil && len(storedVec) == len(queryVec) {
				cosine := dotProduct(queryVec, storedVec)
				score = 0.6*bm25Scores[i] + 0.4*cosine
			}
		}
		candidates = append(candidates, candidate{rec: fr.rec, score: score})
	}

	// ── Filters ───────────────────────────────────────────────────────────────
	filtered := candidates[:0]
	for _, c := range candidates {
		if matchesFilters(c.rec, q) {
			filtered = append(filtered, c)
		}
	}
	candidates = filtered

	if len(candidates) == 0 {
		return nil, nil
	}

	// ── Sort by score descending ───────────────────────────────────────────────
	// Simple insertion sort is fine for small N; use a basic selection.
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	// ── Normalise to [0, 100] ─────────────────────────────────────────────────
	// All FTS5 matches are considered relevant; no minimum-score cutoff so that
	// short or infrequent terms (e.g. "ml") don't silently drop valid results.
	topScore := candidates[0].score
	var out []SearchResult
	for _, c := range candidates {
		normalised := 100.0
		if topScore > 0 {
			normalised = (c.score / topScore) * 100.0
		}
		out = append(out, SearchResult{FactRecord: c.rec, Score: normalised})
		if len(out) >= limit {
			break
		}
	}

	return out, nil
}

// scanFactRecord scans a single FactRecord from a *sql.Row.
func scanFactRecord(row *sql.Row) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := row.Scan(
		&rec.Path, &rec.Title, &rec.Body,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan fact: %w", err)
	}
	if err := json.Unmarshal([]byte(domainJSON), &rec.Domain); err != nil {
		return nil, fmt.Errorf("unmarshal domain: %w", err)
	}
	if err := json.Unmarshal([]byte(entitiesJSON), &rec.Entities); err != nil {
		return nil, fmt.Errorf("unmarshal entities: %w", err)
	}
	if err := json.Unmarshal([]byte(refsJSON), &rec.Refs); err != nil {
		return nil, fmt.Errorf("unmarshal refs: %w", err)
	}
	return &rec, nil
}

// scanFactRecordFromRows scans a FactRecord from *sql.Rows.
func scanFactRecordFromRows(rows *sql.Rows) (*FactRecord, error) {
	var rec FactRecord
	var domainJSON, entitiesJSON, refsJSON string
	err := rows.Scan(
		&rec.Path, &rec.Title, &rec.Body,
		&domainJSON, &entitiesJSON,
		&rec.Confidence, &rec.Sources,
		&refsJSON, &rec.CommitHash,
	)
	if err != nil {
		return nil, fmt.Errorf("scan fact row: %w", err)
	}
	if err := json.Unmarshal([]byte(domainJSON), &rec.Domain); err != nil {
		return nil, fmt.Errorf("unmarshal domain: %w", err)
	}
	if err := json.Unmarshal([]byte(entitiesJSON), &rec.Entities); err != nil {
		return nil, fmt.Errorf("unmarshal entities: %w", err)
	}
	if err := json.Unmarshal([]byte(refsJSON), &rec.Refs); err != nil {
		return nil, fmt.Errorf("unmarshal refs: %w", err)
	}
	return &rec, nil
}
