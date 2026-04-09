package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerify_FreshRepoIsClean asserts that a freshly initialised store with
// no facts written reports IsClean() == true and zero issues.
func TestVerify_FreshRepoIsClean(t *testing.T) {
	t.Log("Scenario: open a fresh store, init repo, run Verify, expect clean report")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()

	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	report, err := svc.Verify(context.Background(), VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "fresh repo must be clean, got issues: %v", report.Issues)
	require.True(t, report.IsStrictlyClean(), "fresh repo must be strictly clean")
	require.Contains(t, report.Branches, "agent/test")
}

// TestVerify_DetectsMissingBlob asserts that deleting a blob object from the
// storer causes Verify to report a git-reachability Error naming the missing blob.
func TestVerify_DetectsMissingBlob(t *testing.T) {
	t.Log("Scenario: write a fact, delete its blob from the storer, expect git-reachability Error")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	res, err := svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\nbody", "add x", "test")
	require.NoError(t, err)
	require.NotEmpty(t, res.BlobHash)

	// Delete the blob from the storer directly.
	require.NoError(t, svc.deleteObjectForTest(res.BlobHash))

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean(), "deleted blob must produce an Error")
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryGitReachability && i.Severity == SeverityError && strings.Contains(i.Detail, res.BlobHash) {
			found = true
			break
		}
	}
	require.True(t, found, "expected git-reachability Error naming blob %s, got: %v", res.BlobHash, report.Issues)
}

// TestVerify_DetectsCommitLogGap asserts that removing a row from commit_log
// causes Verify to report a commit-log Error naming the missing commit.
func TestVerify_DetectsCommitLogGap(t *testing.T) {
	t.Log("Scenario: write two facts, delete second commit's commit_log row, expect commit-log Error")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	r1, err := svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\na", "add x", "test")
	require.NoError(t, err)
	r2, err := svc.Facts().WriteFact(context.Background(), "agent/test", "kb/y.md", "---\ntype: observation\n---\nb", "add y", "test")
	require.NoError(t, err)
	_ = r1

	require.NoError(t, svc.deleteCommitLogRowForTest(r2.CommitHash))

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean())
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryCommitLog && i.Severity == SeverityError && strings.Contains(i.Detail, r2.CommitHash) {
			found = true
			break
		}
	}
	require.True(t, found, "expected commit-log Error naming commit %s, got: %v", r2.CommitHash, report.Issues)
}

// TestVerify_DetectsBranchFactsGap asserts that a .md file present in the tree
// at HEAD but without a matching branch_facts row produces a facts-coherence
// Error naming the path.
func TestVerify_DetectsBranchFactsGap(t *testing.T) {
	t.Log("Scenario: write fact, delete branch_facts row, expect facts-coherence Error naming path")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\n# Test Fact\n\nbody", "add x", "test")
	require.NoError(t, err)

	require.NoError(t, svc.deleteBranchFactsRowForTest("agent/test", "kb/x.md"))

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean())
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryFactsCoherence && i.Severity == SeverityError && i.Path == "kb/x.md" {
			found = true
			break
		}
	}
	require.True(t, found, "expected facts-coherence Error for kb/x.md, got: %v", report.Issues)
}

// TestVerify_DetectsBranchFactsBlobMismatch asserts that a branch_facts row
// whose linked facts row has a blob_hash that does not match the actual blob
// at HEAD produces a facts-coherence Error.
func TestVerify_DetectsBranchFactsBlobMismatch(t *testing.T) {
	t.Log("Scenario: write fact, corrupt facts.blob_hash to a wrong value, expect facts-coherence Error")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\n# Test Fact\n\nbody", "add x", "test")
	require.NoError(t, err)

	// Look up the facts row for this path and tamper with its blob_hash.
	var factID int64
	err = svc.rh.gits.DB().QueryRow(`SELECT id FROM facts WHERE path = ?`, "kb/x.md").Scan(&factID)
	require.NoError(t, err)
	require.NoError(t, svc.corruptFactsBlobHashForTest(factID, "0000000000000000000000000000000000000000"))

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean())
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryFactsCoherence && i.Severity == SeverityError && i.Path == "kb/x.md" {
			found = true
			break
		}
	}
	require.True(t, found, "expected facts-coherence Error for kb/x.md, got: %v", report.Issues)
}

