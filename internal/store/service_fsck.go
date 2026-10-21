package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	"knomit/internal/store/fsck"
)

// DeleteObjectForTest removes a git object (blob, tree, or commit) from the
// storer by hash. EXISTS ONLY for integrity-check tests that need to corrupt
// the store. Do not call from production code paths.
//
// Exported (vs. the previous lowercase form) so the testenv DSL's
// CorruptObject helper in a different package can call it.
func (s *Service) DeleteObjectForTest(hash string) error {
	return s.rh.Storer().DeleteObjectForTest(plumbing.NewHash(hash))
}

// RawDBForTest returns the underlying *sql.DB handle. EXISTS ONLY for
// integrity-check tests that need to tamper with SQLite rows directly —
// deleting commit_log entries, inserting ghost rows, corrupting
// facts.blob_hash, etc. Do not call from production code paths.
func (s *Service) RawDBForTest() *sql.DB {
	return s.rh.Storer().DB()
}

// FetchRefspecsForTest returns the fetch refspecs configured on the named git
// remote, or nil when the remote (or the repository) is absent. EXISTS ONLY so
// tests in other packages can assert that an origin change rewrote the git
// config. See vcs.Repo.FetchRefspecsForTest.
func (s *Service) FetchRefspecsForTest(remote string) []string {
	return s.rh.FetchRefspecsForTest(remote)
}

// RemoteURLsForTest returns the URLs configured on the named git remote, or nil
// when the remote (or the repository) is absent. See
// vcs.Repo.RemoteURLsForTest.
func (s *Service) RemoteURLsForTest(remote string) []string {
	return s.rh.RemoteURLsForTest(remote)
}

// RawWriteForTest commits raw content to a path on the given branch,
// bypassing fact.ParseFact validation. EXISTS ONLY for integrity-check
// tests that need to inject deliberately malformed fact content (e.g. to
// exercise the deep fact-format check). The normal WriteFact path rejects
// invalid YAML at write time, which this escape hatch skips.
//
// The commit is created with the standard author/committer signatures and
// is signed. commit_log and the index are synced so the post-state is
// structurally consistent — only the FACT FORMAT is broken.
func (s *Service) RawWriteForTest(ctx context.Context, branch, path, content, message string) (string, error) {
	commitHash, _, err := s.fi.WriteFileExactForTest(ctx, branch, strings.ToLower(path), content, message, "raw-write-test", "")
	if err != nil {
		return "", err
	}
	return commitHash, nil
}

// RawSymlinkForTest commits a SYMLINK entry (mode 120000) at path, case
// kept, whose blob is target. EXISTS ONLY for tests of how readers treat a
// symlink under .knomit/. See vcs.Repo.RawSymlinkForTest.
func (s *Service) RawSymlinkForTest(ctx context.Context, branch, path, target, message string) (string, error) {
	return s.rh.RawSymlinkForTest(ctx, branch, path, target, message)
}

// integrity returns the integrity checker over this Service's repository
// handler and indices (fsck/integrity.go). The checks take these three pieces rather
// than the Service so the checker does not depend on the facade.
func (s *Service) integrity() *fsck.Checker {
	return fsck.NewChecker(s.rh.Repo, s.fi, s.si)
}

// Verify walks the store and returns an integrity report. See integrity.Verify.
func (s *Service) Verify(ctx context.Context, opts VerifyOpts) (IntegrityReport, error) {
	return s.integrity().Verify(ctx, opts)
}

// PruneGeneratedRefs deletes generated export residue. See
// integrity.PruneGeneratedRefs.
func (s *Service) PruneGeneratedRefs(ctx context.Context) (PruneResult, error) {
	return s.integrity().PruneGeneratedRefs(ctx)
}
