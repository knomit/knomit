package store

import (
	"context"
	"testing"
)

// TestExplainFact_Diamond verifies ExplainFact with a diamond-shaped ref graph:
// d → a, d → b, a → c, b → c. Explain each node and verify incoming/outgoing.
func TestExplainFact_Diamond(t *testing.T) {
	ctx := context.Background()
	branch := "agent/explain-diamond"
	svc, idx := openGraphTestStore(t, branch)

	// Write in dependency order: targets first.
	writeAndSync(t, svc, idx, branch, "kb/c.md",
		makeFact("C", []string{"eng"}, []string{"Core"}))
	writeAndSync(t, svc, idx, branch, "kb/a.md",
		makeFact("A", []string{"eng"}, []string{"Go"}, "kb/c.md"))
	writeAndSync(t, svc, idx, branch, "kb/b.md",
		makeFact("B", []string{"eng"}, []string{"Rust"}, "kb/c.md"))
	writeAndSync(t, svc, idx, branch, "kb/d.md",
		makeFact("D", []string{"eng"}, []string{"Top"}, "kb/a.md", "kb/b.md"))

	// C: incoming from a and b, no outgoing.
	res, err := idx.ExplainFact(ctx, branch, "kb/c.md")
	if err != nil {
		t.Fatal(err)
	}
	in := refPaths(res.Incoming)
	if !in["kb/a.md"] || !in["kb/b.md"] || len(in) != 2 {
		t.Errorf("C incoming: want {a, b}, got %v", in)
	}
	if len(res.Outgoing) != 0 {
		t.Errorf("C outgoing: want empty, got %v", res.Outgoing)
	}

	// A: incoming from d, outgoing to c.
	res, err = idx.ExplainFact(ctx, branch, "kb/a.md")
	if err != nil {
		t.Fatal(err)
	}
	in = refPaths(res.Incoming)
	out := refPaths(res.Outgoing)
	if !in["kb/d.md"] || len(in) != 1 {
		t.Errorf("A incoming: want {d}, got %v", in)
	}
	if !out["kb/c.md"] || len(out) != 1 {
		t.Errorf("A outgoing: want {c}, got %v", out)
	}

	// D: no incoming, outgoing to a and b.
	res, err = idx.ExplainFact(ctx, branch, "kb/d.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Incoming) != 0 {
		t.Errorf("D incoming: want empty, got %v", res.Incoming)
	}
	out = refPaths(res.Outgoing)
	if !out["kb/a.md"] || !out["kb/b.md"] || len(out) != 2 {
		t.Errorf("D outgoing: want {a, b}, got %v", out)
	}
}

// TestExplainFact_RefsChangeOnUpdate verifies that when a fact is updated with
// different refs, ExplainFact reflects the current version's outgoing edges,
// and old targets lose their incoming ref.
func TestExplainFact_RefsChangeOnUpdate(t *testing.T) {
	ctx := context.Background()
	branch := "agent/explain-update"
	svc, idx := openGraphTestStore(t, branch)

	// Initial: a, b, c exist. src → a, src → b.
	writeAndSync(t, svc, idx, branch, "kb/a.md",
		makeFact("A", []string{"eng"}, nil))
	writeAndSync(t, svc, idx, branch, "kb/b.md",
		makeFact("B", []string{"eng"}, nil))
	writeAndSync(t, svc, idx, branch, "kb/c.md",
		makeFact("C", []string{"eng"}, nil))
	writeAndSync(t, svc, idx, branch, "kb/src.md",
		makeFact("Source v1", []string{"eng"}, nil, "kb/a.md", "kb/b.md"))

	// Verify initial state.
	res, err := idx.ExplainFact(ctx, branch, "kb/src.md")
	if err != nil {
		t.Fatal(err)
	}
	out := refPaths(res.Outgoing)
	if !out["kb/a.md"] || !out["kb/b.md"] || len(out) != 2 {
		t.Fatalf("v1 outgoing: want {a, b}, got %v", out)
	}

	// A should have src as incoming.
	res, err = idx.ExplainFact(ctx, branch, "kb/a.md")
	if err != nil {
		t.Fatal(err)
	}
	in := refPaths(res.Incoming)
	if !in["kb/src.md"] {
		t.Errorf("v1: A incoming should include src, got %v", in)
	}

	// Update: src → b, src → c (dropped a, added c).
	writeAndSync(t, svc, idx, branch, "kb/src.md",
		makeFact("Source v2", []string{"eng"}, nil, "kb/b.md", "kb/c.md"))

	// src outgoing should now be {b, c}.
	res, err = idx.ExplainFact(ctx, branch, "kb/src.md")
	if err != nil {
		t.Fatal(err)
	}
	out = refPaths(res.Outgoing)
	if !out["kb/b.md"] || !out["kb/c.md"] {
		t.Errorf("v2 outgoing: want {b, c}, got %v", out)
	}
	if out["kb/a.md"] {
		t.Errorf("v2 outgoing: should not contain a, got %v", out)
	}

	// C should now have src as incoming.
	res, err = idx.ExplainFact(ctx, branch, "kb/c.md")
	if err != nil {
		t.Fatal(err)
	}
	in = refPaths(res.Incoming)
	if !in["kb/src.md"] {
		t.Errorf("v2: C incoming should include src, got %v", in)
	}
}

