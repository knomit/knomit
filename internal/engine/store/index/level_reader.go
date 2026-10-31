package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"knomit/internal/domain/fact"
)

// LevelReader is a reference walk's reads for ONE call (a FactSet page, seek,
// replay or export; a single-fact plan): readiness checked once, and two
// statements prepared once and reused for every level of every unit:
//
//   - Refs: the ref_edges rows of a SET of citing blobs (the primary-key range
//     scan per blob), the blobs bound as ONE json_each array so the statement
//     is the same for any count;
//   - Versions: the metadata (facts row, parsed from the blob when there is
//     none) and the commit date (commit_log) of a set of target versions, bound
//     the same way.
//
// A walk reads the rows of a level first and asks Versions only for the
// targets it has not seen: a level whose targets are already in the unit (a
// back-reference, a sibling) reads no facts row and no commit_log row at all.
// Measured on the 500x499 synthetic store, joining every row of a level to
// facts and commit_log cost 2.6x the rows alone (1.66 s against 0.63 s for the
// 499,000 rows of a full replay's second levels): the join, not the driver,
// was the cost.
type LevelReader struct {
	gs    *GraphStore
	ctx   context.Context
	db    *sql.DB
	ready bool
	refs  *sql.Stmt
	meta  *sql.Stmt
}

// LevelRow is one ref_edges row as a walk reads it: repo "" for this repo;
// TargetBlob and TargetCommit "" for a cross-KB row.
type LevelRow struct {
	SourceBlob   string
	Ord          int
	Repo         string
	Path         string
	TargetBlob   string
	TargetCommit string
}

// TargetVersion names a target version to read: its path, blob, and the
// commit its date is read at ("" for no date).
type TargetVersion struct{ Path, Blob, Commit string }

// TargetMeta is what Versions read of one target version. HasMeta is false
// only when the blob is in neither facts nor objects; Date is 0 when
// commit_log has no row for (commit, path).
type TargetMeta struct {
	Meta    VersionMeta
	HasMeta bool
	Date    int64
}

const levelRefsSQL = `
	SELECT source_blob, ord, target_repo, target_path, COALESCE(target_blob, ''), COALESCE(target_commit, '')
	  FROM ref_edges
	 WHERE source_blob IN (SELECT value FROM json_each(?))
	 ORDER BY source_blob, ord`

const levelMetaSQL = `
	WITH k(p, b, c) AS (SELECT json_extract(value, '$[0]'), json_extract(value, '$[1]'), json_extract(value, '$[2]') FROM json_each(?))
	SELECT k.p, k.b, k.c, f.id IS NOT NULL, COALESCE(f.title, ''), COALESCE(f.kind, ''), COALESCE(f.type, ''),
	       COALESCE(f.confidence, 0), COALESCE(f.refs, ''), COALESCE(cl.committed_at, 0)
	  FROM k
	  LEFT JOIN facts f ON f.path = k.p AND f.blob_hash = k.b
	  LEFT JOIN commit_log cl ON cl.commit_hash = k.c AND cl.path = k.p`

// NewLevelReader opens the reads of one walk call. Close it when the call
// ends. Before the ref_edges backfill, Refs derives the rows exactly as
// RefEdgesOfBlobs does (the same fallback).
func (gs *GraphStore) NewLevelReader(ctx context.Context) (*LevelReader, error) {
	if err := gs.probe("NewLevelReader"); err != nil {
		return nil, err
	}
	db := gs.rh.DB()
	lr := &LevelReader{gs: gs, ctx: ctx, db: db, ready: refEdgesReady(ctx, db)}
	var err error
	if lr.ready {
		if lr.refs, err = db.PrepareContext(ctx, levelRefsSQL); err != nil {
			return nil, fmt.Errorf("NewLevelReader: refs: %w", err)
		}
	}
	if lr.meta, err = db.PrepareContext(ctx, levelMetaSQL); err != nil {
		lr.Close()
		return nil, fmt.Errorf("NewLevelReader: versions: %w", err)
	}
	return lr, nil
}

// Close releases the prepared statements.
func (lr *LevelReader) Close() {
	if lr.refs != nil {
		_ = lr.refs.Close()
		lr.refs = nil
	}
	if lr.meta != nil {
		_ = lr.meta.Close()
		lr.meta = nil
	}
}

