package storetest

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/crypto/ssh"

	"knomit/internal/store"
	"knomit/internal/store/internal/sqlite"
	"knomit/internal/store/internal/vcs"
)

// SetRef moves (or creates) the git reference name to the commit hash WITHOUT a
// commit and without NotifyCommit, for a test that needs a ref shape the write
// path never produces: an agent branch rewound so its old head is no longer an
// ancestor, or main moved as if the acceptance gate had advanced it.
func SetRef(s *store.Service, name, hash string) error {
	return vcs.ServiceRepoForTest(s).SetRefForTest(name, hash)
}

// DeleteRef removes a git reference WITHOUT NotifyCommit, for a test that
// needs a repo with no upstream branch.
func DeleteRef(s *store.Service, name string) error {
	return vcs.ServiceRepoForTest(s).DeleteRefForTest(name)
}

// CommitFiles commits files onto branch as ONE signed commit, moving the ref
// but skipping NotifyCommit: no commit_log row, no index sync, no observer. It
// exists for a test that needs a very large advance, or a commit the write
// path would refuse, without paying the index's cost for it. Returns the commit
// hash.
func CommitFiles(s *store.Service, branch string, files map[string]string, message string) (string, error) {
	return vcs.ServiceRepoForTest(s).CommitFilesForTest(branch, files, message)
}

// RawWrite commits raw content to a path on branch, bypassing fact.ParseFact
// validation, for an integrity-check test that must inject deliberately
// malformed fact content. The path is lowercased as WriteFact would lowercase
// it; the commit is signed and commit_log and the index are synced, so only the
// FACT FORMAT is broken. Returns the commit hash.
func RawWrite(ctx context.Context, s *store.Service, branch, path, content, message string) (string, error) {
	commitHash, _, err := s.Facts().WriteFileExactForTest(ctx, branch, strings.ToLower(path), content, message, "raw-write-test", "")
	if err != nil {
		return "", err
	}
	return commitHash, nil
}

// RawSymlink commits a SYMLINK entry (mode 120000) at path, case kept, whose
// blob is target, for a test of how readers treat a symlink under .knomit/.
func RawSymlink(ctx context.Context, s *store.Service, branch, path, target, message string) (string, error) {
	return vcs.ServiceRepoForTest(s).RawSymlinkForTest(ctx, branch, path, target, message)
}

// RawDB is the database handle under s, for an integrity test that tampers with
// SQLite rows directly: deleting commit_log entries, inserting ghost rows,
// corrupting facts.blob_hash.
func RawDB(s *store.Service) *sql.DB {
	return vcs.ServiceRepoForTest(s).Storer().DB()
}

// DeleteObject removes a git object (blob, tree or commit) from the storer by
// hash, for an integrity test that must corrupt the store.
func DeleteObject(s *store.Service, hash string) error {
	return vcs.ServiceRepoForTest(s).Storer().DeleteObjectForTest(plumbing.NewHash(hash))
}

// FetchRefspecs returns the fetch refspecs configured on the named git remote,
// or nil when the remote (or the repository) is absent, so a test can assert
// that an origin change rewrote the git config.
func FetchRefspecs(s *store.Service, remote string) []string {
	return vcs.ServiceRepoForTest(s).FetchRefspecsForTest(remote)
}

// RemoteURLs returns the URLs configured on the named git remote, or nil when
// the remote (or the repository) is absent.
func RemoteURLs(s *store.Service, remote string) []string {
	return vcs.ServiceRepoForTest(s).RemoteURLsForTest(remote)
}

// SetExperimentActivity backdates an experiment's last_activity_at. Expiry is
// measured in days, so a test of the sweeper either sleeps for days or
// backdates the row; backdating makes the assertion about WHICH experiment the
// cutoff selects. Production records activity through NotifyCommit only.
func SetExperimentActivity(ctx context.Context, s *store.Service, name string, when time.Time) error {
	return s.Merge().SetExperimentActivityForTest(ctx, name, when)
}

// DropBranch removes branch from the git storer (ref deletion) and from every
// SQLite table that references it; the commits themselves stay in the object
// store. No production code outside the store drops a branch (the store's own
// experiment and lifecycle paths call the vcs handle), so the door is a test
// door rather than a Service method.
func DropBranch(ctx context.Context, s *store.Service, name string) error {
	return vcs.ServiceRepoForTest(s).DropBranch(ctx, name)
}

// SetDefaultBranch points the repository's symbolic HEAD at branch, for a test
// that needs a default branch other than the one InitRepo chose (a clone whose
// remote HEAD is not main, a foreign repo being adopted).
func SetDefaultBranch(s *store.Service, branch string) error {
	return vcs.ServiceRepoForTest(s).SetDefaultBranch(branch)
}

// SetFallbackSigner installs signer as the commit signer of every Service in
// this test binary that was given none, so fixtures that open a store without
// SetSigner can still commit. Nil removes it.
func SetFallbackSigner(signer ssh.Signer) { vcs.SetTestFallbackSigner(signer) }

// SetFallbackAgentID installs f as the agent id of every Service in this test
// binary that was given none; f maps a branch to its fixture id. Nil removes it.
func SetFallbackAgentID(f func(branch string) string) { vcs.SetTestFallbackAgentID(f) }

// SetFastDurability turns SQLite's fsyncs off for every database this test
// binary opens from then on. Call it from TestMain, before m.Run.
func SetFastDurability(on bool) { sqlite.SetTestFastDurability(on) }