// TestExplainFact_DeletedTargetMarked verifies that deleting a referenced fact
// marks it as Deleted in the outgoing results, and that it disappears from
// the incoming side of the deleted fact.
func TestExplainFact_DeletedTargetMarked(t *testing.T) {
	ctx := context.Background()
	branch := "agent/explain-delete"
	svc, idx := openGraphTestStore(t, branch)

	writeAndSync(t, svc, idx, branch, "kb/target.md",
		makeFact("Target", []string{"eng"}, nil))
	writeAndSync(t, svc, idx, branch, "kb/src.md",
		makeFact("Source", []string{"eng"}, nil, "kb/target.md"))

	// Before delete: outgoing shows target as not deleted.
	res, err := idx.ExplainFact(ctx, branch, "kb/src.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outgoing) != 1 || res.Outgoing[0].Deleted {
		t.Fatalf("before delete: want 1 non-deleted outgoing, got %v", res.Outgoing)
	}

	// Delete target.
	if err := idx.Delete(ctx, branch, "kb/target.md"); err != nil {
		t.Fatal(err)
	}

	// After delete: outgoing should still show target but marked Deleted.
	res, err = idx.ExplainFact(ctx, branch, "kb/src.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outgoing) != 1 {
		t.Fatalf("after delete: want 1 outgoing, got %d", len(res.Outgoing))
	}
	if !res.Outgoing[0].Deleted {
		t.Errorf("after delete: target should be marked Deleted")
	}
	if res.Outgoing[0].Path != "kb/target.md" {
		t.Errorf("after delete: outgoing path = %q, want kb/target.md", res.Outgoing[0].Path)
	}
}

// TestExplainFact_ChainIsDirectOnly verifies that ExplainFact only returns
// direct neighbours, not transitive refs. a → b → c: explain(b) should show
// a as incoming and c as outgoing, but explain(a) should NOT show c.
func TestExplainFact_ChainIsDirectOnly(t *testing.T) {
	ctx := context.Background()
	branch := "agent/explain-chain"
	svc, idx := openGraphTestStore(t, branch)

	writeAndSync(t, svc, idx, branch, "kb/c.md",
		makeFact("C", []string{"eng"}, nil))
	writeAndSync(t, svc, idx, branch, "kb/b.md",
		makeFact("B", []string{"eng"}, nil, "kb/c.md"))
	writeAndSync(t, svc, idx, branch, "kb/a.md",
		makeFact("A", []string{"eng"}, nil, "kb/b.md"))

	// a → b: a's outgoing should be {b}, NOT {b, c}.
	res, err := idx.ExplainFact(ctx, branch, "kb/a.md")
	if err != nil {
		t.Fatal(err)
	}
	out := refPaths(res.Outgoing)
	if !out["kb/b.md"] || len(out) != 1 {
		t.Errorf("a outgoing: want {b} only, got %v", out)
	}

	// b: incoming {a}, outgoing {c}.
	res, err = idx.ExplainFact(ctx, branch, "kb/b.md")
	if err != nil {
		t.Fatal(err)
	}
	in := refPaths(res.Incoming)
	out = refPaths(res.Outgoing)
	if !in["kb/a.md"] || len(in) != 1 {
		t.Errorf("b incoming: want {a}, got %v", in)
	}
	if !out["kb/c.md"] || len(out) != 1 {
		t.Errorf("b outgoing: want {c}, got %v", out)
	}

	// c: incoming {b}, no outgoing.
	res, err = idx.ExplainFact(ctx, branch, "kb/c.md")
	if err != nil {
		t.Fatal(err)
	}
	in = refPaths(res.Incoming)
	if !in["kb/b.md"] || len(in) != 1 {
		t.Errorf("c incoming: want {b} only (not a), got %v", in)
	}
	if len(res.Outgoing) != 0 {
		t.Errorf("c outgoing: want empty, got %v", res.Outgoing)
	}
}

