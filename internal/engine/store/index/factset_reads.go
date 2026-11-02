package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"knomit/internal/domain/fact"
	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/vcs"
)

// The set-based reads of a streamed FactSet (internal/corpus/materialize).
// Every read is ONE statement per chunk over a whole set of keys: no git
// object is decoded and no read is made per fact. Each key is a primary-key
// or existing-index probe (objects PK (hash, type), facts UNIQUE(path,
// blob_hash), branch_facts UNIQUE(branch_id, path), commit_log PK
// (commit_hash, path), ref_edges PK (source_blob, ord)), so no repo schema
// change is needed.

// factsetChunk is how many keys one statement binds (two variables per
// (path, blob) key: well under SQLite's 32766).
var factsetChunk = 4000

// VersionKey names one version of a fact: its repo-relative path and its blob
// hash, the key of its facts row.
type VersionKey struct{ Path, Blob string }

// VersionMeta is what a reference walk needs of ONE version of a fact: its
// title, kind, type, confidence and refs (in the frontmatter's order), never
// its body.
type VersionMeta struct {
	Blob       string
	Title      string
	Kind       string
	Type       string
	Confidence float64
	Refs       []string
	// Parsed: the version has no facts row (a pinned OLD version often has
	// none, kb/gotchas/store/graph/derived-from-order), so this was parsed
	// from its blob in `objects`.
	Parsed bool
}

// CurrentVersion is the version a branch holds for a path now (branch_facts):
// the commit that holds it (recordedCommit) and its blob.
type CurrentVersion struct {
	Commit string
	Blob   string
}

// LiveVersion is the version of a path live at a commit: the commit that
// holds it (recordedCommit), its blob, its metadata (HasMeta false only when
// the blob is in neither facts nor `objects`) and the commit's date (Unix
// seconds; 0 when commit_log has no row).
type LiveVersion struct {
	Commit  string
	Blob    string
	Meta    VersionMeta
	HasMeta bool
	Date    int64
}

// RefTarget is a ref_edges row reduced to what a FactSet's reservation reads:
// the citing blob, the ref's position, and the target (repo "" for this repo;
// TargetBlob "" for a cross-KB row).
type RefTarget struct {
	SourceBlob string
	Ord        int
	Repo       string
	Path       string
	TargetBlob string
}

// PathAtCommit names one version of a path by the commit that wrote it.
type PathAtCommit struct{ Path, Commit string }

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func pairValues(n int) string { return strings.TrimSuffix(strings.Repeat("(?,?),", n), ",") }

func decodeRefsJSON(s string) ([]string, error) {
	if s == "" || s == "null" {
		return nil, nil
	}
	var out []string
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}

