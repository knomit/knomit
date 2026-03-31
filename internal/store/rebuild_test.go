package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"knomit/internal/git"
)

// ── Stub embedders ─────────────────────────────────────────────────────────

const vecDim768 = 768

func make768vec() []float32 {
	v := make([]float32, vecDim768)
	v[0] = 1
	return v
}

// countingBatchEmbedder implements BatchEmbedder and records per-call batch sizes.
type countingBatchEmbedder struct {
	batchSizes []int
}

func (e *countingBatchEmbedder) Embed(text string) ([]float32, error) {
	e.batchSizes = append(e.batchSizes, 1)
	return make768vec(), nil
}

func (e *countingBatchEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	e.batchSizes = append(e.batchSizes, len(texts))
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make768vec()
	}
	return out, nil
}

// countingSingleEmbedder implements only Embed (no EmbedBatch), exercising the
// non-batch code path. errOnCall, if > 0, makes the Nth call return an error.
type countingSingleEmbedder struct {
	calls    int
	errOnCall int
}

func (e *countingSingleEmbedder) Embed(_ string) ([]float32, error) {
	e.calls++
	if e.errOnCall > 0 && e.calls == e.errOnCall {
		return nil, fmt.Errorf("stub embed error on call %d", e.calls)
	}
	return make768vec(), nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// setupNFacts creates a Service+GitStore with n synthetic facts already synced.
func setupNFacts(t *testing.T, n int) (*Service, *git.Store) {
	ctx := context.Background()
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
	files := make(map[string]string, n)
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("kb/fact-%04d.md", i)] = fmt.Sprintf(
			"---\ntype: observation\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: [item%d]\nrefs: []\n---\n# Fact %d\n\nBody for fact %d.\n",
			i, i, i,
		)
	}
	if _, _, err := gs.BatchWrite("agent/test", files, "add bench facts", "learn"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Index().Sync(ctx, gs, "agent/test"); err != nil {
		t.Fatal(err)
	}
	return svc, gs
}

