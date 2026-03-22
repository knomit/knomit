package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"knomit/internal/git"
)

// setupRebuildStore creates a Service with a git store and some facts,
// syncs the index, then returns everything needed for rebuild tests.
func setupRebuildStore(t *testing.T, facts map[string]string) (*Service, *git.Store) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	svc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })

	gs, err := git.InitWithStorer(svc.GitStorer(), nil, "agent/test")
	if err != nil {
		t.Fatal(err)
	}

	for path, content := range facts {
		if _, _, err := gs.WriteFile(path, content, "add "+path, "learn"); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	if err := svc.Index().Sync(gs, "agent/test"); err != nil {
		t.Fatalf("initial Sync: %v", err)
	}

	return svc, gs
}

func countFacts(t *testing.T, svc *Service) int {
	t.Helper()
	var n int
	if err := svc.DB().QueryRow("SELECT COUNT(*) FROM facts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRebuildFacts_BulkInsert(t *testing.T) {
	facts := map[string]string{
		"kb/alpha.md": "---\ntype: observation\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: [knomit]\nrefs: []\n---\n# Alpha Fact\n\nAlpha body content.\n",
		"kb/beta.md":  "---\ntype: observation\ndomain: [dev]\nconfidence: 0.8\nsources: 2\nentities: [golang]\nrefs: []\n---\n# Beta Fact\n\nBeta body content.\n",
	}

	svc, gs := setupRebuildStore(t, facts)
	idx := svc.Index()

	// Verify facts exist after initial sync.
	if n := countFacts(t, svc); n != 2 {
		t.Fatalf("expected 2 facts after sync, got %d", n)
	}

	// Clear the facts table.
	if _, err := svc.DB().Exec("DELETE FROM facts"); err != nil {
		t.Fatal(err)
	}
	if n := countFacts(t, svc); n != 0 {
		t.Fatalf("expected 0 facts after DELETE, got %d", n)
	}

	// Rebuild should repopulate.
	if err := idx.Rebuild(gs, "agent/test", nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if n := countFacts(t, svc); n != 2 {
		t.Fatalf("expected 2 facts after rebuild, got %d", n)
	}

	// Verify individual facts.
	rec, err := idx.GetByPath("kb/alpha.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected alpha fact after rebuild")
	}
	if rec.Title != "Alpha Fact" {
		t.Fatalf("expected title 'Alpha Fact', got %q", rec.Title)
	}
	if rec.Confidence != 0.9 {
		t.Fatalf("expected confidence 0.9, got %v", rec.Confidence)
	}

	rec, err = idx.GetByPath("kb/beta.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected beta fact after rebuild")
	}
	if rec.Title != "Beta Fact" {
		t.Fatalf("expected title 'Beta Fact', got %q", rec.Title)
	}
	if rec.Sources != 2 {
		t.Fatalf("expected sources 2, got %d", rec.Sources)
	}
}

func TestRebuildFacts_SkipsNonFacts(t *testing.T) {
	// kb.md (manifest) and ontology.yaml are non-fact files.
	// Only valid fact files should be indexed.
	facts := map[string]string{
		"kb/real-fact.md": "---\ntype: observation\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: [knomit]\nrefs: []\n---\n# Real Fact\n\nThis is a real fact.\n",
		"ontology.yaml":   "domains:\n  - testing\n  - dev\n",
		"kb/no-title.md":  "---\ntype: observation\ndomain: [testing]\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\nNo heading here, just text.\n",
	}

	svc, gs := setupRebuildStore(t, facts)
	idx := svc.Index()

	// Clear and rebuild.
	if _, err := svc.DB().Exec("DELETE FROM facts"); err != nil {
		t.Fatal(err)
	}

	if err := idx.Rebuild(gs, "agent/test", nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Only the real fact should be indexed.
	if n := countFacts(t, svc); n != 1 {
		t.Fatalf("expected 1 fact (only real-fact.md), got %d", n)
	}

	rec, err := idx.GetByPath("kb/real-fact.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected real-fact.md to be indexed")
	}

	// Non-facts should not appear.
	rec, err = idx.GetByPath("ontology.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("ontology.yaml should not be indexed as a fact")
	}

	rec, err = idx.GetByPath("kb/no-title.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("kb/no-title.md (no heading) should not be indexed as a fact")
	}
}

