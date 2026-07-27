// Package storytests contains the invariant test suite for knomit's store
// and repos layers. Tests live here (not in the package they exercise)
// because they use the testenv Storyboard DSL to drive real Service /
// RepoInstance instances end-to-end rather than unit-testing internals.
//
// Category G — Verify detection. These tests deliberately corrupt a repo
// and assert that the production Verify tool reports the right category
// of integrity issue. They are the trust anchor for every other category:
// if Verify can't see corruption, auto-verify in other tests is useless.
//
// Every G test calls r.ExpectDirty() after corrupting so the Storyboard
// teardown auto-verify skips the repo — otherwise the test would fail
// during teardown because the repo is correctly broken.
package storytests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/test/testenv"
)

// ── G1 ────────────────────────────────────────────────────────────────────

// TestVerify_CleanRepoIsClean asserts that a fresh repo with a handful of
// facts written through the normal WriteFact path reports IsClean() AND
// IsStrictlyClean(). This is the baseline — if this fails, every other
// category test is suspect because the auto-verify would have been
// reporting false positives all along.
func TestVerify_CleanRepoIsClean(t *testing.T) {
	t.Log("G1: fresh repo, 3 facts written, VerifyWith Deep:true reports strictly clean")
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("alpha")
	agent := r.Branch("agent/test")
	agent.Write("kb/a.md", testenv.Fact("a"), "add a")
	agent.Write("kb/b.md", testenv.Fact("b"), "add b")
	agent.Write("kb/c.md", testenv.Fact("c"), "add c")

	r.MustVerify() // fails on any Error

	rep := r.VerifyWith(store.VerifyOpts{Deep: true})
	require.True(t, rep.IsStrictlyClean(), "strictly clean: no errors, no warnings: %v", rep.Issues)
}

// ── G2 ────────────────────────────────────────────────────────────────────

