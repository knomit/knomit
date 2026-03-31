package store

import (
	"context"
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
	ctx := context.Background()
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

	n, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil)
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
	ctx := context.Background()
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

	if _, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	var edgeCount int
	idx.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE type = 'PREV_VERSION'`).Scan(&edgeCount)
	if edgeCount != 2 {
		t.Errorf("expected 2 PREV_VERSION edges, got %d", edgeCount)
	}
}

func TestRebuildGraphHistory_DerivedFromOnVersions(t *testing.T) {
	ctx := context.Background()
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
	idx.Upsert(ctx, testBranch, "abc", FactRecord{
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

	if _, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	var edgeCount int
	idx.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE type = 'DERIVED_FROM'`).Scan(&edgeCount)
	if edgeCount == 0 {
		t.Error("expected at least one DERIVED_FROM edge from FactVersion")
	}
}

func TestRebuildGraphHistory_SkipsDeletedCommits(t *testing.T) {
	ctx := context.Background()
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

	n, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil)
	if err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}
	// Only c1 (added) should produce a FactVersion; c2 (deleted) is skipped.
	if n != 1 {
		t.Errorf("expected 1 version, got %d", n)
	}
}

func TestFactVersionHistory(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.EnsureBranch(ctx, testBranch, "refs/heads/"+testBranch)

	path := "kb/test/fact.md"
	c1 := "aaa0000000000000000000000000000000000000aa"
	c2 := "bbb0000000000000000000000000000000000000bb"

	insertCommitLog(t, idx, path, c1, 1000, "added")
	insertCommitLog(t, idx, path, c2, 2000, "modified")

	git := &mockGitReader{
		commitFiles: map[string]map[string]string{
			c1: {path: factContent("Version One")},
			c2: {path: factContent("Version Two")},
		},
		files: map[string]string{path: factContent("Version Two")},
		head:  c2,
	}
	if _, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	versions, err := idx.FactVersionHistory(ctx, testBranch, path)
	if err != nil {
		t.Fatalf("FactVersionHistory: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}
	// Newest first.
	if versions[0].CommitHash != c2 {
		t.Errorf("expected newest version %s first, got %s", c2, versions[0].CommitHash)
	}
	if versions[0].Title != "Version Two" {
		t.Errorf("expected title %q, got %q", "Version Two", versions[0].Title)
	}
}

func TestExplainFactAt_OutgoingRefs(t *testing.T) {
	ctx := context.Background()
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

	blobHash := "deadbeef00000002"
	idx.db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
		blobHash, BlobObjectType, 10, []byte(factContent("Target")))
	idx.Upsert(ctx, testBranch, "abc", FactRecord{
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
	if _, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	result, err := idx.ExplainFactAt(ctx, testBranch, source, c2)
	if err != nil {
		t.Fatalf("ExplainFactAt: %v", err)
	}
	if len(result.Outgoing) != 1 || result.Outgoing[0].Path != target {
		t.Errorf("expected outgoing ref to %s, got %v", target, result.Outgoing)
	}
}

func TestExplainFactAt_IncomingRefs(t *testing.T) {
	ctx := context.Background()
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

	blobHash := "deadbeef00000003"
	idx.db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
		blobHash, BlobObjectType, 10, []byte(factContent("Target")))
	idx.Upsert(ctx, testBranch, "abc", FactRecord{
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
	if _, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	result, err := idx.ExplainFactAt(ctx, testBranch, target, c1)
	if err != nil {
		t.Fatalf("ExplainFactAt: %v", err)
	}
	if len(result.Incoming) != 1 || result.Incoming[0].Path != source {
		t.Errorf("expected incoming from %s, got %v", source, result.Incoming)
	}
}

func TestHistoryNavigation_WalkUpAndDown(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.EnsureBranch(ctx, testBranch, "refs/heads/"+testBranch)

	path := "kb/test/fact.md"
	commits := []string{
		"aaa0000000000000000000000000000000000000a1",
		"bbb0000000000000000000000000000000000000b2",
		"ccc0000000000000000000000000000000000000c3",
		"ddd0000000000000000000000000000000000000d4",
		"eee0000000000000000000000000000000000000e5",
		"fff0000000000000000000000000000000000000f6",
	}

	commitFiles := make(map[string]map[string]string)
	for i, c := range commits {
		action := "modified"
		if i == 0 {
			action = "added"
		}
		insertCommitLog(t, idx, path, c, int64((i+1)*1000), action)
		commitFiles[c] = map[string]string{path: factContent(fmt.Sprintf("V%d", i+1))}
	}

	git := &mockGitReader{
		commitFiles: commitFiles,
		files:       map[string]string{path: factContent("V6")},
		head:        commits[5],
	}

	if _, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	// Verify FactVersionHistory returns all 6 versions, newest first.
	versions, err := idx.FactVersionHistory(ctx, testBranch, path)
	if err != nil {
		t.Fatalf("FactVersionHistory: %v", err)
	}
	if len(versions) != 6 {
		t.Fatalf("expected 6 versions, got %d", len(versions))
	}
	for i, want := range []string{commits[5], commits[4], commits[3], commits[2], commits[1], commits[0]} {
		if versions[i].CommitHash != want {
			t.Errorf("versions[%d]: want %s, got %s", i, want[:8], versions[i].CommitHash[:8])
		}
	}

	// Walk backward via PREV_VERSION: newest → oldest.
	prevByHash := func(hash string) string {
		nodeID, err := idx.graphNodeIDByProp(ctx, NodeFactVersion, "commit_hash", hash)
		if err != nil || nodeID == 0 {
			t.Fatalf("node not found for %s", hash[:8])
		}
		var targetID int64
		err = idx.db.QueryRow(
			`SELECT target_id FROM edges WHERE source_id = ? AND type = ?`,
			nodeID, EdgePrevVersion,
		).Scan(&targetID)
		if err != nil {
			return "" // no previous
		}
		var prevHash string
		idx.db.QueryRow(`
			SELECT np.value FROM node_props_text np
			JOIN property_keys pk ON pk.id = np.key_id
			WHERE np.node_id = ? AND pk.key = 'commit_hash'
		`, targetID).Scan(&prevHash)
		return prevHash
	}

	// Walk c6 → c5 → c4 → c3 → c2 → c1 → "".
	wantChain := []string{commits[5], commits[4], commits[3], commits[2], commits[1], commits[0]}
	cur := commits[5]
	for i, want := range wantChain {
		if cur != want {
			t.Errorf("backward walk step %d: want %s, got %s", i, want[:8], cur[:8])
		}
		cur = prevByHash(cur)
	}
	if cur != "" {
		t.Errorf("expected no prev after c1, got %s", cur[:8])
	}

	// Walk forward via reverse PREV_VERSION: oldest → newest.
	nextByHash := func(hash string) string {
		nodeID, err := idx.graphNodeIDByProp(ctx, NodeFactVersion, "commit_hash", hash)
		if err != nil || nodeID == 0 {
			t.Fatalf("node not found for %s", hash[:8])
		}
		var sourceID int64
		err = idx.db.QueryRow(
			`SELECT source_id FROM edges WHERE target_id = ? AND type = ?`,
			nodeID, EdgePrevVersion,
		).Scan(&sourceID)
		if err != nil {
			return "" // no next
		}
		var nextHash string
		idx.db.QueryRow(`
			SELECT np.value FROM node_props_text np
			JOIN property_keys pk ON pk.id = np.key_id
			WHERE np.node_id = ? AND pk.key = 'commit_hash'
		`, sourceID).Scan(&nextHash)
		return nextHash
	}

	// Walk c1 → c2 → c3 → c4 → c5 → c6 → "".
	wantFwd := []string{commits[0], commits[1], commits[2], commits[3], commits[4], commits[5]}
	cur = commits[0]
	for i, want := range wantFwd {
		if cur != want {
			t.Errorf("forward walk step %d: want %s, got %s", i, want[:8], cur[:8])
		}
		cur = nextByHash(cur)
	}
	if cur != "" {
		t.Errorf("expected no next after c6, got %s", cur[:8])
	}
}

func TestExplainFactAt_MultipleRefsInAndOut(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	pathA := "kb/test/a.md"
	pathB := "kb/test/b.md"
	pathC := "kb/test/c.md"
	pathD := "kb/test/d.md"
	pathE := "kb/test/e.md"

	cA := "aaa0000000000000000000000000000000000000aa"
	cB := "bbb0000000000000000000000000000000000000bb"
	cC := "ccc0000000000000000000000000000000000000cc"
	cD := "ddd0000000000000000000000000000000000000dd"
	cE := "eee0000000000000000000000000000000000000ee"

	insertCommitLog(t, idx, pathA, cA, 1000, "added")
	insertCommitLog(t, idx, pathB, cB, 2000, "added")
	insertCommitLog(t, idx, pathC, cC, 3000, "added")
	insertCommitLog(t, idx, pathD, cD, 4000, "added")
	insertCommitLog(t, idx, pathE, cE, 5000, "added")

	// Upsert Fact nodes for a, b, c, d so DERIVED_FROM can link to them.
	for i, p := range []string{pathA, pathB, pathC, pathD} {
		bh := fmt.Sprintf("deadbeef0000000%d", i+10)
		title := []string{"A", "B", "C", "D"}[i]
		idx.db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
			bh, BlobObjectType, 10, []byte(factContent(title)))
		idx.Upsert(ctx, testBranch, "abc", FactRecord{
			Path: p, Title: title, BlobHash: bh,
			Type: "observation", Domain: []string{"test"}, Confidence: 0.8, Sources: 1,
		})
	}

	git := &mockGitReader{
		commitFiles: map[string]map[string]string{
			cA: {pathA: factContent("A")},
			cB: {pathB: factContent("B")},
			cC: {pathC: factContent("C")},
			cD: {pathD: factContent("D", pathA, pathB)},
			cE: {pathE: factContent("E", pathC, pathD)},
		},
		files: map[string]string{
			pathA: factContent("A"),
			pathB: factContent("B"),
			pathC: factContent("C"),
			pathD: factContent("D", pathA, pathB),
			pathE: factContent("E", pathC, pathD),
		},
		head: cE,
	}

	if _, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	containsPath := func(refs []RefSummary, path string) bool {
		for _, r := range refs {
			if r.Path == path {
				return true
			}
		}
		return false
	}

	// D's outgoing refs: A and B.
	resD, err := idx.ExplainFactAt(ctx, testBranch, pathD, cD)
	if err != nil {
		t.Fatalf("ExplainFactAt(D): %v", err)
	}
	if !containsPath(resD.Outgoing, pathA) || !containsPath(resD.Outgoing, pathB) {
		t.Errorf("D outgoing: want [A, B], got %v", resD.Outgoing)
	}
	if len(resD.Outgoing) != 2 {
		t.Errorf("D outgoing: expected 2, got %d", len(resD.Outgoing))
	}

	// E's outgoing refs: C and D.
	resE, err := idx.ExplainFactAt(ctx, testBranch, pathE, cE)
	if err != nil {
		t.Fatalf("ExplainFactAt(E): %v", err)
	}
	if !containsPath(resE.Outgoing, pathC) || !containsPath(resE.Outgoing, pathD) {
		t.Errorf("E outgoing: want [C, D], got %v", resE.Outgoing)
	}

	// A's incoming: D's version references it.
	resA, err := idx.ExplainFactAt(ctx, testBranch, pathA, cA)
	if err != nil {
		t.Fatalf("ExplainFactAt(A): %v", err)
	}
	if !containsPath(resA.Incoming, pathD) {
		t.Errorf("A incoming: want D, got %v", resA.Incoming)
	}

	// B's incoming: D's version references it.
	resB, err := idx.ExplainFactAt(ctx, testBranch, pathB, cB)
	if err != nil {
		t.Fatalf("ExplainFactAt(B): %v", err)
	}
	if !containsPath(resB.Incoming, pathD) {
		t.Errorf("B incoming: want D, got %v", resB.Incoming)
	}

	// C's incoming: E's version references it.
	resC, err := idx.ExplainFactAt(ctx, testBranch, pathC, cC)
	if err != nil {
		t.Fatalf("ExplainFactAt(C): %v", err)
	}
	if !containsPath(resC.Incoming, pathE) {
		t.Errorf("C incoming: want E, got %v", resC.Incoming)
	}

	// D's incoming: E's version references it.
	if !containsPath(resD.Incoming, pathE) {
		t.Errorf("D incoming: want E, got %v", resD.Incoming)
	}
}

func TestExplainFactAt_RefsChangeWithHistory(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	pathA := "kb/test/a.md"
	pathB := "kb/test/b.md"
	pathC := "kb/test/c.md"

	cA := "aaa0000000000000000000000000000000000000aa"
	cC := "ccc0000000000000000000000000000000000000cc"
	// Three versions of b.
	cB1 := "b110000000000000000000000000000000000000b1"
	cB2 := "b220000000000000000000000000000000000000b2"
	cB3 := "b330000000000000000000000000000000000000b3"

	insertCommitLog(t, idx, pathA, cA, 1000, "added")
	insertCommitLog(t, idx, pathC, cC, 2000, "added")
	insertCommitLog(t, idx, pathB, cB1, 3000, "added")
	insertCommitLog(t, idx, pathB, cB2, 4000, "modified")
	insertCommitLog(t, idx, pathB, cB3, 5000, "modified")

	// Upsert Fact nodes for A and C so DERIVED_FROM edges can link to them.
	for i, info := range []struct{ path, title, hash string }{
		{pathA, "A", "deadbeef00000020"},
		{pathC, "C", "deadbeef00000021"},
	} {
		_ = i
		idx.db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
			info.hash, BlobObjectType, 10, []byte(factContent(info.title)))
		idx.Upsert(ctx, testBranch, "abc", FactRecord{
			Path: info.path, Title: info.title, BlobHash: info.hash,
			Type: "observation", Domain: []string{"test"}, Confidence: 0.8, Sources: 1,
		})
	}

	git := &mockGitReader{
		commitFiles: map[string]map[string]string{
			cA:  {pathA: factContent("A")},
			cC:  {pathC: factContent("C")},
			cB1: {pathB: factContent("B-v1", pathA)},        // v1: refs [A]
			cB2: {pathB: factContent("B-v2", pathA, pathC)}, // v2: refs [A, C]
			cB3: {pathB: factContent("B-v3", pathC)},        // v3: refs [C] only
		},
		files: map[string]string{
			pathA: factContent("A"),
			pathC: factContent("C"),
			pathB: factContent("B-v3", pathC),
		},
		head: cB3,
	}

	if _, err := idx.rebuildGraphHistory(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("rebuildGraphHistory: %v", err)
	}

	containsPath := func(refs []RefSummary, path string) bool {
		for _, r := range refs {
			if r.Path == path {
				return true
			}
		}
		return false
	}

	// v1 outgoing: only A.
	resB1, err := idx.ExplainFactAt(ctx, testBranch, pathB, cB1)
	if err != nil {
		t.Fatalf("ExplainFactAt(B,v1): %v", err)
	}
	if !containsPath(resB1.Outgoing, pathA) {
		t.Errorf("B@v1 outgoing: want [A], got %v", resB1.Outgoing)
	}
	if containsPath(resB1.Outgoing, pathC) {
		t.Errorf("B@v1 outgoing: should not contain C, got %v", resB1.Outgoing)
	}

	// v2 outgoing: A and C.
	resB2, err := idx.ExplainFactAt(ctx, testBranch, pathB, cB2)
	if err != nil {
		t.Fatalf("ExplainFactAt(B,v2): %v", err)
	}
	if !containsPath(resB2.Outgoing, pathA) || !containsPath(resB2.Outgoing, pathC) {
		t.Errorf("B@v2 outgoing: want [A, C], got %v", resB2.Outgoing)
	}

	// v3 outgoing: only C.
	resB3, err := idx.ExplainFactAt(ctx, testBranch, pathB, cB3)
	if err != nil {
		t.Fatalf("ExplainFactAt(B,v3): %v", err)
	}
	if !containsPath(resB3.Outgoing, pathC) {
		t.Errorf("B@v3 outgoing: want [C], got %v", resB3.Outgoing)
	}
	if containsPath(resB3.Outgoing, pathA) {
		t.Errorf("B@v3 outgoing: should not contain A, got %v", resB3.Outgoing)
	}

	// Incoming to A: B referenced it in v1 and v2.
	resA, err := idx.ExplainFactAt(ctx, testBranch, pathA, cA)
	if err != nil {
		t.Fatalf("ExplainFactAt(A): %v", err)
	}
	if !containsPath(resA.Incoming, pathB) {
		t.Errorf("A incoming: want B (from v1,v2), got %v", resA.Incoming)
	}

	// Incoming to C: B referenced it in v2 and v3.
	resC, err := idx.ExplainFactAt(ctx, testBranch, pathC, cC)
	if err != nil {
		t.Fatalf("ExplainFactAt(C): %v", err)
	}
	if !containsPath(resC.Incoming, pathB) {
		t.Errorf("C incoming: want B (from v2,v3), got %v", resC.Incoming)
	}
}

func TestRebuild_IncludesHistoryPhase(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	path := "kb/test/fact.md"
	c1 := "aaa0000000000000000000000000000000000000aa"
	c2 := "bbb0000000000000000000000000000000000000bb"

	blobHash := "deadbeef00000004"
	git := &mockGitReader{
		files:      map[string]string{path: factContent("Version Two")},
		blobHashes: map[string]string{path: blobHash},
		commitFiles: map[string]map[string]string{
			c1: {path: factContent("Version One")},
			c2: {path: factContent("Version Two")},
		},
		head: c2,
	}

	// Seed commit_log before rebuild so history phase has entries to process.
	insertCommitLog(t, idx, path, c1, 1000, "added")
	insertCommitLog(t, idx, path, c2, 2000, "modified")

	// Seed objects so Upsert during facts phase can find the blob.
	idx.db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
		blobHash, BlobObjectType, 10, []byte(factContent("Version Two")))

	if err := idx.Rebuild(ctx, git, "machine/test", nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// After Rebuild, FactVersion nodes for both commits should exist.
	versions, err := idx.FactVersionHistory(ctx, "machine/test", path)
	if err != nil {
		t.Fatalf("FactVersionHistory: %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("expected 2 FactVersion nodes after Rebuild, got %d", len(versions))
	}
}