// uniqueSorted drops "" and duplicates and sorts.
func uniqueSorted(in []string) []string {
	set := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, dup := set[s]; dup {
			continue
		}
		set[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// VersionsByBlob is the metadata of every (path, blob) in keys: from its facts
// row, or, for a key with no row, parsed from the blob in `objects` (one
// batched object read for all such keys). A key whose blob is not in
// `objects` either is absent from the map: under the real-blob invariant a
// ref_edges row never points at one, so the caller treats it as no version.
func (fq *FactQuery) VersionsByBlob(ctx context.Context, keys []VersionKey) (map[VersionKey]VersionMeta, error) {
	if err := fq.probe("VersionsByBlob"); err != nil {
		return nil, err
	}
	out, err := versionsByBlob(ctx, conn(ctx, fq.rh.DB()), keys)
	if err != nil {
		return nil, fmt.Errorf("VersionsByBlob: %w", err)
	}
	return out, nil
}

func versionsByBlob(ctx context.Context, db storegit.CtxExecer, keys []VersionKey) (map[VersionKey]VersionMeta, error) {
	set := make(map[VersionKey]struct{}, len(keys))
	uniq := make([]VersionKey, 0, len(keys))
	for _, k := range keys {
		if k.Blob == "" {
			continue
		}
		if _, dup := set[k]; dup {
			continue
		}
		set[k] = struct{}{}
		uniq = append(uniq, k)
	}
	out := make(map[VersionKey]VersionMeta, len(uniq))
	chunk := max(factsetChunk/2, 1)
	for lo := 0; lo < len(uniq); lo += chunk {
		part := uniq[lo:min(len(uniq), lo+chunk)]
		args := make([]any, 0, 2*len(part))
		for _, k := range part {
			args = append(args, k.Path, k.Blob)
		}
		rows, err := db.QueryContext(ctx,
			`SELECT path, blob_hash, title, kind, type, confidence, refs FROM facts
			  WHERE (path, blob_hash) IN (VALUES `+pairValues(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k VersionKey
			var m VersionMeta
			var refs string
			if err := rows.Scan(&k.Path, &k.Blob, &m.Title, &m.Kind, &m.Type, &m.Confidence, &refs); err != nil {
				rows.Close()
				return nil, err
			}
			m.Blob = k.Blob
			if m.Refs, err = decodeRefsJSON(refs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%s: refs: %w", k.Path, err)
			}
			out[k] = m
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	var missing []VersionKey
	for _, k := range uniq {
		if _, ok := out[k]; !ok {
			missing = append(missing, k)
		}
	}
	if err := parseMissing(ctx, db, missing, out); err != nil {
		return nil, err
	}
	return out, nil
}

// parseMissing fills out[k] for each key with no facts row from its blob in
// `objects` (one batched object read); a key whose blob is absent stays out.
func parseMissing(ctx context.Context, db storegit.CtxExecer, missing []VersionKey, out map[VersionKey]VersionMeta) error {
	if len(missing) == 0 {
		return nil
	}
	blobs := make([]string, len(missing))
	for i, k := range missing {
		blobs[i] = k.Blob
	}
	data, err := blobsByHash(ctx, db, blobs)
	if err != nil {
		return err
	}
	for _, k := range missing {
		raw, ok := data[k.Blob]
		if !ok {
			continue
		}
		f, err := fact.ParseFact(k.Path, string(raw))
		if err != nil {
			return fmt.Errorf("parse %s@%s: %w", k.Path, k.Blob, err)
		}
		out[k] = VersionMeta{Blob: k.Blob, Title: f.Title, Kind: string(f.Kind), Type: string(f.Type),
			Confidence: f.Confidence, Refs: f.Refs, Parsed: true}
	}
	return nil
}

// BlobsByHash reads blob objects by hash from the store's object table: one
// primary-key probe per blob, batched; no tree walk, no commit decode. A hash
// with no blob object is absent from the map.
func (fq *FactQuery) BlobsByHash(ctx context.Context, hashes []string) (map[string][]byte, error) {
	if err := fq.probe("BlobsByHash"); err != nil {
		return nil, err
	}
	out, err := blobsByHash(ctx, conn(ctx, fq.rh.DB()), hashes)
	if err != nil {
		return nil, fmt.Errorf("BlobsByHash: %w", err)
	}
	return out, nil
}

func blobsByHash(ctx context.Context, db storegit.CtxExecer, hashes []string) (map[string][]byte, error) {
	keys := uniqueSorted(hashes)
	out := make(map[string][]byte, len(keys))
	chunk := max(factsetChunk, 1)
	for lo := 0; lo < len(keys); lo += chunk {
		part := keys[lo:min(len(keys), lo+chunk)]
		args := make([]any, 0, len(part)+1)
		args = append(args, vcs.BlobObjectType)
		for _, h := range part {
			args = append(args, h)
		}
		rows, err := db.QueryContext(ctx,
			`SELECT hash, data FROM objects WHERE type = ? AND hash IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h string
			var data []byte
			if err := rows.Scan(&h, &data); err != nil {
				rows.Close()
				return nil, err
			}
			out[h] = data
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CurrentBlobs is the version branch holds NOW for each of paths
// (branch_facts joined to facts.blob_hash): the comparison FactQuery.CitedVersionState
// makes for one path, batched. A path the branch holds no live version of
// (retracted, never there) is absent from the map.
func (fq *FactQuery) CurrentBlobs(ctx context.Context, branch string, paths []string) (map[string]CurrentVersion, error) {
	if err := fq.probe("CurrentBlobs"); err != nil {
		return nil, err
	}
	out, err := currentBlobs(ctx, fq.rh, branch, paths)
	if err != nil {
		return nil, fmt.Errorf("CurrentBlobs: %w", err)
	}
	return out, nil
}

func currentBlobs(ctx context.Context, rh *vcs.Repo, branch string, paths []string) (map[string]CurrentVersion, error) {
	branchID, err := rh.BranchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("branchID: %w", err)
	}
	keys := uniqueSorted(paths)
	db := conn(ctx, rh.DB())
	out := make(map[string]CurrentVersion, len(keys))
	chunk := max(factsetChunk, 1)
	for lo := 0; lo < len(keys); lo += chunk {
		part := keys[lo:min(len(keys), lo+chunk)]
		args := make([]any, 0, len(part)+1)
		args = append(args, branchID)
		for _, p := range part {
			args = append(args, p)
		}
		rows, err := db.QueryContext(ctx,
			`SELECT bf.path, bf.commit_hash, f.blob_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
			  WHERE bf.branch_id = ? AND bf.path IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			var v CurrentVersion
			if err := rows.Scan(&p, &v.Commit, &v.Blob); err != nil {
				rows.Close()
				return nil, err
			}
			out[p] = v
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// indexWatermark is the commit the index of branch reflects (the sync
// watermark, meta last_commit:<branch>), "" when the branch was never
// indexed. It is Index.SyncWatermark, readable from a query service.
func indexWatermark(ctx context.Context, db storegit.CtxExecer, branch string) (string, error) {
	var hash string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, "last_commit:"+branch).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

// LiveAt is the version of each path live at commit anchor on branch, with
// its metadata and date (LiveVersion). A path with no live version at the
// anchor (never written by then, or deleted on its first-parent line) is
// absent from the map.
//
// Fast path: while the branch's index watermark IS the anchor, ONE statement
// answers for all paths (branch_facts joined to facts and commit_log: the rows
// CurrentBlobs reads). Otherwise the derived history answers per path
// (vcs.LiveVersionAt: path_changes jump pointers plus commit_log deletions),
// then one batched metadata read and one batched date read; still no git
// object is decoded. historical reports which ran, so a caller can tell that a
// replay after the branch moved did not read the branch's present.
func (hq *HistoryQuery) LiveAt(ctx context.Context, branch, anchor string, paths []string) (out map[string]LiveVersion, historical bool, err error) {
	if err := hq.probe("LiveAt"); err != nil {
		return nil, false, err
	}
	db := conn(ctx, hq.rh.DB())
	wm, err := indexWatermark(ctx, db, branch)
	if err != nil {
		return nil, false, fmt.Errorf("LiveAt: watermark: %w", err)
	}
	branchID, err := hq.rh.BranchID(ctx, branch)
	if err != nil {
		return nil, false, fmt.Errorf("LiveAt: branchID: %w", err)
	}
	keys := uniqueSorted(paths)
	out = make(map[string]LiveVersion, len(keys))
	if wm != "" && wm == anchor {
		// Each statement reads the watermark and the in-flight sync marker
		// beside the rows: one statement is one read snapshot, so the rows
		// are branch_facts exactly when that snapshot's watermark is the
		// anchor AND no sync is writing rows past it (Index.Sync rewrites
		// branch_facts file by file before it moves the watermark). Anything
		// else falls back to the derived history rather than serving a
		// version from after the anchor (review N1).
		consistent := true
		chunk := max(factsetChunk, 1)
		for lo := 0; lo < len(keys) && consistent; lo += chunk {
			part := keys[lo:min(len(keys), lo+chunk)]
			args := make([]any, 0, len(part)+4)
			args = append(args, "last_commit:"+branch, "last_commit:"+branch, SyncPendingKey(branch), branchID)
			for _, p := range part {
				args = append(args, p)
			}
			rows, err := db.QueryContext(ctx,
				`SELECT CASE key WHEN ? THEN 1 ELSE 2 END, value, '', '', '', '', '', 0, '', 0
				   FROM meta WHERE key IN (?, ?)
				 UNION ALL
				 SELECT 0, bf.path, bf.commit_hash, f.blob_hash, f.title, f.kind, f.type, f.confidence, f.refs,
				        COALESCE(cl.committed_at, 0)
				   FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
				   LEFT JOIN commit_log cl ON cl.commit_hash = bf.commit_hash AND cl.path = bf.path
				  WHERE bf.branch_id = ? AND bf.path IN (`+placeholders(len(part))+`)`, args...)
			if err != nil {
				return nil, false, fmt.Errorf("LiveAt: %w", err)
			}
			sawMark := false
			for rows.Next() {
				var mark int
				var p, refs string
				var v LiveVersion
				if err := rows.Scan(&mark, &p, &v.Commit, &v.Blob, &v.Meta.Title, &v.Meta.Kind, &v.Meta.Type, &v.Meta.Confidence, &refs, &v.Date); err != nil {
					rows.Close()
					return nil, false, fmt.Errorf("LiveAt: %w", err)
				}
				if mark != 0 {
					// 1: the watermark, which must be the anchor. 2: a
					// sync's marker, harmless only once the watermark has
					// reached the head it names.
					sawMark = sawMark || mark == 1
					if p != anchor {
						consistent = false
					}
					continue
				}
				v.Meta.Blob, v.HasMeta = v.Blob, true
				if v.Meta.Refs, err = decodeRefsJSON(refs); err != nil {
					rows.Close()
					return nil, false, fmt.Errorf("LiveAt %s: refs: %w", p, err)
				}
				out[p] = v
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, false, fmt.Errorf("LiveAt: %w", err)
			}
			if !sawMark {
				consistent = false
			}
		}
		if consistent {
			return out, false, nil
		}
		out = make(map[string]LiveVersion, len(keys))
	}
	var vkeys []VersionKey
	var pairs []PathAtCommit
	for _, p := range keys {
		row, blob, live, err := vcs.LiveVersionAt(ctx, db, vcs.ActiveTables, p, anchor, branchID)
		if err != nil {
			return nil, true, fmt.Errorf("LiveAt %s: %w", p, err)
		}
		if live && blob != "" {
			out[p] = LiveVersion{Commit: row, Blob: blob}
			vkeys = append(vkeys, VersionKey{p, blob})
			pairs = append(pairs, PathAtCommit{p, row})
		}
	}
	metas, err := versionsByBlob(ctx, db, vkeys)
	if err != nil {
		return nil, true, fmt.Errorf("LiveAt: %w", err)
	}
	dates, err := commitDates(ctx, db, pairs)
	if err != nil {
		return nil, true, fmt.Errorf("LiveAt: %w", err)
	}
	for p, v := range out {
		v.Meta, v.HasMeta = metas[VersionKey{p, v.Blob}]
		v.Date = dates[PathAtCommit{p, v.Commit}]
		out[p] = v
	}
	return out, true, nil
}

// CommitDates is the commit date (Unix seconds, commit_log.committed_at) of
// each (path, commit), one primary-key probe each, batched. A pair with no
// commit_log row is absent from the map.
func (hq *HistoryQuery) CommitDates(ctx context.Context, pairs []PathAtCommit) (map[PathAtCommit]int64, error) {
	if err := hq.probe("CommitDates"); err != nil {
		return nil, err
	}
	out, err := commitDates(ctx, conn(ctx, hq.rh.DB()), pairs)
	if err != nil {
		return nil, fmt.Errorf("CommitDates: %w", err)
	}
	return out, nil
}

func commitDates(ctx context.Context, db storegit.CtxExecer, pairs []PathAtCommit) (map[PathAtCommit]int64, error) {
	set := make(map[PathAtCommit]struct{}, len(pairs))
	uniq := make([]PathAtCommit, 0, len(pairs))
	for _, p := range pairs {
		if p.Commit == "" {
			continue
		}
		if _, dup := set[p]; dup {
			continue
		}
		set[p] = struct{}{}
		uniq = append(uniq, p)
	}
	out := make(map[PathAtCommit]int64, len(uniq))
	chunk := max(factsetChunk/2, 1)
	for lo := 0; lo < len(uniq); lo += chunk {
		part := uniq[lo:min(len(uniq), lo+chunk)]
		args := make([]any, 0, 2*len(part))
		for _, p := range part {
			args = append(args, p.Commit, p.Path)
		}
		rows, err := db.QueryContext(ctx,
			`SELECT commit_hash, path, committed_at FROM commit_log
			  WHERE (commit_hash, path) IN (VALUES `+pairValues(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k PathAtCommit
			var at int64
			if err := rows.Scan(&k.Commit, &k.Path, &at); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = at
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Watermark is the commit the index of branch reflects (meta
// last_commit:<branch>; "" when the branch was never indexed): the anchor a
// FactSet freezes, because branch_facts describes exactly that commit.
func (hq *HistoryQuery) Watermark(ctx context.Context, branch string) (string, error) {
	if err := hq.probe("Watermark"); err != nil {
		return "", err
	}
	wm, err := indexWatermark(ctx, conn(ctx, hq.rh.DB()), branch)
	if err != nil {
		return "", fmt.Errorf("Watermark: %w", err)
	}
	return wm, nil
}

// SettledWatermark is Watermark plus whether the branch's derived rows are AT
// it: false while an Index.Sync is rewriting branch_facts past it (its
// sync_pending marker names another head), when a read of the rows can see
// versions from after the watermark, and false while the watermark is empty
// (never synced, or a Rebuild rewriting every row). generation is the
// branch's rebuild generation (RebuildGenKey), bumped by every Rebuild: a
// whole Rebuild at the same head between two reads leaves the watermark
// equal but the generation not (review R3-2). All three come from one
// statement, so one snapshot. A FactSet's anchored search needs settled on
// both sides of the search and both values unchanged.
func (hq *HistoryQuery) SettledWatermark(ctx context.Context, branch string) (wm, generation string, settled bool, err error) {
	if err := hq.probe("SettledWatermark"); err != nil {
		return "", "", false, err
	}
	rows, err := conn(ctx, hq.rh.DB()).QueryContext(ctx,
		`SELECT key, value FROM meta WHERE key IN (?, ?, ?)`, "last_commit:"+branch, SyncPendingKey(branch), RebuildGenKey(branch))
	if err != nil {
		return "", "", false, fmt.Errorf("SettledWatermark: %w", err)
	}
	defer rows.Close()
	var pending string
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return "", "", false, fmt.Errorf("SettledWatermark: %w", err)
		}
		switch k {
		case SyncPendingKey(branch):
			pending = v
		case RebuildGenKey(branch):
			generation = v
		default:
			wm = v
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", false, fmt.Errorf("SettledWatermark: %w", err)
	}
	// An empty watermark is a branch never synced or a Rebuild in progress
	// (it sets "" before rewriting every row): no rows to anchor on.
	return wm, generation, wm != "" && (pending == "" || pending == wm), nil
}

// SettledOnce reports whether branch's index watermark has EVER been set to a
// commit (SettledOnceKey present), or is set now (a store indexed before the
// key existed reads as settled through its watermark). False for a branch
// whose first build is in flight or failed, whose index was wiped, or that
// does not exist. A branch that was settled once and is now syncing or
// rebuilding stays true. One statement.
func (hq *HistoryQuery) SettledOnce(ctx context.Context, branch string) (bool, error) {
	if err := hq.probe("SettledOnce"); err != nil {
		return false, err
	}
	var n int
	if err := conn(ctx, hq.rh.DB()).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM meta WHERE (key = ? AND value != '') OR (key = ? AND value != '')`,
		SettledOnceKey(branch), "last_commit:"+branch).Scan(&n); err != nil {
		return false, fmt.Errorf("SettledOnce: %w", err)
	}
	return n > 0, nil
}

// ErrRefEdgesNotReady: ref_edges is not backfilled yet (the reads derive rows
// from the edges) AND this repo's id is unresolved, so a kb://<self>/ ref can
// only be told from a foreign one by learning it from the data. A FactSet
// refuses to plan in that state (retry-able), rather than freeze a structure
// that the backfill would change.
var ErrRefEdgesNotReady = errors.New("reference index not ready")

// RefEdgesReadiness is ErrRefEdgesNotReady or nil: whether a reference walk
// may plan on this store now. Ready when the backfill sentinel is set, or,
// before it, when this repo's id resolves (the fallback then derives exactly
// the rows the backfill will write).
func (gs *GraphStore) RefEdgesReadiness(ctx context.Context) error {
	if err := gs.probe("RefEdgesReadiness"); err != nil {
		return err
	}
	db := conn(ctx, gs.rh.DB())
	if refEdgesReady(ctx, db) {
		return nil
	}
	if gs.repoIDOf(ctx, db, "") == "" {
		return ErrRefEdgesNotReady
	}
	return nil
}

// RefTargetsOfBlobs is RefEdgesOfBlobs reduced to what a FactSet's
// reservation reads (no target_commit): every row of a SET of citing blobs, in
// (source_blob, ord) order, one covering range scan of the primary key per
// blob, chunked. Before the backfill it derives the rows exactly as
// RefEdgesOfBlobs does (the same fallback).
func (gs *GraphStore) RefTargetsOfBlobs(ctx context.Context, blobs []string) ([]RefTarget, error) {
	if err := gs.probe("RefTargetsOfBlobs"); err != nil {
		return nil, err
	}
	keys := uniqueSorted(blobs)
	db := conn(ctx, gs.rh.DB())
	if !refEdgesReady(ctx, db) {
		rows, err := gs.derivedRefRows(ctx, db, keys)
		if err != nil {
			return nil, fmt.Errorf("RefTargetsOfBlobs: %w", err)
		}
		out := make([]RefTarget, len(rows))
		for i, r := range rows {
			out[i] = RefTarget{SourceBlob: r.SourceBlob, Ord: r.Ord, Repo: r.Repo, Path: r.Path, TargetBlob: r.TargetBlob}
		}
		return out, nil
	}
	chunk := max(refEdgesChunk, 1)
	var out []RefTarget
	for lo := 0; lo < len(keys); lo += chunk {
		part := keys[lo:min(len(keys), lo+chunk)]
		args := make([]any, len(part))
		for i, b := range part {
			args[i] = b
		}
		rows, err := db.QueryContext(ctx, `
			SELECT source_blob, ord, target_repo, target_path, COALESCE(target_blob, '')
			  FROM ref_edges
			 WHERE source_blob IN (`+placeholders(len(part))+`)
			 ORDER BY source_blob, ord`, args...)
		if err != nil {
			return nil, fmt.Errorf("RefTargetsOfBlobs: %w", err)
		}
		for rows.Next() {
			var r RefTarget
			if err := rows.Scan(&r.SourceBlob, &r.Ord, &r.Repo, &r.Path, &r.TargetBlob); err != nil {
				rows.Close()
				return nil, fmt.Errorf("RefTargetsOfBlobs: %w", err)
			}
			out = append(out, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("RefTargetsOfBlobs: %w", err)
		}
	}
	return out, nil
}
