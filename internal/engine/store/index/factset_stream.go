package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"knomit/internal/engine/store/internal/vcs"
)

// PROTOTYPE (proto/factset-streaming): the set-based reads of a streamed
// FactSet. Every read here is ONE SQL statement per chunk over a whole set
// of versions: no git object decode, no per-fact query.

// VersionKey names one version of a fact: its path and blob hash, the key
// of its facts row and of its graph Fact node.
type VersionKey struct{ Path, Blob string }

// StreamEdge is one DERIVED_FROM edge out of a source node, described from
// the target's side: the target version (path, blob) the edge pins, the
// commit that wrote it (target_commit) and that commit's date. Meta is the
// target's facts row when HasRow (only when asked for).
type StreamEdge struct {
	Source int64
	EdgeID int64
	Target int64
	Path   string
	Blob   string
	Commit string
	Date   int64
	HasRow bool
	Meta   VersionMeta
}

// LiveVersion is the version of a path live on a branch (branch_facts):
// the commit that wrote it and its facts row.
type LiveVersion struct {
	Commit string
	Date   int64
	Meta   VersionMeta
}

const streamChunk = 400

func valuesList(n, width int) string {
	one := "(" + strings.TrimSuffix(strings.Repeat("?,", width), ",") + ")"
	return strings.TrimSuffix(strings.Repeat(one+",", n), ",")
}

func decodeRefs(s string) ([]string, error) {
	if s == "" || s == "null" {
		return nil, nil
	}
	var out []string
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}

// VersionsByBlob is the facts row of every (path, blob) that has one; a key
// absent from the map has no row (an orphan historical version: its graph
// node survives a rebuild, its row does not).
func (fq *FactQuery) VersionsByBlob(ctx context.Context, keys []VersionKey) (map[VersionKey]VersionMeta, error) {
	if err := fq.probe("VersionsByBlob"); err != nil {
		return nil, err
	}
	out := make(map[VersionKey]VersionMeta, len(keys))
	for lo := 0; lo < len(keys); lo += streamChunk {
		part := keys[lo:min(len(keys), lo+streamChunk)]
		args := make([]any, 0, 2*len(part))
		for _, k := range part {
			args = append(args, k.Path, k.Blob)
		}
		rows, err := conn(ctx, fq.rh.DB()).QueryContext(ctx,
			`SELECT path, blob_hash, title, kind, type, confidence, refs FROM facts
			  WHERE (path, blob_hash) IN (VALUES `+valuesList(len(part), 2)+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("VersionsByBlob: %w", err)
		}
		for rows.Next() {
			var k VersionKey
			var m VersionMeta
			var refs string
			if err := rows.Scan(&k.Path, &k.Blob, &m.Title, &m.Kind, &m.Type, &m.Confidence, &refs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("VersionsByBlob: %w", err)
			}
			m.Blob = k.Blob
			if m.Refs, err = decodeRefs(refs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("VersionsByBlob %s: refs: %w", k.Path, err)
			}
			out[k] = m
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("VersionsByBlob: %w", err)
		}
	}
	return out, nil
}

// BlobsByHash reads blob objects by hash from the store's object table: one
// primary-key lookup per blob, no tree walk and no commit decode.
func (fq *FactQuery) BlobsByHash(ctx context.Context, hashes []string) (map[string][]byte, error) {
	if err := fq.probe("BlobsByHash"); err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(hashes))
	for lo := 0; lo < len(hashes); lo += streamChunk {
		part := hashes[lo:min(len(hashes), lo+streamChunk)]
		args := make([]any, 0, len(part)+1)
		args = append(args, vcs.BlobObjectType)
		for _, h := range part {
			args = append(args, h)
		}
		rows, err := conn(ctx, fq.rh.DB()).QueryContext(ctx,
			`SELECT hash, data FROM objects WHERE type = ? AND hash IN (?`+strings.Repeat(",?", len(part)-1)+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("BlobsByHash: %w", err)
		}
		for rows.Next() {
			var h string
			var data []byte
			if err := rows.Scan(&h, &data); err != nil {
				rows.Close()
				return nil, fmt.Errorf("BlobsByHash: %w", err)
			}
			out[h] = data
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("BlobsByHash: %w", err)
		}
	}
	return out, nil
}

