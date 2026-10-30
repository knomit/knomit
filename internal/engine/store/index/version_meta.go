package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"knomit/internal/engine/store/internal/vcs"
)

// VersionMeta is what the index holds about ONE version of a fact (its
// (path, blob) row in the content-addressed facts table): everything a
// reference walk needs to decide what to visit and how to title it, and
// nothing of the body.
type VersionMeta struct {
	Blob       string
	Title      string
	Kind       string
	Type       string
	Confidence float64
	// Refs are the fact's refs in the order its frontmatter writes them.
	Refs []string
}

// VersionMetaAt reads the index row of path as it stands in the tree of
// commit, without reading the blob. ok is false when the fast path cannot
// answer: the path is not in that tree under its exact (lowercase) name, or
// the version was never indexed (a historical blob a rebuild did not see).
// A caller that gets ok false reads the blob itself; this method never
// guesses, so a false answer is never a wrong one.
func (fq *FactQuery) VersionMetaAt(ctx context.Context, path, commit string) (VersionMeta, bool, error) {
	if err := fq.probe("VersionMetaAt"); err != nil {
		return VersionMeta{}, false, err
	}
	blob, err := fq.rh.ReadBlobHashAtCommit(ctx, path, commit)
	if err != nil || blob == "" {
		// Not in the tree under this exact name, or the commit is unknown:
		// the caller's blob read has the case-insensitive and
		// before-the-commit fallbacks and the real error text.
		return VersionMeta{}, false, nil
	}
	var (
		m    = VersionMeta{Blob: blob}
		refs string
	)
	err = conn(ctx, fq.rh.DB()).QueryRowContext(ctx,
		`SELECT title, kind, type, confidence, refs FROM facts WHERE path = ? AND blob_hash = ?`,
		path, blob,
	).Scan(&m.Title, &m.Kind, &m.Type, &m.Confidence, &refs)
	if errors.Is(err, sql.ErrNoRows) {
		return VersionMeta{}, false, nil
	}
	if err != nil {
		return VersionMeta{}, false, fmt.Errorf("VersionMetaAt %s@%s: %w", path, commit, err)
	}
	if refs != "" && refs != "null" {
		if err := json.Unmarshal([]byte(refs), &m.Refs); err != nil {
			return VersionMeta{}, false, fmt.Errorf("VersionMetaAt %s@%s: refs: %w", path, commit, err)
		}
	}
	return m, true, nil
}

// PathAtCommit is one (path, commit) pair of VersionMetasAt.
type PathAtCommit = vcs.PathAtCommit

// VersionMetasAt is VersionMetaAt for many (path, commit) pairs at once, with
// the same answer for each: a pair in the map is a hit with exactly the row
// VersionMetaAt returns; a pair absent from it is VersionMetaAt's ok false
// (the caller's blob fallback). It resolves the blobs with one tree pass
// (vcs.Repo.BlobHashesAtCommits: every commit and tree decoded once) and
// reads the rows in one query per chunk, where VersionMetaAt walks the
// trees and queries the row again for every pair.
func (fq *FactQuery) VersionMetasAt(ctx context.Context, pairs []PathAtCommit) (map[PathAtCommit]VersionMeta, error) {
	if err := fq.probe("VersionMetasAt"); err != nil {
		return nil, err
	}
	blobs, err := fq.rh.BlobHashesAtCommits(ctx, pairs)
	if err != nil {
		return nil, fmt.Errorf("VersionMetasAt: %w", err)
	}
	type pathBlob struct{ path, blob string }
	want := make(map[pathBlob][]PathAtCommit, len(blobs))
	keys := make([]pathBlob, 0, len(blobs))
	for p, b := range blobs {
		k := pathBlob{p.Path, b}
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
		want[k] = append(want[k], p)
	}
	out := make(map[PathAtCommit]VersionMeta, len(blobs))
	const chunk = 250
	for lo := 0; lo < len(keys); lo += chunk {
		part := keys[lo:min(len(keys), lo+chunk)]
		args := make([]any, 0, 2*len(part))
		for _, k := range part {
			args = append(args, k.path, k.blob)
		}
		rows, err := conn(ctx, fq.rh.DB()).QueryContext(ctx,
			`SELECT path, blob_hash, title, kind, type, confidence, refs FROM facts
			  WHERE (path, blob_hash) IN (VALUES (?, ?)`+strings.Repeat(", (?, ?)", len(part)-1)+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("VersionMetasAt: %w", err)
		}
		for rows.Next() {
			var k pathBlob
			var m VersionMeta
			var refs string
			if err := rows.Scan(&k.path, &k.blob, &m.Title, &m.Kind, &m.Type, &m.Confidence, &refs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("VersionMetasAt: %w", err)
			}
			m.Blob = k.blob
			if refs != "" && refs != "null" {
				if err := json.Unmarshal([]byte(refs), &m.Refs); err != nil {
					rows.Close()
					return nil, fmt.Errorf("VersionMetasAt %s: refs: %w", k.path, err)
				}
			}
			for _, p := range want[k] {
				if _, dup := out[p]; !dup {
					out[p] = m
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("VersionMetasAt: %w", err)
		}
	}
	return out, nil
}