// countVec returns how many rows are in facts_vec.
func countVec(t *testing.T, svc *Service) int {
	t.Helper()
	var n int
	if err := svc.db.QueryRow("SELECT COUNT(*) FROM facts_vec").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// setupRebuildStore creates a Service with a git store and some facts,
// syncs the index, then returns everything needed for rebuild tests.
func setupRebuildStore(t *testing.T, facts map[string]string) (*Service, *git.Store) {
	ctx := context.Background()
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
		if _, _, err := gs.WriteFile("agent/test", path, content, "add "+path, "learn"); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	if err := svc.Index().Sync(ctx, gs, "agent/test"); err != nil {
		t.Fatalf("initial Sync: %v", err)
	}

	return svc, gs
}

func countFacts(t *testing.T, svc *Service) int {
	t.Helper()
	var n int
	if err := svc.db.QueryRow("SELECT COUNT(*) FROM facts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRebuildFacts_BulkInsert(t *testing.T) {
	ctx := context.Background()
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

	// Clear the facts and branch_facts tables.
	if _, err := svc.db.Exec("DELETE FROM branch_facts"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec("DELETE FROM facts"); err != nil {
		t.Fatal(err)
	}
	if n := countFacts(t, svc); n != 0 {
		t.Fatalf("expected 0 facts after DELETE, got %d", n)
	}

	// Rebuild should repopulate.
	if err := idx.Rebuild(ctx, gs, "agent/test", nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if n := countFacts(t, svc); n != 2 {
		t.Fatalf("expected 2 facts after rebuild, got %d", n)
	}

	// Verify individual facts.
	rec, err := idx.GetByPath(ctx, testBranch, "kb/alpha.md")
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

	rec, err = idx.GetByPath(ctx, testBranch, "kb/beta.md")
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
	ctx := context.Background()
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
	svc.db.Exec("DELETE FROM branch_facts")
	if _, err := svc.db.Exec("DELETE FROM facts"); err != nil {
		t.Fatal(err)
	}

	if err := idx.Rebuild(ctx, gs, "agent/test", nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Only the real fact should be indexed.
	if n := countFacts(t, svc); n != 1 {
		t.Fatalf("expected 1 fact (only real-fact.md), got %d", n)
	}

	rec, err := idx.GetByPath(ctx, testBranch, "kb/real-fact.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected real-fact.md to be indexed")
	}

	// Non-facts should not appear.
	rec, err = idx.GetByPath(ctx, testBranch, "ontology.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("ontology.yaml should not be indexed as a fact")
	}

	rec, err = idx.GetByPath(ctx, testBranch, "kb/no-title.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("kb/no-title.md (no heading) should not be indexed as a fact")
	}
}

func TestRebuildFacts_CommitLogJoin(t *testing.T) {
	ctx := context.Background()
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
	commitV1, _, err := gs.WriteFile("agent/test", "kb/evolving.md", factV1, "add evolving v1", "learn")
	if err != nil {
		t.Fatal(err)
	}

	// Write v2 to same path — creates a second commit touching this file.
	commitV2, _, err := gs.WriteFile("agent/test", "kb/evolving.md", factV2, "update evolving v2", "learn")
	if err != nil {
		t.Fatal(err)
	}

	if commitV1 == commitV2 {
		t.Fatal("expected two distinct commits")
	}

	// Sync to populate index and commit_log.
	if err := svc.Index().Sync(ctx, gs, "agent/test"); err != nil {
		t.Fatal(err)
	}

	// Clear facts and rebuild.
	svc.db.Exec("DELETE FROM branch_facts")
	if _, err := svc.db.Exec("DELETE FROM facts"); err != nil {
		t.Fatal(err)
	}

	if err := svc.Index().Rebuild(ctx, gs, "agent/test", nil); err != nil {
		t.Fatal(err)
	}

	rec, err := svc.Index().GetByPath(ctx, testBranch, "kb/evolving.md")
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
	ctx := context.Background()
	facts := map[string]string{
		"kb/timing.md": "---\ntype: observation\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: [knomit]\nrefs: []\n---\n# Timing Test\n\nBody.\n",
	}

	svc, gs := setupRebuildStore(t, facts)
	idx := svc.Index()

	// Clear facts for rebuild.
	svc.db.Exec("DELETE FROM branch_facts")
	if _, err := svc.db.Exec("DELETE FROM facts"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	phases := make(map[string]bool)
	progress := func(phase string, done, total int) {
		mu.Lock()
		defer mu.Unlock()
		phases[phase] = true
	}

	if err := idx.Rebuild(ctx, gs, "agent/test", progress); err != nil {
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
	ctx := context.Background()
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
		if _, _, err := gs.WriteFile("agent/test", fmt.Sprintf("kb/bench/fact-%03d.md", i), content, fmt.Sprintf("add fact %d", i), "learn"); err != nil {
			b.Fatal(err)
		}
	}

	// Initial sync to populate commit_log and objects.
	if err := svc.Index().Sync(ctx, gs, "agent/test"); err != nil {
		b.Fatal(err)
	}

	db := svc.db
	idx := svc.Index()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Exec("DELETE FROM branch_facts"); db.Exec("DELETE FROM facts")
		db.Exec("DELETE FROM facts_vec")
		if err := idx.Rebuild(ctx, gs, "agent/test", nil); err != nil {
			b.Fatal(err)
		}
	}
}

// TestRebuildEmbeddings_ChunkedProcessing verifies that rebuildEmbeddings
// processes facts in bounded batches rather than loading all body text at once.
// Uses 40 facts (> batchSize=32) so at least two EmbedBatch calls are made.
func TestRebuildEmbeddings_ChunkedProcessing(t *testing.T) {
	ctx := context.Background()
	const nFacts = 40

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

	// Write nFacts facts.
	for i := 0; i < nFacts; i++ {
		content := fmt.Sprintf(
			"---\ntype: observation\ndomain: [chunked]\nconfidence: 0.9\nsources: 1\nentities: [item%d]\nrefs: []\n---\n# Chunked Fact %d\n\nBody for fact number %d.\n",
			i, i, i,
		)
		if _, _, err := gs.WriteFile("agent/test", fmt.Sprintf("kb/fact-%03d.md", i), content, fmt.Sprintf("add fact %d", i), "learn"); err != nil {
			t.Fatal(err)
		}
	}

	// Sync to populate the facts table and objects store.
	if err := svc.Index().Sync(ctx, gs, "agent/test"); err != nil {
		t.Fatal(err)
	}

	// Verify all facts indexed, none yet embedded.
	var factCount int
	if err := svc.db.QueryRow("SELECT COUNT(*) FROM facts").Scan(&factCount); err != nil {
		t.Fatal(err)
	}
	if factCount != nFacts {
		t.Fatalf("expected %d facts, got %d", nFacts, factCount)
	}
	var vecCount int
	svc.db.QueryRow("SELECT COUNT(*) FROM facts_vec").Scan(&vecCount)
	if vecCount != 0 {
		t.Fatalf("expected 0 embeddings before rebuild, got %d", vecCount)
	}

	// Attach counting embedder and run rebuildEmbeddings.
	emb := &countingBatchEmbedder{}
	idx := svc.Index()
	idx.SetEmbedder(emb)

	done, err := idx.rebuildEmbeddings(ctx, nil)
	if err != nil {
		t.Fatalf("rebuildEmbeddings: %v", err)
	}
	if done != nFacts {
		t.Errorf("rebuildEmbeddings returned done=%d, want %d", done, nFacts)
	}

	// Verify all facts got embeddings.
	svc.db.QueryRow("SELECT COUNT(*) FROM facts_vec").Scan(&vecCount)
	if vecCount != nFacts {
		t.Errorf("facts_vec has %d rows, want %d", vecCount, nFacts)
	}

	// Verify the embedder was called in batches of at most batchSize.
	totalTexts := 0
	for _, bsz := range emb.batchSizes {
		if bsz > 32 {
			t.Errorf("EmbedBatch called with %d texts, want <= 32", bsz)
		}
		totalTexts += bsz
	}
	if totalTexts != nFacts {
		t.Errorf("embedder saw %d total texts, want %d", totalTexts, nFacts)
	}
	if len(emb.batchSizes) < 2 {
		t.Errorf("expected >= 2 batch calls for %d facts, got %d", nFacts, len(emb.batchSizes))
	}
}

func TestRebuildEmbeddings_NoEmbedder(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupNFacts(t, 5)
	done, err := svc.Index().rebuildEmbeddings(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done != 0 {
		t.Errorf("done=%d, want 0 with no embedder", done)
	}
	if n := countVec(t, svc); n != 0 {
		t.Errorf("facts_vec has %d rows, want 0", n)
	}
}

func TestRebuildEmbeddings_NothingToEmbed(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupNFacts(t, 5)
	idx := svc.Index()
	idx.SetEmbedder(&countingBatchEmbedder{})

	// First pass embeds all 5.
	if _, err := idx.rebuildEmbeddings(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := countVec(t, svc); n != 5 {
		t.Fatalf("expected 5 after first pass, got %d", n)
	}

	// Second pass: nothing left to embed.
	emb2 := &countingBatchEmbedder{}
	idx.SetEmbedder(emb2)
	done, err := idx.rebuildEmbeddings(ctx, nil)
	if err != nil {
		t.Fatalf("second pass error: %v", err)
	}
	if done != 0 {
		t.Errorf("second pass done=%d, want 0", done)
	}
	if len(emb2.batchSizes) != 0 {
		t.Errorf("embedder called %d times on second pass, want 0", len(emb2.batchSizes))
	}
}

func TestRebuildEmbeddings_SingleBatch(t *testing.T) {
	ctx := context.Background()
	// Fewer than batchSize=32 facts → exactly one EmbedBatch call.
	const n = 10
	svc, _ := setupNFacts(t, n)
	emb := &countingBatchEmbedder{}
	idx := svc.Index()
	idx.SetEmbedder(emb)

	done, err := idx.rebuildEmbeddings(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if done != n {
		t.Errorf("done=%d, want %d", done, n)
	}
	if countVec(t, svc) != n {
		t.Errorf("facts_vec=%d, want %d", countVec(t, svc), n)
	}
	if len(emb.batchSizes) != 1 {
		t.Errorf("expected 1 EmbedBatch call, got %d", len(emb.batchSizes))
	}
	if emb.batchSizes[0] != n {
		t.Errorf("EmbedBatch called with %d texts, want %d", emb.batchSizes[0], n)
	}
}

func TestRebuildEmbeddings_ExactBatchBoundary(t *testing.T) {
	ctx := context.Background()
	// Exactly batchSize=32 facts → one EmbedBatch call of size 32.
	const n = 32
	svc, _ := setupNFacts(t, n)
	emb := &countingBatchEmbedder{}
	idx := svc.Index()
	idx.SetEmbedder(emb)

	done, err := idx.rebuildEmbeddings(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if done != n {
		t.Errorf("done=%d, want %d", done, n)
	}
	if len(emb.batchSizes) != 1 || emb.batchSizes[0] != 32 {
		t.Errorf("batchSizes=%v, want [32]", emb.batchSizes)
	}
}

func TestRebuildEmbeddings_NonBatchEmbedder(t *testing.T) {
	ctx := context.Background()
	// Single-embed path: embedder without EmbedBatch.
	const n = 10
	svc, _ := setupNFacts(t, n)
	emb := &countingSingleEmbedder{}
	idx := svc.Index()
	idx.SetEmbedder(emb)

	done, err := idx.rebuildEmbeddings(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if done != n {
		t.Errorf("done=%d, want %d", done, n)
	}
	if emb.calls != n {
		t.Errorf("Embed called %d times, want %d", emb.calls, n)
	}
	if countVec(t, svc) != n {
		t.Errorf("facts_vec=%d, want %d", countVec(t, svc), n)
	}
}

func TestRebuildEmbeddings_EmbedErrorSkipped(t *testing.T) {
	ctx := context.Background()
	// Single-embed path: one error → that fact is skipped, rest succeed.
	const n = 5
	svc, _ := setupNFacts(t, n)
	emb := &countingSingleEmbedder{errOnCall: 3} // 3rd call fails
	idx := svc.Index()
	idx.SetEmbedder(emb)

	done, err := idx.rebuildEmbeddings(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done != n-1 {
		t.Errorf("done=%d, want %d (one skipped)", done, n-1)
	}
	if countVec(t, svc) != n-1 {
		t.Errorf("facts_vec=%d, want %d", countVec(t, svc), n-1)
	}
}

func TestRebuildEmbeddings_ProgressReporting(t *testing.T) {
	ctx := context.Background()
	const n = 40
	svc, _ := setupNFacts(t, n)
	emb := &countingBatchEmbedder{}
	idx := svc.Index()
	idx.SetEmbedder(emb)

	type report struct{ done, total int }
	var reports []report
	progress := func(phase string, done, total int) {
		if phase == "embeddings" {
			reports = append(reports, report{done, total})
		}
	}

	if _, err := idx.rebuildEmbeddings(ctx, progress); err != nil {
		t.Fatal(err)
	}

	if len(reports) == 0 {
		t.Fatal("no progress reports for embeddings phase")
	}
	// First report: total must equal n.
	if reports[0].total != n {
		t.Errorf("first report total=%d, want %d", reports[0].total, n)
	}
	// Last report: done must equal n.
	last := reports[len(reports)-1]
	if last.done != n {
		t.Errorf("last report done=%d, want %d", last.done, n)
	}
	// Done values must be non-decreasing.
	for i := 1; i < len(reports); i++ {
		if reports[i].done < reports[i-1].done {
			t.Errorf("progress went backwards: %d → %d", reports[i-1].done, reports[i].done)
		}
	}
}

// ── Integration tests ────────────────────────────────────────────────────────

// TestRebuild_WithEmbedder exercises the full three-phase Rebuild with an
// embedder attached, verifying that facts_vec is populated after rebuild.
func TestRebuild_WithEmbedder(t *testing.T) {
	ctx := context.Background()
	const n = 15
	svc, gs := setupNFacts(t, n)
	emb := &countingBatchEmbedder{}
	idx := svc.Index()
	idx.SetEmbedder(emb)

	// Clear facts and facts_vec to simulate a fresh rebuild.
	svc.db.Exec("DELETE FROM branch_facts")
	svc.db.Exec("DELETE FROM facts")
	svc.db.Exec("DELETE FROM facts_vec")

	if err := idx.Rebuild(ctx, gs, "agent/test", nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if countFacts(t, svc) != n {
		t.Errorf("facts=%d after rebuild, want %d", countFacts(t, svc), n)
	}
	if countVec(t, svc) != n {
		t.Errorf("facts_vec=%d after rebuild, want %d", countVec(t, svc), n)
	}
}

// TestRebuild_WithEmbedder_Idempotent verifies that running Rebuild twice
// does not double-insert rows into facts_vec.
func TestRebuild_WithEmbedder_Idempotent(t *testing.T) {
	ctx := context.Background()
	const n = 10
	svc, gs := setupNFacts(t, n)
	idx := svc.Index()
	idx.SetEmbedder(&countingBatchEmbedder{})

	// First rebuild.
	svc.db.Exec("DELETE FROM branch_facts")
	svc.db.Exec("DELETE FROM facts")
	svc.db.Exec("DELETE FROM facts_vec")
	if err := idx.Rebuild(ctx, gs, "agent/test", nil); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}

	// Second rebuild: facts_vec should not grow.
	svc.db.Exec("DELETE FROM branch_facts")
	svc.db.Exec("DELETE FROM facts")
	if err := idx.Rebuild(ctx, gs, "agent/test", nil); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if v := countVec(t, svc); v != n {
		t.Errorf("facts_vec=%d after second rebuild, want %d", v, n)
	}
}

// TestRebuildFacts_CommitLogBranchScoped verifies that rebuildFacts picks
// commit_hash from the correct branch's commit_log, not from another branch
// that happens to have a more recent entry for the same path.
func TestRebuildFacts_CommitLogBranchScoped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	svc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	branchA := "agent/branch-a"
	branchB := "agent/branch-b"

	gs, err := git.InitWithStorer(svc.GitStorer(), nil, branchA)
	if err != nil {
		t.Fatal(err)
	}

	fact := "---\ntype: observation\ndomain: [testing]\nconfidence: 0.9\nsources: 1\nentities: [knomit]\nrefs: []\n---\n# Shared Fact\n\nBody.\n"

	// Write fact on branchA first.
	commitA, _, err := gs.WriteFile(branchA, "kb/shared.md", fact, "add on A", "learn")
	if err != nil {
		t.Fatal(err)
	}

	// Sync branchA to populate index and commit_log.
	if err := svc.Index().Sync(ctx, gs, branchA); err != nil {
		t.Fatal(err)
	}

	// Create branchB from branchA, write same fact — gets a different commit.
	if err := gs.CreateBranch(branchB, branchA); err != nil {
		t.Fatal(err)
	}
	commitB, _, err := gs.WriteFile(branchB, "kb/shared.md", fact, "add on B", "learn")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Index().Sync(ctx, gs, branchB); err != nil {
		t.Fatal(err)
	}

	if commitA == commitB {
		t.Fatal("expected distinct commits for each branch")
	}

	// Clear facts and rebuild only branchA.
	svc.db.Exec("DELETE FROM branch_facts")
	svc.db.Exec("DELETE FROM facts")

	if err := svc.Index().Rebuild(ctx, gs, branchA, nil); err != nil {
		t.Fatal(err)
	}

	rec, err := svc.Index().GetByPath(ctx, branchA, "kb/shared.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected fact after rebuild")
	}

	// The commit_hash must come from branchA's commit_log, not branchB's.
	if rec.CommitHash != commitA {
		t.Fatalf("commit_hash=%q, want branchA's %q (not branchB's %q)", rec.CommitHash, commitA, commitB)
	}
}

// ── Benchmarks ───────────────────────────────────────────────────────────────

func BenchmarkRebuildEmbeddings(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{50, 200, 1000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
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
			// Write all facts in one commit — matches the real learn path (BatchWrite).
			files := make(map[string]string, n)
			for i := 0; i < n; i++ {
				files[fmt.Sprintf("kb/fact-%05d.md", i)] = fmt.Sprintf(
					"---\ntype: observation\ndomain: [bench]\nconfidence: 0.9\nsources: 1\nentities: [item%d]\nrefs: []\n---\n# Bench Fact %d\n\nBody content for benchmark fact %d.\n",
					i, i, i,
				)
			}
			if _, _, err := gs.BatchWrite("agent/test", files, "add bench facts", "learn"); err != nil {
				b.Fatal(err)
			}
			if err := svc.Index().Sync(ctx, gs, "agent/test"); err != nil {
				b.Fatal(err)
			}

			idx := svc.Index()
			emb := &countingBatchEmbedder{}
			idx.SetEmbedder(emb)
			db := svc.db

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				db.Exec("DELETE FROM facts_vec")
				emb.batchSizes = emb.batchSizes[:0]
				if _, err := idx.rebuildEmbeddings(ctx, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
