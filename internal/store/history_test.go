package store

import (
	"fmt"
	"testing"
)

// factContent returns a minimal valid fact markdown with optional refs.
func factContent(title string, refs ...string) string {
	refsYAML := "refs: []"
	if len(refs) > 0 {
		refsYAML = "refs:\n"
		for _, r := range refs {
			refsYAML += fmt.Sprintf("  - %s\n", r)
		}
	}
	return fmt.Sprintf("---\ndomain: [test]\nentities: [TestEntity]\nconfidence: 0.8\nsources: 1\n%s\n---\n# %s\n\nBody.\n", refsYAML, title)
}

// insertCommitLog inserts rows into commit_log for testing.
func insertCommitLog(t *testing.T, idx *Index, path, commitHash string, committedAt int64, action string) {
	t.Helper()
	_, err := idx.db.Exec(
		`INSERT OR IGNORE INTO commit_log(commit_hash, path, committed_at, message, operation, author_email, action) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		commitHash, path, committedAt, "test commit", "update", "test@example.com", action,
	)
	if err != nil {
		t.Fatalf("insertCommitLog: %v", err)
	}
}

func TestRebuildGraphHistory_CreatesFactVersionNodes(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	path := "kb/test/fact.md"
	c1, c2 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	insertCommitLog(t, idx, path, c1, 1000, "added")
	insertCommitLog(t, idx, path, c2, 2000, "modified")

	git := &mockGitReader{
		files: map[string]string{path: factContent("Version Two")},
		commitFiles: map[string]map[string]string{
			c1: {path: factContent("Version One")},
			c2: {path: factContent("Version Two")},
		},
		head: c2,
	}

	n, err := idx.rebuildGraphHistory(git, "machine/test", nil)
	if err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 versions processed, got %d", n)
	}

	// Both FactVersion nodes must exist.
	pj := jsonParams("path", path)
	rows, err := idx.db.Query(
		`SELECT json_extract(value, '$.commit_hash') FROM json_each(cypher('MATCH (v:FactVersion {path: $path}) RETURN v.commit_hash AS commit_hash', ?))`,
		pj,
	)
	if err != nil {
		t.Fatalf("query FactVersion: %v", err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		rows.Scan(&h)
		hashes = append(hashes, h)
	}
	rows.Close()
	if len(hashes) != 2 {
		t.Errorf("expected 2 FactVersion nodes, got %d: %v", len(hashes), hashes)
	}
}

func TestRebuildGraphHistory_PrevVersionChain(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	path := "kb/test/fact.md"
	c1 := "aaa0000000000000000000000000000000000000aa"
	c2 := "bbb0000000000000000000000000000000000000bb"
	c3 := "ccc0000000000000000000000000000000000000cc"

	insertCommitLog(t, idx, path, c1, 1000, "added")
	insertCommitLog(t, idx, path, c2, 2000, "modified")
	insertCommitLog(t, idx, path, c3, 3000, "modified")

	git := &mockGitReader{
		commitFiles: map[string]map[string]string{
			c1: {path: factContent("V1")},
			c2: {path: factContent("V2")},
			c3: {path: factContent("V3")},
		},
		files: map[string]string{path: factContent("V3")},
		head:  c3,
	}

	if _, err := idx.rebuildGraphHistory(git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	var edgeCount int
	idx.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE type = 'PREV_VERSION'`).Scan(&edgeCount)
	if edgeCount != 2 {
		t.Errorf("expected 2 PREV_VERSION edges, got %d", edgeCount)
	}
}

func TestRebuildGraphHistory_DerivedFromOnVersions(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	source := "kb/test/source.md"
	target := "kb/test/target.md"
	c1 := "aaa0000000000000000000000000000000000000aa"
	c2 := "bbb0000000000000000000000000000000000000bb"

	insertCommitLog(t, idx, target, c1, 1000, "added")
	insertCommitLog(t, idx, source, c2, 2000, "added")

	// target must exist as a Fact node for DERIVED_FROM to link to it.
	blobHash := "deadbeef00000001"
	idx.db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
		blobHash, BlobObjectType, 10, []byte(factContent("Target")))
	idx.Upsert(FactRecord{
		Path: target, Title: "Target", BlobHash: blobHash,
		Type: "observation", Domain: []string{"test"}, Confidence: 0.8, Sources: 1,
	})

	git := &mockGitReader{
		commitFiles: map[string]map[string]string{
			c1: {target: factContent("Target")},
			c2: {source: factContent("Source", target)},
		},
		files: map[string]string{
			target: factContent("Target"),
			source: factContent("Source", target),
		},
		head: c2,
	}

	if _, err := idx.rebuildGraphHistory(git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	var edgeCount int
	idx.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE type = 'DERIVED_FROM'`).Scan(&edgeCount)
	if edgeCount == 0 {
		t.Error("expected at least one DERIVED_FROM edge from FactVersion")
	}
}

func TestRebuildGraphHistory_SkipsDeletedCommits(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	path := "kb/test/fact.md"
	c1 := "aaa0000000000000000000000000000000000000aa"
	c2 := "bbb0000000000000000000000000000000000000bb"

	insertCommitLog(t, idx, path, c1, 1000, "added")
	insertCommitLog(t, idx, path, c2, 2000, "deleted")

	git := &mockGitReader{
		commitFiles: map[string]map[string]string{
			c1: {path: factContent("V1")},
			// c2 has no file — it was deleted
		},
		files: map[string]string{},
		head:  c2,
	}

	n, err := idx.rebuildGraphHistory(git, "machine/test", nil)
	if err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}
	// Only c1 (added) should produce a FactVersion; c2 (deleted) is skipped.
	if n != 1 {
		t.Errorf("expected 1 version, got %d", n)
	}
}