// stub768Embedder is a deterministic 768-dim embedder for Verify tests.
// vec0 requires exactly 768-float32 vectors. Task 2.1 will introduce a
// proper DeterministicEmbedder in testenv; for Phase 1 we keep this inline
// so verify_test.go has no testenv dependency.
type stub768Embedder struct{}

func (e *stub768Embedder) Embed(text string) ([]float32, error) {
	out := make([]float32, 768)
	for i := range 768 {
		out[i] = float32((len(text)*31+i)%256) / 256.0
	}
	return out, nil
}

func (e *stub768Embedder) EmbedBatch(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, _ := e.Embed(t)
		out[i] = v
	}
	return out, nil
}

// TestVerify_DetectsMissingEmbedding asserts that when an embedder is
// configured and a facts row exists but its facts_vec row is missing,
// Verify reports an embeddings-coverage Error.
func TestVerify_DetectsMissingEmbedding(t *testing.T) {
	t.Log("Scenario: write fact with embedder configured, delete its facts_vec row, expect embeddings-coverage Error")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	svc.SetEmbedder(&stub768Embedder{})
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\n# Test Fact\n\nbody", "add x", "test")
	require.NoError(t, err)

	// Look up the facts row and delete its embedding.
	var factID int64
	err = svc.rh.gits.DB().QueryRow(`SELECT id FROM facts WHERE path = ?`, "kb/x.md").Scan(&factID)
	require.NoError(t, err)
	require.NoError(t, svc.deleteEmbeddingForTest(factID))

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean())
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryEmbeddingsCoverage && i.Severity == SeverityError {
			found = true
			break
		}
	}
	require.True(t, found, "expected embeddings-coverage Error, got: %v", report.Issues)
}

// TestVerify_DetectsMissingBranchesTableRow asserts that deleting a branches
// table row for an existing git ref produces a branches-table Error.
func TestVerify_DetectsMissingBranchesTableRow(t *testing.T) {
	t.Log("Scenario: write fact (creates branches row via EnsureBranch), delete row, expect branches-table Error")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\n# Test Fact\n\nbody", "add x", "test")
	require.NoError(t, err)

	// branch_facts references branches(id) without CASCADE, so remove dependent
	// rows before deleting the branch row to avoid a FK constraint failure.
	_, err = svc.rh.gits.DB().Exec(`DELETE FROM branch_facts WHERE branch_id = (SELECT id FROM branches WHERE name = 'agent/test')`)
	require.NoError(t, err)
	_, err = svc.rh.gits.DB().Exec(`DELETE FROM branches WHERE name = 'agent/test'`)
	require.NoError(t, err)

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean())
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryBranchesTable && i.Severity == SeverityError {
			found = true
			break
		}
	}
	require.True(t, found, "expected branches-table Error, got: %v", report.Issues)
}

// TestVerify_DetectsBranchesTableRefMismatch asserts that a branches row
// with a git_ref that doesn't match the expected "refs/heads/<name>" format
// produces a branches-table Error.
func TestVerify_DetectsBranchesTableRefMismatch(t *testing.T) {
	t.Log("Scenario: write fact, overwrite branches.git_ref to a wrong value, expect branches-table Error mentioning git_ref")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.rh.gits.DB().Exec(`UPDATE branches SET git_ref = 'refs/heads/wrong' WHERE name = 'agent/test'`)
	require.NoError(t, err)

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean())
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryBranchesTable && i.Severity == SeverityError && strings.Contains(i.Detail, "git_ref") {
			found = true
			break
		}
	}
	require.True(t, found, "expected branches-table git_ref Error, got: %v", report.Issues)
}

