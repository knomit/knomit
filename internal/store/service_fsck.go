package store

import (
	"context"
	"database/sql"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/store/fsck"
	"knomit/internal/store/internal/vcs"
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
// tests in other packages can assert that an origin change actually rewrote the
// git config — the refspec is what decides which branch the next Sync fetches,
// and it is otherwise invisible from outside this package (go-git's config lives
// in the SQLite-backed storer, not a .git/config file on disk).
//
// Without it, deleting the ConfigureRemote call from an upstream-change path
// leaves the stored branch correct and the fetch silently tracking the OLD
// branch — see TestPatchOriginUpstream_PersistsToControlDB.
func (s *Service) FetchRefspecsForTest(remote string) []string {
	if s.rh.Repository() == nil {
		return nil
	}
	cfg, err := s.rh.Repository().Config()
	if err != nil {
		return nil
	}
	rc, ok := cfg.Remotes[remote]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rc.Fetch))
	for _, rs := range rc.Fetch {
		out = append(out, string(rs))
	}
	return out
}

// RemoteURLsForTest returns the URLs configured on the named git remote, or nil
// when the remote (or the repository) is absent. EXISTS ONLY as the URL half of
// FetchRefspecsForTest: an origin write that re-points the git remote and then
// fails to make that durable has to be observed as a URL, because the refspecs
// are unchanged when only the url moved.
//
// See TestSetOrigin_FailedPersistRestoresTheGitRemote — without this, a repo
// left fetching a url nothing records looks identical to one that was never
// touched.
func (s *Service) RemoteURLsForTest(remote string) []string {
	if s.rh.Repository() == nil {
		return nil
	}
	cfg, err := s.rh.Repository().Config()
	if err != nil {
		return nil
	}
	rc, ok := cfg.Remotes[remote]
	if !ok {
		return nil
	}
	return append([]string(nil), rc.URLs...)
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
	commitHash, _, err := s.fi.WriteFile(ctx, branch, path, content, message, "raw-write-test")
	if err != nil {
		return "", err
	}
	return commitHash, nil
}

// RawSymlinkForTest commits a SYMLINK entry (mode 120000) at path, case
// kept, whose blob is target — what `git add` of a symlink records. EXISTS
// ONLY for tests of how readers treat a symlink under .knomit/ (never
// followed, read as absent); no writer in knomit can create one.
func (s *Service) RawSymlinkForTest(ctx context.Context, branch, path, target, message string) (string, error) {
	return s.rawEntryForTest(ctx, branch, path, target, message, filemode.Symlink)
}

func (s *Service) rawEntryForTest(ctx context.Context, branch, path, content, message string, mode filemode.FileMode) (string, error) {
	target := content
	rh := s.rh
	unlock := rh.LockBranch(branch)
	defer unlock()
	head, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", err
	}
	tip, err := rh.Repository().CommitObject(head)
	if err != nil {
		return "", err
	}
	root, err := tip.Tree()
	if err != nil {
		return "", err
	}
	blob, err := vcs.WriteBlobToStore(rh.Storer(), []byte(target))
	if err != nil {
		return "", err
	}
	rootHash, err := vcs.BuildTreeMode(rh.Storer(), root, path, mode, blob)
	if err != nil {
		return "", err
	}
	c := &object.Commit{Author: tip.Author, Committer: tip.Committer, Message: message,
		TreeHash: rootHash, ParentHashes: []plumbing.Hash{head}}
	obj := rh.Storer().NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return "", err
	}
	h, err := rh.Storer().SetEncodedObject(obj)
	if err != nil {
		return "", err
	}
	if err := rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)); err != nil {
		return "", err
	}
	return h.String(), nil
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
