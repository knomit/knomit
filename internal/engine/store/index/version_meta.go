package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