// TestExplainFact_MultipleIncoming verifies that a fact referenced by many
// others shows all of them in incoming.
func TestExplainFact_MultipleIncoming(t *testing.T) {
	ctx := context.Background()
	branch := "agent/explain-multi"
	svc, idx := openGraphTestStore(t, branch)

	writeAndSync(t, svc, idx, branch, "kb/hub.md",
		makeFact("Hub", []string{"eng"}, nil))

	for _, name := range []string{"kb/s1.md", "kb/s2.md", "kb/s3.md", "kb/s4.md"} {
		writeAndSync(t, svc, idx, branch, name,
			makeFact(name, []string{"eng"}, nil, "kb/hub.md"))
	}

	res, err := idx.ExplainFact(ctx, branch, "kb/hub.md")
	if err != nil {
		t.Fatal(err)
	}
	in := refPaths(res.Incoming)
	for _, name := range []string{"kb/s1.md", "kb/s2.md", "kb/s3.md", "kb/s4.md"} {
		if !in[name] {
			t.Errorf("hub incoming: missing %s, got %v", name, in)
		}
	}
	if len(in) != 4 {
		t.Errorf("hub incoming: want 4, got %d", len(in))
	}
}

// TestExplainFact_NoRefs verifies that a fact with no refs has empty
// incoming and outgoing.
func TestExplainFact_NoRefs(t *testing.T) {
	ctx := context.Background()
	branch := "agent/explain-norefs"
	svc, idx := openGraphTestStore(t, branch)

	writeAndSync(t, svc, idx, branch, "kb/lonely.md",
		makeFact("Lonely", []string{"eng"}, nil))

	res, err := idx.ExplainFact(ctx, branch, "kb/lonely.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Incoming) != 0 {
		t.Errorf("incoming: want empty, got %v", res.Incoming)
	}
	if len(res.Outgoing) != 0 {
		t.Errorf("outgoing: want empty, got %v", res.Outgoing)
	}
}

// TestExplainFact_ExternalRefsIgnored verifies that HTTP/HTTPS refs in the
// refs field do NOT produce DERIVED_FROM edges (only local paths do).
func TestExplainFact_ExternalRefsIgnored(t *testing.T) {
	ctx := context.Background()
	branch := "agent/explain-external"
	svc, idx := openGraphTestStore(t, branch)

	writeAndSync(t, svc, idx, branch, "kb/local.md",
		makeFact("Local", []string{"eng"}, nil))
	writeAndSync(t, svc, idx, branch, "kb/src.md",
		makeFact("Source", []string{"eng"}, nil, "kb/local.md", "https://example.com", "http://other.org"))

	res, err := idx.ExplainFact(ctx, branch, "kb/src.md")
	if err != nil {
		t.Fatal(err)
	}
	out := refPaths(res.Outgoing)
	if !out["kb/local.md"] {
		t.Errorf("outgoing should include local ref, got %v", out)
	}
	if len(out) != 1 {
		t.Errorf("outgoing should only have 1 local ref (no HTTP refs), got %v", out)
	}
}