// Refs is the ref_edges rows of blobs, in (source_blob, ord) order. "" and
// duplicate blobs are ignored.
func (lr *LevelReader) Refs(blobs []string) ([]LevelRow, error) {
	keys := uniqueSorted(blobs)
	if len(keys) == 0 {
		return nil, nil
	}
	if !lr.ready {
		rows, err := lr.gs.derivedRefRows(lr.ctx, conn(lr.ctx, lr.db), keys)
		if err != nil {
			return nil, fmt.Errorf("LevelReader.Refs: %w", err)
		}
		out := make([]LevelRow, len(rows))
		for i, r := range rows {
			out[i] = LevelRow{SourceBlob: r.SourceBlob, Ord: r.Ord, Repo: r.Repo, Path: r.Path, TargetBlob: r.TargetBlob, TargetCommit: r.TargetCommit}
		}
		return out, nil
	}
	arg, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	rows, err := lr.refs.QueryContext(lr.ctx, string(arg))
	if err != nil {
		return nil, fmt.Errorf("LevelReader.Refs: %w", err)
	}
	defer rows.Close()
	var out []LevelRow
	// Scanned into reused raw buffers; the source blob is interned per run of
	// rows (they come grouped by it), the other fields copied once.
	var src, repo, path, tblob, tcommit sql.RawBytes
	var ord int
	last := ""
	// Commits and repo ids repeat across rows (a batch of facts shares its
	// commit): one string each per call.
	intern := map[string]string{}
	in := func(b sql.RawBytes) string {
		if len(b) == 0 {
			return ""
		}
		if s, ok := intern[string(b)]; ok {
			return s
		}
		s := string(b)
		intern[s] = s
		return s
	}
	for rows.Next() {
		if err := rows.Scan(&src, &ord, &repo, &path, &tblob, &tcommit); err != nil {
			return nil, fmt.Errorf("LevelReader.Refs: %w", err)
		}
		if string(src) != last {
			last = string(src)
		}
		out = append(out, LevelRow{SourceBlob: last, Ord: ord, Repo: in(repo), Path: string(path), TargetBlob: string(tblob), TargetCommit: in(tcommit)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("LevelReader.Refs: %w", err)
	}
	return out, nil
}

// Versions is the metadata and date of each target version, in one statement
// for the facts rows and dates, plus one batched object read when some
// version has no facts row (parsed reports it: real stores' old pinned
// versions often have none). A key whose blob is in neither is absent.
//
// The answer is keyed by the WHOLE TargetVersion: one blob pinned at two
// commits (a revert to earlier bytes) is two keys with one metadata and two
// dates, so the date served with a commit is always that commit's.
func (lr *LevelReader) Versions(keys []TargetVersion) (out map[TargetVersion]TargetMeta, parsed bool, err error) {
	seen := make(map[TargetVersion]struct{}, len(keys))
	arg := make([][3]string, 0, len(keys))
	for _, k := range keys {
		if k.Blob == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		arg = append(arg, [3]string{k.Path, k.Blob, k.Commit})
	}
	out = make(map[TargetVersion]TargetMeta, len(arg))
	if len(arg) == 0 {
		return out, false, nil
	}
	js, err := json.Marshal(arg)
	if err != nil {
		return nil, false, err
	}
	rows, err := lr.meta.QueryContext(lr.ctx, string(js))
	if err != nil {
		return nil, false, fmt.Errorf("LevelReader.Versions: %w", err)
	}
	var missing []TargetVersion
	dates := map[TargetVersion]int64{}
	var title, kind, typ, refs sql.RawBytes
	// kind and type take a handful of values: one string each per call.
	intern := map[string]string{}
	in := func(b sql.RawBytes) string {
		if s, ok := intern[string(b)]; ok {
			return s
		}
		s := string(b)
		intern[s] = s
		return s
	}
	for rows.Next() {
		var k TargetVersion
		var has bool
		var conf float64
		var date int64
		if err := rows.Scan(&k.Path, &k.Blob, &k.Commit, &has, &title, &kind, &typ, &conf, &refs, &date); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("LevelReader.Versions: %w", err)
		}
		if !has {
			missing = append(missing, k)
			dates[k] = date
			continue
		}
		m := VersionMeta{Blob: k.Blob, Title: string(title), Kind: in(kind), Type: in(typ), Confidence: conf}
		if m.Refs, err = decodeRefsJSON(string(refs)); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("LevelReader.Versions %s: refs: %w", k.Path, err)
		}
		out[k] = TargetMeta{Meta: m, HasMeta: true, Date: date}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, fmt.Errorf("LevelReader.Versions: %w", err)
	}
	if len(missing) == 0 {
		return out, false, nil
	}
	data, err := blobsByHash(lr.ctx, conn(lr.ctx, lr.db), blobsOf(missing))
	if err != nil {
		return nil, true, fmt.Errorf("LevelReader.Versions: %w", err)
	}
	for _, k := range missing {
		raw, ok := data[k.Blob]
		if !ok {
			continue
		}
		f, err := fact.ParseFact(k.Path, string(raw))
		if err != nil {
			return nil, true, fmt.Errorf("LevelReader.Versions: parse %s@%s: %w", k.Path, k.Blob, err)
		}
		out[k] = TargetMeta{Meta: VersionMeta{Blob: k.Blob, Title: f.Title, Kind: string(f.Kind), Type: string(f.Type),
			Confidence: f.Confidence, Refs: f.Refs, Parsed: true}, HasMeta: true, Date: dates[k]}
	}
	return out, true, nil
}

func blobsOf(keys []TargetVersion) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Blob
	}
	return out
}

// Level is one level of a walk in ONE call: Refs for blobs, then Versions for
// the local targets the walk has not seen (known reports the ones it has; a
// nil known reads no metadata at all, for a caller that versions targets
// another way, unpin and cross-KB). Two statements at most, plus the object
// read Versions makes for row-less versions (parsed).
func (lr *LevelReader) Level(blobs []string, known func(path string) bool) (rows []LevelRow, metas map[TargetVersion]TargetMeta, parsed bool, err error) {
	if err := lr.gs.probe("LevelReader.Level"); err != nil {
		return nil, nil, false, err
	}
	if rows, err = lr.Refs(blobs); err != nil {
		return nil, nil, false, err
	}
	if known == nil {
		return rows, nil, false, nil
	}
	var want []TargetVersion
	for _, r := range rows {
		if r.Repo == "" && r.TargetBlob != "" && !known(r.Path) {
			want = append(want, TargetVersion{r.Path, r.TargetBlob, r.TargetCommit})
		}
	}
	metas, parsed, err = lr.Versions(want)
	return rows, metas, parsed, err
}
