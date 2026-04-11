package testenv

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// TestCorruptObject_ProducesReachabilityError asserts that
// CorruptObject(blobHash) + ExpectDirty produces a non-clean VerifyWith
// report with a git-reachability Error.
func TestCorruptObject_ProducesReachabilityError(t *testing.T) {
	t.Log("Scenario: write fact, corrupt its blob, ExpectDirty, VerifyWith reports git-reachability")
	sb := NewStoryboard(t)
	r := sb.Repo("alpha")
	agent := r.Branch("agent/test")
	agent.Write("kb/x.md", Fact("x"), "add x")

	// Look up the blob hash via the production API.
	var res store.ReadFactResult
	var err error
	r.ri.WithRead(func(svc *store.Service) {
		res, err = svc.Facts().ReadFact(context.Background(), "agent/test", "kb/x.md",
			&store.ReadFactOpts{WithHash: true})
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.BlobHash)

	r.CorruptObject(res.BlobHash)
	r.ExpectDirty()

	rep := r.VerifyWith(store.VerifyOpts{})
	require.False(t, rep.IsClean(), "report should be non-clean after corrupt")

	found := false
	for _, issue := range rep.Issues {
		if issue.Category == store.CategoryGitReachability && issue.Severity == store.SeverityError {
			found = true
			break
		}
	}
	require.True(t, found, "expected git-reachability Error, got: %v", rep.Issues)
}

// TestRawSQL_CanDeleteBranchFactsRow asserts that RawSQL returns a live
// *sql.DB handle that tests can use to tamper with SQLite rows, and that
// the resulting corruption is visible to Verify.
func TestRawSQL_CanDeleteBranchFactsRow(t *testing.T) {
	t.Log("Scenario: write fact, RawSQL.Exec DELETE FROM branch_facts for it, VerifyWith reports facts-coherence")
	sb := NewStoryboard(t)
	r := sb.Repo("alpha")
	r.Branch("agent/test").Write("kb/x.md", Fact("x"), "add x")

	db := r.RawSQL()
	require.NotNil(t, db)

	_, err := db.Exec(
		`DELETE FROM branch_facts
		 WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND path = ?`,
		"agent/test", "kb/x.md")
	require.NoError(t, err)
	r.ExpectDirty()

	rep := r.VerifyWith(store.VerifyOpts{})
	found := false
	for _, issue := range rep.Issues {
		if issue.Category == store.CategoryFactsCoherence && issue.Path == "kb/x.md" {
			found = true
			break
		}
	}
	require.True(t, found, "expected facts-coherence Error for kb/x.md, got: %v", rep.Issues)
}

// TestRawGitWrite_BypassesFactValidation asserts that RawGitWrite commits
// content that the normal WriteFact path would reject, and that the deep
// fact-format check catches it as a warning.
//
// WriteFact itself does not currently reject malformed YAML at write time
// — it's the downstream indexFile/parseFact that silently skips it. But
// RawGitWrite is positioned as the "bypass fact validation" escape hatch
// for tests that want to write content which doesn't parse. For this
// simple test we just assert that the raw write succeeds and the Deep
// verify warns about the bad YAML.
func TestRawGitWrite_DeepVerifyCatchesBadYAML(t *testing.T) {
	t.Log("Scenario: RawGitWrite malformed YAML, Deep verify reports fact-format Warning")
	sb := NewStoryboard(t)
	r := sb.Repo("alpha")
	// This content has broken YAML (unclosed bracket) so fact.ParseFact fails.
	badContent := "---\nthis is: not\nvalid: [yaml\n---\nbody"
	commit := r.RawGitWrite("agent/test", "kb/bad.md", badContent, "add bad")
	require.NotEmpty(t, commit)

	// Deep verify should warn about the malformed fact. It is NOT an error
	// (warnings don't affect IsClean). We use VerifyWith to inspect issues
	// directly rather than MustVerify which only trips on errors.
	rep := r.VerifyWith(store.VerifyOpts{Deep: true})
	foundWarn := false
	for _, issue := range rep.Issues {
		if issue.Category == store.CategoryFactFormat && issue.Severity == store.SeverityWarning && issue.Path == "kb/bad.md" {
			foundWarn = true
			break
		}
	}
	require.True(t, foundWarn, "expected fact-format Warning for kb/bad.md under Deep verify, got: %v", rep.Issues)

	// Shallow verify must NOT report fact-format issues.
	shallow := r.VerifyWith(store.VerifyOpts{Deep: false})
	for _, issue := range shallow.Issues {
		require.NotEqual(t, store.CategoryFactFormat, issue.Category,
			"shallow verify must not run fact-format check: %v", shallow.Issues)
	}

	// IMPORTANT: RawGitWrite with malformed YAML creates a file in the tree
	// with no matching facts / branch_facts rows (because parseFact fails
	// inside upsert's code path). That IS a facts-coherence Error, which
	// means the teardown auto-verify would fail. Mark the repo dirty.
	r.ExpectDirty()
}