// TestFactVersionHistory_TracksUpdates verifies that FactVersionHistory returns
// all versions of a fact after multiple updates, ordered newest first.
func TestFactVersionHistory_TracksUpdates(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.EnsureBranch(ctx, testBranch, "refs/heads/"+testBranch)

	path := "kb/evolving.md"
	c1 := "aaa0000000000000000000000000000000000000a1"
	c2 := "bbb0000000000000000000000000000000000000b2"
	c3 := "ccc0000000000000000000000000000000000000c3"

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

	if _, err := idx.rebuildGraphHistory(ctx, git, testBranch, nil); err != nil {
		t.Fatal(err)
	}

	versions, err := idx.FactVersionHistory(ctx, testBranch, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(versions))
	}
	// Newest first.
	if versions[0].Title != "V3" {
		t.Errorf("newest version title = %q, want 'V3'", versions[0].Title)
	}
	if versions[2].Title != "V1" {
		t.Errorf("oldest version title = %q, want 'V1'", versions[2].Title)
	}
}

// TestExplainFactAt_OutgoingRefsPerVersion verifies that ExplainFactAt returns
// the outgoing refs that were declared in a specific historical version, not the
// current version.
func TestExplainFactAt_OutgoingRefsPerVersion(t *testing.T) {
	ctx := context.Background()
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	pathA := "kb/a.md"
	pathB := "kb/b.md"
	pathSrc := "kb/src.md"
	cA := "aaa0000000000000000000000000000000000000aa"
	cB := "bbb0000000000000000000000000000000000000bb"
	c1 := "cc10000000000000000000000000000000000000c1"
	c2 := "cc20000000000000000000000000000000000000c2"

	insertCommitLog(t, idx, pathA, cA, 1000, "added")
	insertCommitLog(t, idx, pathB, cB, 2000, "added")
	insertCommitLog(t, idx, pathSrc, c1, 3000, "added")
	insertCommitLog(t, idx, pathSrc, c2, 4000, "modified")

	// Upsert Fact nodes for a and b so DERIVED_FROM can link to them.
	for _, info := range []struct{ path, hash, title string }{
		{pathA, "deadbeef0000000a", "A"},
		{pathB, "deadbeef0000000b", "B"},
	} {
		idx.db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
			info.hash, BlobObjectType, 10, []byte(factContent(info.title)))
		idx.Upsert(ctx, testBranch, "abc", FactRecord{
			Path: info.path, Title: info.title, BlobHash: info.hash,
			Type: "observation", Domain: []string{"test"}, Confidence: 0.8, Sources: 1,
		})
	}

	git := &mockGitReader{
		commitFiles: map[string]map[string]string{
			cA: {pathA: factContent("A")},
			cB: {pathB: factContent("B")},
			c1: {pathSrc: factContent("Src v1", pathA)},  // v1: refs [A]
			c2: {pathSrc: factContent("Src v2", pathB)},  // v2: refs [B]
		},
		files: map[string]string{
			pathA:   factContent("A"),
			pathB:   factContent("B"),
			pathSrc: factContent("Src v2", pathB),
		},
		head: c2,
	}

	if _, err := idx.rebuildGraphHistory(ctx, git, testBranch, nil); err != nil {
		t.Fatal(err)
	}

	// ExplainFactAt v1: outgoing should be {a}.
	res1, err := idx.ExplainFactAt(ctx, testBranch, pathSrc, c1)
	if err != nil {
		t.Fatal(err)
	}
	out1 := refPaths(res1.Outgoing)
	if !out1[pathA] || len(out1) != 1 {
		t.Errorf("v1 outgoing: want {a}, got %v", out1)
	}

	// ExplainFactAt v2: outgoing should be {b}.
	res2, err := idx.ExplainFactAt(ctx, testBranch, pathSrc, c2)
	if err != nil {
		t.Fatal(err)
	}
	out2 := refPaths(res2.Outgoing)
	if !out2[pathB] || len(out2) != 1 {
		t.Errorf("v2 outgoing: want {b}, got %v", out2)
	}
}

// --- helpers ---

func refPaths(refs []RefSummary) map[string]bool {
	m := map[string]bool{}
	for _, r := range refs {
		m[r.Path] = true
	}
	return m
}