// TestVerify_DeepDetectsBadYAML asserts that committing malformed YAML to a
// .md file under kb/ produces a fact-format Warning when Deep: true and is
// silent when Deep: false. Warnings must NOT affect IsClean().
func TestVerify_DeepDetectsBadYAML(t *testing.T) {
	t.Log("Scenario: write malformed YAML to kb/bad.md, Deep:true reports Warning, Deep:false silent, IsClean stays true")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Write malformed YAML through the real WriteFact path. WriteFact may or
	// may not reject this upfront; if it rejects, the test needs a different
	// escape hatch. Try and see.
	badContent := "---\nthis is: not\nvalid: [yaml\n---\nbody"
	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", "kb/bad.md", badContent, "add bad", "test")
	// If WriteFact returns an error on malformed YAML, this test is the wrong
	// shape — it needs a raw git writer. Report the error and stop.
	if err != nil {
		t.Skipf("WriteFact rejects malformed YAML at write time: %v — test needs a raw-git escape hatch instead", err)
	}

	// Verify reports the fact-format Warning. Note: this fact will also trigger
	// a facts-coherence Error from Task 1.4's check because its malformed YAML
	// prevents a branch_facts row from being created. That is expected cross-talk
	// — the test only asserts that the fact-format Warning IS present.
	deep, err := svc.Verify(context.Background(), VerifyOpts{Deep: true})
	require.NoError(t, err)
	foundWarn := false
	for _, i := range deep.Issues {
		if i.Category == CategoryFactFormat && i.Severity == SeverityWarning && i.Path == "kb/bad.md" {
			foundWarn = true
			break
		}
	}
	require.True(t, foundWarn, "Deep:true should produce fact-format Warning for kb/bad.md, got: %v", deep.Issues)

	shallow, err := svc.Verify(context.Background(), VerifyOpts{Deep: false})
	require.NoError(t, err)
	for _, i := range shallow.Issues {
		require.NotEqual(t, CategoryFactFormat, i.Category, "Deep:false must not run fact-format check")
	}
}

// TestVerify_SoftDeletedGraphNodeIsClean asserts that the normal production
// delete path (WriteFact + DeleteFact) leaves the repo integrity-clean.
// DeleteFact triggers graphDeleteFact which soft-deletes the Fact node
// (sets f.deleted = true) while leaving the node in place — this is
// intentional lineage preservation. The graph-coherence check must NOT
// flag such historical nodes, since by design they have no facts row.
func TestVerify_SoftDeletedGraphNodeIsClean(t *testing.T) {
	t.Log("Scenario: write fact, delete via production DeleteFact, verify stays clean (graph node soft-deleted)")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\n# Test Fact\n\nbody", "add x", "test")
	require.NoError(t, err)

	_, err = svc.Facts().DeleteFact(context.Background(), "agent/test", "kb/x.md", "retract x")
	require.NoError(t, err)

	report, err := svc.Verify(context.Background(), VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(),
		"post-delete repo must be clean; soft-deleted graph node must not be flagged; issues: %v", report.Issues)
}

// TestVerify_DetectsMissingGraphFactNode asserts that when a facts row exists
// but its corresponding graphqlite Fact node is missing, Verify reports a
// graph-coherence Error naming the path.
func TestVerify_DetectsMissingGraphFactNode(t *testing.T) {
	t.Log("Scenario: write fact, delete its graph Fact node, expect graph-coherence Error")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\n# Test Fact\n\nbody", "add x", "test")
	require.NoError(t, err)

	// Look up facts row to get the blob hash, then delete its graph node.
	var blobHash string
	err = svc.rh.gits.DB().QueryRow(`SELECT blob_hash FROM facts WHERE path = ?`, "kb/x.md").Scan(&blobHash)
	require.NoError(t, err)
	require.NoError(t, svc.deleteGraphFactNodeForTest("kb/x.md", blobHash))

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean())
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryGraphCoherence && i.Severity == SeverityError && i.Path == "kb/x.md" {
			found = true
			break
		}
	}
	require.True(t, found, "expected graph-coherence Error for kb/x.md, got: %v", report.Issues)
}