func TestRebuildFacts_CommitLogJoin(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	svc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	gs, err := git.InitWithStorer(svc.GitStorer(), nil, "agent/test")
	if err != nil {
		t.Fatal(err)
	}

	factV1 := "---\ntype: observation\ndomain: [testing]\nconfidence: 0.7\nsources: 1\nentities: [knomit]\nrefs: []\n---\n# Evolving Fact\n\nVersion 1.\n"
	factV2 := "---\ntype: observation\ndomain: [testing]\nconfidence: 0.9\nsources: 2\nentities: [knomit]\nrefs: []\n---\n# Evolving Fact\n\nVersion 2 with updates.\n"

	// Write v1.
	commitV1, _, err := gs.WriteFile("kb/evolving.md", factV1, "add evolving v1", "learn")
	if err != nil {
		t.Fatal(err)
	}

	// Write v2 to same path — creates a second commit touching this file.
	commitV2, _, err := gs.WriteFile("kb/evolving.md", factV2, "update evolving v2", "learn")
	if err != nil {
		t.Fatal(err)
	}

	if commitV1 == commitV2 {
		t.Fatal("expected two distinct commits")
	}

	// Sync to populate index and commit_log.
	if err := svc.Index().Sync(gs, "agent/test"); err != nil {
		t.Fatal(err)
	}

	// Clear facts and rebuild.
	if _, err := svc.DB().Exec("DELETE FROM facts"); err != nil {
		t.Fatal(err)
	}

	if err := svc.Index().Rebuild(gs, "agent/test", nil); err != nil {
		t.Fatal(err)
	}

	rec, err := svc.Index().GetByPath("kb/evolving.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected evolving fact after rebuild")
	}

	// The commit_hash should come from commit_log (one of the two commits
	// that touched this file), not an empty HEAD fallback.
	if rec.CommitHash == "" {
		t.Fatal("expected non-empty commit_hash from commit_log join")
	}
	if rec.CommitHash != commitV1 && rec.CommitHash != commitV2 {
		t.Fatalf("commit_hash=%q does not match either v1=%q or v2=%q", rec.CommitHash, commitV1, commitV2)
	}
}

func TestRebuild_PhaseTiming(t *testing.T) {
	facts := map[string]string{
		"kb/timing.md": "---\ntype: observation\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: [knomit]\nrefs: []\n---\n# Timing Test\n\nBody.\n",
	}

	svc, gs := setupRebuildStore(t, facts)
	idx := svc.Index()

	// Clear facts for rebuild.
	if _, err := svc.DB().Exec("DELETE FROM facts"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	phases := make(map[string]bool)
	progress := func(phase string, done, total int) {
		mu.Lock()
		defer mu.Unlock()
		phases[phase] = true
	}

	if err := idx.Rebuild(gs, "agent/test", progress); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// "facts" and "graph" always report progress. "embeddings" only reports
	// when an embedder is configured (none in this test).
	for _, expected := range []string{"facts", "graph"} {
		if !phases[expected] {
			t.Errorf("expected phase %q to be reported, got phases: %v", expected, phases)
		}
	}
}

func BenchmarkRebuild(b *testing.B) {
	dir := b.TempDir()
	dbPath := filepath.Join(dir, "bench.db")
	svc, err := Open(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer svc.Close()

	gs, err := git.InitWithStorer(svc.GitStorer(), nil, "agent/test")
	if err != nil {
		b.Fatal(err)
	}

	// Write 50 facts.
	for i := 0; i < 50; i++ {
		content := fmt.Sprintf("---\ntype: observation\ndomain: [bench]\nconfidence: 0.9\nsources: 1\nentities: [item%d]\nrefs: []\n---\n# Benchmark Fact %d\n\nBody content for fact number %d.\n", i, i, i)
		if _, _, err := gs.WriteFile(fmt.Sprintf("kb/bench/fact-%03d.md", i), content, fmt.Sprintf("add fact %d", i), "learn"); err != nil {
			b.Fatal(err)
		}
	}

	// Initial sync to populate commit_log and objects.
	if err := svc.Index().Sync(gs, "agent/test"); err != nil {
		b.Fatal(err)
	}

	db := svc.DB()
	idx := svc.Index()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Exec("DELETE FROM facts")
		db.Exec("DELETE FROM facts_vec")
		if err := idx.Rebuild(gs, "agent/test", nil); err != nil {
			b.Fatal(err)
		}
	}
}