// LiveVersions is the version of each path live on branch as the index
// last synced it (branch_facts): valid as "the version at a frozen tip"
// only while the tip is the index watermark of that branch.
func (fq *FactQuery) LiveVersions(ctx context.Context, branch string, paths []string) (map[string]LiveVersion, error) {
	if err := fq.probe("LiveVersions"); err != nil {
		return nil, err
	}
	branchID, err := fq.rh.BranchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("LiveVersions: %w", err)
	}
	out := make(map[string]LiveVersion, len(paths))
	for lo := 0; lo < len(paths); lo += streamChunk {
		part := paths[lo:min(len(paths), lo+streamChunk)]
		args := make([]any, 0, len(part)+1)
		args = append(args, branchID)
		for _, p := range part {
			args = append(args, p)
		}
		rows, err := conn(ctx, fq.rh.DB()).QueryContext(ctx,
			`SELECT bf.path, bf.commit_hash, f.blob_hash, f.title, f.kind, f.type, f.confidence, f.refs,
			        COALESCE((SELECT cl.committed_at FROM commit_log cl WHERE cl.commit_hash = bf.commit_hash AND cl.path = bf.path), 0)
			   FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id
			  WHERE bf.branch_id = ? AND bf.path IN (?`+strings.Repeat(",?", len(part)-1)+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("LiveVersions: %w", err)
		}
		for rows.Next() {
			var p, refs string
			var v LiveVersion
			if err := rows.Scan(&p, &v.Commit, &v.Meta.Blob, &v.Meta.Title, &v.Meta.Kind, &v.Meta.Type, &v.Meta.Confidence, &refs, &v.Date); err != nil {
				rows.Close()
				return nil, fmt.Errorf("LiveVersions: %w", err)
			}
			if v.Meta.Refs, err = decodeRefs(refs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("LiveVersions %s: refs: %w", p, err)
			}
			out[p] = v
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("LiveVersions: %w", err)
		}
	}
	return out, nil
}

