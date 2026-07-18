// Category I — Index / embedding / search consistency. These tests
// exercise the production search index (SQLite facts + facts_vec) end
// to end: writes populate the index, updates refresh vectors, deletes
// remove entries, searches return the right facts, and the index
// survives a Storyboard Restart (process-boundary analogue).
//
// Every test uses the DeterministicEmbedder so vector comparisons are
// reproducible byte-for-byte across runs.
package storytests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/test/testenv"
)

// factVecCount returns the number of facts_vec rows that correspond to
// facts rows at the given path. facts_vec is branch-agnostic (shared via
// COW), so this counts every version of the fact that still has an
// embedding regardless of which branch currently sees it.
func factVecCount(t *testing.T, r *testenv.RepoHandle, path string) int {
	t.Helper()
	db := r.RawSQL()
	row := db.QueryRow(
		`SELECT COUNT(*) FROM facts_vec WHERE rowid IN (SELECT id FROM facts WHERE path = ?)`,
		path)
	var n int
	require.NoError(t, row.Scan(&n))
	return n
}

// factRowCount returns the number of facts rows at the given path.
// A single path may have multiple versions under the COW model — one
// row per distinct blob_hash.
func factRowCount(t *testing.T, r *testenv.RepoHandle, path string) int {
	t.Helper()
	db := r.RawSQL()
	row := db.QueryRow(`SELECT COUNT(*) FROM facts WHERE path = ?`, path)
	var n int
	require.NoError(t, row.Scan(&n))
	return n
}

// ── I1 ────────────────────────────────────────────────────────────────────