// TestVerify_DetectsMissingBlob deletes a blob object from the store and
// asserts the git-reachability check reports an Error naming the blob.
// This is the most structural of all the checks — a missing blob means
// the git object graph itself is broken.
func TestVerify_DetectsMissingBlob(t *testing.T) {
	t.Log("G2: write fact, corrupt blob, ExpectDirty, Verify reports git-reachability Error")
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("alpha")
	r.Branch("agent/test").Write("kb/x.md", testenv.Fact("x"), "add x")

	// Look up the blob hash via the production read-with-hash API.
	var res store.ReadFactResult
	var err error
	r.Instance().WithRead(func(svc *store.Service) {
		res, err = svc.Facts().ReadFact(context.Background(), "agent/test", "kb/x.md",
			&store.ReadFactOpts{WithHash: true})
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.BlobHash)

	r.CorruptObject(res.BlobHash)
	r.ExpectDirty()

	rep := r.VerifyWith(store.VerifyOpts{})
	require.False(t, rep.IsClean())
	requireIssue(t, rep, store.CategoryGitReachability, "")
}

// ── G3 ────────────────────────────────────────────────────────────────────

// TestVerify_DetectsCommitLogGap writes two facts and deletes the second
// commit's branch_commits row via raw SQL. The commit-log parity check
// must detect the missing visibility row. (The check was updated
// 2026-04-09 to key off branch_commits instead of commit_log — a
// legitimate no-op commit with zero file changes has a branch_commits
// row but no commit_log row, and the earlier check falsely flagged
// those. branch_commits is now the authoritative visibility invariant.)
func TestVerify_DetectsCommitLogGap(t *testing.T) {
	t.Log("G3: 2 facts, delete branch_commits row for the second commit, Verify reports commit-log Error")
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("alpha")
	agent := r.Branch("agent/test")
	agent.Write("kb/a.md", testenv.Fact("a"), "add a")
	c2 := agent.Write("kb/b.md", testenv.Fact("b"), "add b")

	// Delete the branch_commits visibility row for c2's commit on agent/test.
	_, err := r.RawSQL().Exec(
		`DELETE FROM branch_commits
		 WHERE commit_hash = ?
		   AND branch_id = (SELECT id FROM branches WHERE name = 'agent/test')`,
		c2.Commit)
	require.NoError(t, err)
	r.ExpectDirty()

	rep := r.VerifyWith(store.VerifyOpts{})
	require.False(t, rep.IsClean())
	requireIssue(t, rep, store.CategoryCommitLog, "")
}

// ── G4 ────────────────────────────────────────────────────────────────────

// TestVerify_DetectsFactsCoherenceGap writes a fact then deletes its
// branch_facts row, leaving the tree with content that has no per-branch
// view row. The facts-coherence check must fire.
func TestVerify_DetectsFactsCoherenceGap(t *testing.T) {
	t.Log("G4: write fact, DELETE branch_facts row, Verify reports facts-coherence Error naming path")
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("alpha")
	r.Branch("agent/test").Write("kb/x.md", testenv.Fact("x"), "add x")

	_, err := r.RawSQL().Exec(
		`DELETE FROM branch_facts
		 WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND path = ?`,
		"agent/test", "kb/x.md")
	require.NoError(t, err)
	r.ExpectDirty()

	rep := r.VerifyWith(store.VerifyOpts{})
	requireIssue(t, rep, store.CategoryFactsCoherence, "kb/x.md")
}

// ── G5 ────────────────────────────────────────────────────────────────────

// TestVerify_DetectsFactsCoherenceBlobMismatch writes a fact then corrupts
// the facts table's blob_hash so it no longer matches the tree blob at
// HEAD. The facts-coherence check compares the two hashes and must fire.
func TestVerify_DetectsFactsCoherenceBlobMismatch(t *testing.T) {
	t.Log("G5: write fact, UPDATE facts.blob_hash to a wrong value, Verify reports facts-coherence Error")
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("alpha")
	r.Branch("agent/test").Write("kb/x.md", testenv.Fact("x"), "add x")

	_, err := r.RawSQL().Exec(
		`UPDATE facts SET blob_hash = ? WHERE path = ?`,
		"0000000000000000000000000000000000000000", "kb/x.md")
	require.NoError(t, err)
	r.ExpectDirty()

	rep := r.VerifyWith(store.VerifyOpts{})
	requireIssue(t, rep, store.CategoryFactsCoherence, "kb/x.md")
}

// ── G6 ────────────────────────────────────────────────────────────────────

// TestVerify_DeepCatchesFactFormat writes malformed YAML via the raw-git
// escape hatch and asserts that Deep:true reports a fact-format Warning
// while Deep:false does not. Warnings don't affect IsClean().
//
// Note: RawGitWrite creates a tree entry for kb/bad.md but bypasses the
// facts-table upsert, which produces a facts-coherence Error on top of
// the fact-format Warning. We only assert the Warning; the Error is
// expected cross-talk and the repo is marked dirty.
func TestVerify_DeepCatchesFactFormat(t *testing.T) {
	t.Log("G6: RawGitWrite malformed YAML, Deep reports fact-format Warning, Shallow does not")
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("alpha")
	badContent := "---\nthis is: not\nvalid: [yaml\n---\nbody"
	r.RawGitWrite("agent/test", "kb/bad.md", badContent, "add malformed")
	r.ExpectDirty()

	deep := r.VerifyWith(store.VerifyOpts{Deep: true})
	foundWarn := false
	for _, issue := range deep.Issues {
		if issue.Category == store.CategoryFactFormat && issue.Severity == store.SeverityWarning && issue.Path == "kb/bad.md" {
			foundWarn = true
			break
		}
	}
	require.True(t, foundWarn, "Deep verify must report fact-format Warning: %v", deep.Issues)

	shallow := r.VerifyWith(store.VerifyOpts{Deep: false})
	for _, issue := range shallow.Issues {
		require.NotEqual(t, store.CategoryFactFormat, issue.Category,
			"shallow verify must not run fact-format check")
	}
}

// ── G7 ────────────────────────────────────────────────────────────────────

// TestVerify_SurvivesMultipleRepos asserts that Verify is scoped to the
// repo it is called on — corrupting one repo does not affect another
// repo's report. Two Storyboard repos, one gets corrupted, both are
// verified independently.
func TestVerify_SurvivesMultipleRepos(t *testing.T) {
	t.Log("G7: corrupt repo A, repo B stays clean, both Verify independently")
	sb := testenv.NewStoryboard(t)
	a := sb.Repo("a")
	b := sb.Repo("b")
	a.Branch("agent/test").Write("kb/x.md", testenv.Fact("x"), "add x")
	b.Branch("agent/test").Write("kb/y.md", testenv.Fact("y"), "add y")

	// Corrupt only A's branch_facts row.
	_, err := a.RawSQL().Exec(
		`DELETE FROM branch_facts
		 WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND path = ?`,
		"agent/test", "kb/x.md")
	require.NoError(t, err)
	a.ExpectDirty()

	aRep := a.VerifyWith(store.VerifyOpts{})
	require.False(t, aRep.IsClean(), "A should be dirty")

	bRep := b.VerifyWith(store.VerifyOpts{})
	require.True(t, bRep.IsClean(), "B should stay clean: %v", bRep.Issues)
}

// ── G8 ────────────────────────────────────────────────────────────────────

// TestVerify_DetectsMissingEmbedding asserts that when the
// DeterministicEmbedder is configured (Storyboard default), deleting a
// facts_vec row for an existing facts row produces an embeddings-coverage
// Error.
func TestVerify_DetectsMissingEmbedding(t *testing.T) {
	t.Log("G8: write fact (embedder configured), DELETE facts_vec row, Verify reports embeddings-coverage Error")
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("alpha")
	r.Branch("agent/test").Write("kb/x.md", testenv.Fact("x"), "add x")

	// Look up the facts row id then delete its embedding.
	var factID int64
	err := r.RawSQL().QueryRow(`SELECT id FROM facts WHERE path = ?`, "kb/x.md").Scan(&factID)
	require.NoError(t, err)
	_, err = r.RawSQL().Exec(`DELETE FROM facts_vec WHERE rowid = ?`, factID)
	require.NoError(t, err)
	r.ExpectDirty()

	rep := r.VerifyWith(store.VerifyOpts{})
	require.False(t, rep.IsClean())
	requireIssue(t, rep, store.CategoryEmbeddingsCoverage, "")
}

// ── G9 ────────────────────────────────────────────────────────────────────

// TestVerify_DetectsMissingGraphFactNode writes a fact then hard-deletes
// its graph Fact node by deleting the node row (edges cascade). The live-nodes-only
// graph-coherence check must fire because the facts row is still there
// but its corresponding live graph node is gone.
func TestVerify_DetectsMissingGraphFactNode(t *testing.T) {
	t.Log("G9: write fact, hard-delete the graph Fact node, Verify reports graph-coherence Error")
	sb := testenv.NewStoryboard(t)
	r := sb.Repo("alpha")
	r.Branch("agent/test").Write("kb/x.md", testenv.Fact("x"), "add x")

	var blobHash string
	err := r.RawSQL().QueryRow(`SELECT blob_hash FROM facts WHERE path = ?`, "kb/x.md").Scan(&blobHash)
	require.NoError(t, err)

	// Delete the graph Fact node hard (not soft-deleted) so the live-nodes
	// count drops below the facts-table count. Mirrors
	// store.Service.deleteGraphFactNodeForTest: resolve the node by
	// (path, blob_hash) and delete it — labels, properties and incident edges
	// follow via ON DELETE CASCADE, which is what Cypher's DETACH DELETE did.
	_, err = r.RawSQL().Exec(`
		DELETE FROM nodes
		WHERE id IN (
			SELECT nl.node_id
			FROM node_labels nl
			JOIN node_props_text p ON p.node_id = nl.node_id
			JOIN property_keys kp ON kp.id = p.key_id AND kp.key = 'path'
			JOIN node_props_text b ON b.node_id = nl.node_id
			JOIN property_keys kb ON kb.id = b.key_id AND kb.key = 'blob_hash'
			WHERE nl.label = 'Fact' AND p.value = ? AND b.value = ?
		)`, "kb/x.md", blobHash)
	require.NoError(t, err)
	r.ExpectDirty()

	rep := r.VerifyWith(store.VerifyOpts{})
	require.False(t, rep.IsClean())
	requireIssue(t, rep, store.CategoryGraphCoherence, "kb/x.md")
}

// ── helpers ───────────────────────────────────────────────────────────────

// requireIssue asserts the report contains at least one Error-severity
// issue with the given category and (if non-empty) path.
func requireIssue(t *testing.T, rep store.IntegrityReport, category, path string) {
	t.Helper()
	for _, issue := range rep.Issues {
		if issue.Severity != store.SeverityError {
			continue
		}
		if issue.Category != category {
			continue
		}
		if path != "" && issue.Path != path {
			continue
		}
		return
	}
	t.Fatalf("expected Error issue category=%q path=%q, got: %v", category, path, rep.Issues)
}