// NodeIDsByBlob is the graph Fact node id of every (path, blob) that has one.
func (gs *GraphStore) NodeIDsByBlob(ctx context.Context, keys []VersionKey) (map[VersionKey]int64, error) {
	if err := gs.probe("NodeIDsByBlob"); err != nil {
		return nil, err
	}
	out := make(map[VersionKey]int64, len(keys))
	for lo := 0; lo < len(keys); lo += streamChunk {
		part := keys[lo:min(len(keys), lo+streamChunk)]
		args := make([]any, 0, 2*len(part))
		for _, k := range part {
			args = append(args, k.Path, k.Blob)
		}
		// CROSS JOIN pins the join order: the key list first, then the
		// (key_id, value) index. Left to itself the planner may scan.
		rows, err := conn(ctx, gs.rh.DB()).QueryContext(ctx,
			`WITH v(p, b) AS (VALUES `+valuesList(len(part), 2)+`),
			      k AS (SELECT (SELECT id FROM property_keys WHERE key = 'path') AS kp,
			                   (SELECT id FROM property_keys WHERE key = 'blob_hash') AS kb)
			 SELECT v.p, v.b, np.node_id
			   FROM v CROSS JOIN k
			   CROSS JOIN node_props_text np ON np.key_id = k.kp AND np.value = v.p
			   CROSS JOIN node_props_text nb ON nb.node_id = np.node_id AND nb.key_id = k.kb AND nb.value = v.b`, args...)
		if err != nil {
			return nil, fmt.Errorf("NodeIDsByBlob: %w", err)
		}
		for rows.Next() {
			var k VersionKey
			var id int64
			if err := rows.Scan(&k.Path, &k.Blob, &id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("NodeIDsByBlob: %w", err)
			}
			if _, dup := out[k]; !dup {
				out[k] = id
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("NodeIDsByBlob: %w", err)
		}
	}
	return out, nil
}

// DerivedFromOfNodes is every DERIVED_FROM edge out of the given Fact nodes,
// in one set-based join per chunk: the target version (its node, path and
// blob), target_commit, and that commit's date from commit_log, which is
// also OutgoingAtCommit's filter (an edge whose target commit the index does
// not know is dropped: its link would 404). withMeta adds the target's facts
// row. Rows come ordered by source node, then edge id.
func (gs *GraphStore) DerivedFromOfNodes(ctx context.Context, ids []int64, withMeta bool) ([]StreamEdge, error) {
	if err := gs.probe("DerivedFromOfNodes"); err != nil {
		return nil, err
	}
	var out []StreamEdge
	meta, metaJoin := `, 0, '', '', '', 0, ''`, ``
	if withMeta {
		meta = `, f.id IS NOT NULL, COALESCE(f.title, ''), COALESCE(f.kind, ''), COALESCE(f.type, ''), COALESCE(f.confidence, 0), COALESCE(f.refs, '')`
		metaJoin = ` LEFT JOIN facts f ON f.path = j.tpath AND f.blob_hash = j.tblob`
	}
	for lo := 0; lo < len(ids); lo += streamChunk {
		part := ids[lo:min(len(ids), lo+streamChunk)]
		args := make([]any, 0, len(part)+1)
		for _, id := range part {
			args = append(args, id)
		}
		args = append(args, EdgeDerivedFrom)
		rows, err := conn(ctx, gs.rh.DB()).QueryContext(ctx,
			`WITH s(id) AS (VALUES `+valuesList(len(part), 1)+`),
			      k AS (SELECT (SELECT id FROM property_keys WHERE key = 'path') AS kp,
			                   (SELECT id FROM property_keys WHERE key = 'blob_hash') AS kb,
			                   (SELECT id FROM property_keys WHERE key = 'target_commit') AS ktc),
			      j AS (
			 SELECT e.source_id AS src, e.id AS eid, e.target_id AS tid, tp.value AS tpath, tb.value AS tblob, tc.value AS tcommit,
			        (SELECT cl.committed_at FROM commit_log cl WHERE cl.commit_hash = tc.value AND cl.path = tp.value) AS tdate
			   FROM s CROSS JOIN k
			   CROSS JOIN edges e ON e.source_id = s.id AND e.type = ?
			   CROSS JOIN node_props_text tp ON tp.node_id = e.target_id AND tp.key_id = k.kp
			   CROSS JOIN node_props_text tb ON tb.node_id = e.target_id AND tb.key_id = k.kb
			   CROSS JOIN edge_props_text tc ON tc.edge_id = e.id AND tc.key_id = k.ktc)
			 SELECT j.src, j.eid, j.tid, j.tpath, j.tblob, j.tcommit, j.tdate`+meta+`
			   FROM j`+metaJoin+`
			  WHERE j.tdate IS NOT NULL
			  ORDER BY j.src, j.eid`, args...)
		if err != nil {
			return nil, fmt.Errorf("DerivedFromOfNodes: %w", err)
		}
		for rows.Next() {
			var e StreamEdge
			var refs string
			var hasRow bool
			var date sql.NullInt64
			if err := rows.Scan(&e.Source, &e.EdgeID, &e.Target, &e.Path, &e.Blob, &e.Commit, &date,
				&hasRow, &e.Meta.Title, &e.Meta.Kind, &e.Meta.Type, &e.Meta.Confidence, &refs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("DerivedFromOfNodes: %w", err)
			}
			e.Date = date.Int64
			e.HasRow = withMeta && hasRow
			e.Meta.Blob = e.Blob
			if e.HasRow {
				if e.Meta.Refs, err = decodeRefs(refs); err != nil {
					rows.Close()
					return nil, fmt.Errorf("DerivedFromOfNodes %s: refs: %w", e.Path, err)
				}
			}
			out = append(out, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("DerivedFromOfNodes: %w", err)
		}
	}
	return out, nil
}