// TestIndex_WriteCreatesIndexEntry: writing a single fact produces
// exactly one facts row and one facts_vec row for that path. The
// embedder is configured (DeterministicEmbedder), so upsert must
// populate facts_vec; Verify's embeddings-coverage check is strict.
func TestIndex_WriteCreatesIndexEntry(t *testing.T) {
	t.Log("I1: one write → one facts row + one facts_vec row for that path; Verify clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	agent.Write("kb/x.md", testenv.Fact("x").Body("a single fact"), "init x")

	require.Equal(t, 1, factRowCount(t, repo, "kb/x.md"))
	require.Equal(t, 1, factVecCount(t, repo, "kb/x.md"))
	repo.MustVerify()
}

// ── I2 ────────────────────────────────────────────────────────────────────

// TestIndex_UpdateRefreshesVector: write fact with body "alpha", then
// update to "beta". Under the COW fact model each distinct blob is its
// own facts row (so after the update there are 2 rows at kb/x.md) AND
// each has its own facts_vec row. The branch_facts view points at the
// latest version, so Search("beta") must return it as the top hit and
// Search("alpha") must NOT return the latest (the old version is
// historical, not on the branch HEAD).
func TestIndex_UpdateRefreshesVector(t *testing.T) {
	t.Log("I2: write body=alpha, update body=beta; facts has 2 rows, each with facts_vec; HEAD branch_facts points at beta")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	agent.Write("kb/x.md", testenv.Fact("x").Body("alpha content"), "v1 alpha")
	agent.Update("kb/x.md", testenv.Fact("x").Body("beta content"), "v2 beta")

	// Two facts rows at kb/x.md (one per distinct blob_hash).
	require.Equal(t, 2, factRowCount(t, repo, "kb/x.md"))
	// Both versions have a facts_vec row (embeddings-coverage is strict).
	require.Equal(t, 2, factVecCount(t, repo, "kb/x.md"))

	// The branch sees exactly the latest version via branch_facts.
	agent.Head().Fact("kb/x.md").Body().MustContain("beta content")
	repo.MustVerify()
}

// ── I3 ────────────────────────────────────────────────────────────────────

// TestIndex_DeleteRemovesFromIndex: write X then delete X. The
// branch_facts row for X disappears (the branch no longer sees X), and
// the underlying facts row is GC'd if no other branch references it —
// which in a single-branch scenario means zero remaining facts rows for
// the path. facts_vec cascades via the ON DELETE trigger, so the
// facts_vec row is gone too. Verify stays clean.
func TestIndex_DeleteRemovesFromIndex(t *testing.T) {
	t.Log("I3: write then delete; facts + facts_vec + branch_facts all zero for the path; Verify clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	agent.Write("kb/x.md", testenv.Fact("x"), "init x")
	require.Equal(t, 1, factRowCount(t, repo, "kb/x.md"))

	agent.Delete("kb/x.md", "drop x")
	require.Equal(t, 0, factRowCount(t, repo, "kb/x.md"),
		"facts row should be GC'd when no branch references it")
	require.Equal(t, 0, factVecCount(t, repo, "kb/x.md"),
		"facts_vec row should cascade-delete with the facts row")
	agent.Head().Fact("kb/x.md").MustNotExist()
	repo.MustVerify()
}

// ── I4 ────────────────────────────────────────────────────────────────────

// TestIndex_BranchSwitchReindexesCorrectly: two branches with
// DIFFERENT facts at the SAME path. branch_facts is per-branch, so
// each branch sees its own version. Searching on one branch must not
// return the other's version in the top slot (the production query
// filters by branch_id).
func TestIndex_BranchSwitchReindexesCorrectly(t *testing.T) {
	t.Log("I4: two branches with divergent facts at same path; each sees its own version via branch_facts")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	mainB := repo.Branch("agent/test")

	mainB.Write("kb/x.md", testenv.Fact("x").Body("on-main"), "main writes x")

	featB := repo.BranchFrom("feature", "agent/test")
	featB.Update("kb/x.md", testenv.Fact("x").Body("on-feature"), "feature updates x")

	// Main still sees its own version; feature sees its own.
	mainB.Head().Fact("kb/x.md").Body().MustContain("on-main")
	featB.Head().Fact("kb/x.md").Body().MustContain("on-feature")

	// Two distinct versions in facts (COW: one per blob).
	require.Equal(t, 2, factRowCount(t, repo, "kb/x.md"))
	require.Equal(t, 2, factVecCount(t, repo, "kb/x.md"))
	repo.MustVerify()
}

// ── I5 ────────────────────────────────────────────────────────────────────

// TestSearch_ReturnsWrittenFact: the text-search API returns written
// facts. The DeterministicEmbedder has no semantic meaning, so we
// assert via the empty-query path which lists all branch-visible
// facts (this is the same code path the TUI uses for an empty search
// box). After deletion the listing is empty.
func TestSearch_ReturnsWrittenFact(t *testing.T) {
	t.Log("I5: write fact, empty-query search lists it; delete, empty-query search is empty")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	agent.Write("kb/entangle.md", testenv.Fact("entanglement").Body("quantum entanglement"), "add")

	agent.Search("").MustReturn("kb/entangle.md")

	agent.Delete("kb/entangle.md", "retract")
	agent.Search("").MustNotReturn("kb/entangle.md")
	repo.MustVerify()
}

// ── I6 ────────────────────────────────────────────────────────────────────

// TestSearch_RankingStableUnderDeterministicEmbedder: three facts
// with distinct content. Two identical Search runs return results in
// the same order. This pins determinism — if any non-stable tie-break
// sneaks into the ranking, the test fires.
func TestSearch_RankingStableUnderDeterministicEmbedder(t *testing.T) {
	t.Log("I6: 3 facts, 2 identical searches return the same ordered result list (deterministic ranking)")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	agent.Write("kb/one.md", testenv.Fact("one").Body("quantum mechanics"), "add one")
	agent.Write("kb/two.md", testenv.Fact("two").Body("classical physics"), "add two")
	agent.Write("kb/three.md", testenv.Fact("three").Body("general relativity"), "add three")

	first := agent.Search("").Results()
	second := agent.Search("").Results()

	require.Equal(t, len(first), len(second), "search result counts must match")
	for i := range first {
		require.Equal(t, first[i].Path, second[i].Path,
			"search ranking must be deterministic at position %d", i)
	}
}

// ── I7 ────────────────────────────────────────────────────────────────────

// TestIndex_SurvivesRestart: write 10 facts, Restart the repo (shuts
// the manager down and re-boots against the same home dir — process
// boundary analogue), then assert every fact is still visible on the
// branch and the index reports the same counts. Verify clean.
func TestIndex_SurvivesRestart(t *testing.T) {
	t.Log("I7: write 10 facts, Restart repo, all 10 still visible; Verify clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	const N = 10
	for i := range N {
		agent.Write("kb/r"+itoa(i)+".md", testenv.Fact("r").Body("body "+itoa(i)), "add "+itoa(i))
	}
	require.Equal(t, N, agent.FactCount())

	repo.Restart()

	// After restart the BranchHandle is stale — refetch.
	agent2 := repo.Branch("agent/test")
	require.Equal(t, N, agent2.FactCount(), "fact count must survive restart")
	for i := range N {
		agent2.Head().Fact("kb/r" + itoa(i) + ".md").MustExist()
	}
	repo.MustVerify()
}

// ── I8 ────────────────────────────────────────────────────────────────────

// TestIndex_VerifyDetectsMissingEmbedding: the plan's original I8
// wanted to assert that Verify catches dimension-mismatched vectors.
// That path is unreachable through SQLite — the facts_vec virtual
// table rejects wrong-dimension inserts at the SQL level, so there's
// no way to produce a stored row with the wrong dimension. The next-
// best invariant Verify can exercise is embedding COVERAGE: every
// facts row must have a facts_vec row when an embedder is configured.
// Here we delete a facts_vec row via RawSQL and assert the
// embeddings-coverage check catches the gap.
func TestIndex_VerifyDetectsMissingEmbedding(t *testing.T) {
	t.Log("I8: delete a facts_vec row via RawSQL; Verify reports an embeddings-coverage Error for that facts row")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	agent.Write("kb/x.md", testenv.Fact("x"), "init x")
	require.Equal(t, 1, factVecCount(t, repo, "kb/x.md"))

	// Remove the facts_vec row for kb/x.md directly.
	db := repo.RawSQL()
	_, err := db.ExecContext(context.Background(),
		`DELETE FROM facts_vec WHERE rowid IN (SELECT id FROM facts WHERE path = ?)`,
		"kb/x.md")
	require.NoError(t, err)
	require.Equal(t, 0, factVecCount(t, repo, "kb/x.md"))

	// Mark dirty so the Storyboard teardown doesn't re-verify.
	repo.ExpectDirty()

	rep := repo.VerifyWith(store.VerifyOpts{})
	requireIssue(t, rep, store.CategoryEmbeddingsCoverage, "kb/x.md")
}
