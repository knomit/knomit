package testenv

import (
	"context"
	"database/sql"

	"knomit/internal/store"
)

// Corruption helpers on RepoHandle. These exist ONLY for Verify-detection
// tests (Category G in the scenario suite) that deliberately produce an
// invalid repo state to assert that Verify catches it. Every test that
// calls one of these MUST also call r.ExpectDirty() so the Storyboard
// teardown auto-verify skips the repo — otherwise the test fails during
// teardown because the repo is correctly broken.

// CorruptObject deletes a git object (blob, tree, or commit) from the
// repo's SQLite-backed object store by hash. The next Verify run's
// git-reachability check will flag the dangling reference.
//
// Usage pattern:
//
//	r := sb.Repo("alpha")
//	snap := r.Branch("agent/test").Write("kb/x.md", Fact("x"), "add x")
//	// resolve the blob hash, then delete it
//	res, _ := r.Instance().Facts().ReadFact(ctx, "agent/test", "kb/x.md",
//	    &store.ReadFactOpts{WithHash: true})
//	r.CorruptObject(res.BlobHash)
//	r.ExpectDirty()
//	rep := r.VerifyWith(store.VerifyOpts{})
//	// assert rep contains a git-reachability Error naming res.BlobHash
func (r *RepoHandle) CorruptObject(hash string) {
	t := r.sb.t
	t.Helper()
	var err error
	r.ri.WithRead(func(svc *store.Service) {
		err = svc.DeleteObjectForTest(hash)
	})
	if err != nil {
		t.Fatalf("CorruptObject(%s): %v", hash, err)
	}
}

// RawSQL returns the underlying *sql.DB handle for arbitrary tampering.
// Tests that need to DELETE / UPDATE / INSERT rows directly (bypassing
// the production write path) use this to set up corruption scenarios.
//
// Usage pattern:
//
//	r := sb.Repo("alpha")
//	r.Branch("agent/test").Write("kb/x.md", Fact("x"), "add x")
//	_, err := r.RawSQL().Exec(
//	    `DELETE FROM branch_facts WHERE path = ?`, "kb/x.md")
//	require.NoError(t, err)
//	r.ExpectDirty()
//
// The returned *sql.DB is the same handle the production code uses. Do
// not Close it — the repo owns its lifecycle.
func (r *RepoHandle) RawSQL() *sql.DB {
	var db *sql.DB
	r.ri.WithRead(func(svc *store.Service) {
		db = svc.RawDBForTest()
	})
	return db
}

// RawGitWrite commits raw content to a path on the given branch, bypassing
// fact.ParseFact validation. Used by the G6 deep-fact-format test to inject
// malformed YAML that the normal WriteFact path would reject.
//
// The commit IS created with real git machinery — tree, commit object,
// signature, ref update, commit_log append, index sync — only the FACT
// CONTENT is invalid. The resulting state is structurally consistent
// (commit_log parity, branches-table parity, etc all clean) while the
// fact-format deep check fires on the malformed YAML.
//
// Returns the commit hash.
func (r *RepoHandle) RawGitWrite(branch, path, content, message string) string {
	t := r.sb.t
	t.Helper()
	var commitHash string
	var err error
	r.ri.WithRead(func(svc *store.Service) {
		commitHash, err = svc.RawWriteForTest(context.Background(), branch, path, content, message)
	})
	if err != nil {
		t.Fatalf("RawGitWrite(%s on %s): %v", path, branch, err)
	}
	return commitHash
}
