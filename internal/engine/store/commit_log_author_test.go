package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestCommitLog_SurfacesAuthorIdentity is the regression test for the gap
// where the git author was captured into commit_log.author_email but never
// read back out — so neither LogPaginated nor CommitDetail exposed WHO made a
// commit. The author identity must survive verbatim: the agent-id in the name
// and the +operation-subaddressed email, exactly as written by authorSig.
func TestCommitLog_SurfacesAuthorIdentity(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))

	ctx := context.Background()
	res, err := svc.Facts().WriteFact(ctx, "main", "kb/a.md", testFactBody("a", 0.9, nil), "learn fact a", "learn")
	require.NoError(t, err)
	require.NotEmpty(t, res.CommitHash)

	// authorSig("main", "learn") → name "main", email "main+learn@agents.knomit.io".
	const wantName = "main"
	const wantEmail = "main+learn@agents.knomit.io"

	entries, _, _, err := svc.Search().LogPaginated(ctx, "main", "kb/a.md", 10, "", "", "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, wantName, entries[0].Author.Name, "LogPaginated author name")
	require.Equal(t, wantEmail, entries[0].Author.Email, "LogPaginated author email")

	detail, err := svc.Search().CommitDetail(ctx, res.CommitHash, "")
	require.NoError(t, err)
	require.Equal(t, wantName, detail.Author.Name, "CommitDetail author name")
	require.Equal(t, wantEmail, detail.Author.Email, "CommitDetail author email")
}

// emptyCommit writes a commit over tip that changes nothing: authored at one time
// and committed at another. It is in the object store only; the index does not
// know it until AppendCommitLog.
func emptyCommit(t *testing.T, svc *Service, tip string, authored, committed time.Time) plumbing.Hash {
	t.Helper()
	st := svc.rh.Storer()
	parent, err := object.GetCommit(st, plumbing.NewHash(tip))
	require.NoError(t, err)
	c := &object.Commit{
		Author:    object.Signature{Name: "someone", Email: "someone+update@agents.knomit.io", When: authored},
		Committer: object.Signature{Name: "someone", Email: "someone+update@agents.knomit.io", When: committed},
		TreeHash:  parent.TreeHash, ParentHashes: []plumbing.Hash{parent.Hash},
		Message: "nothing changed\n\nmore words",
	}
	enc := st.NewEncodedObject()
	require.NoError(t, c.Encode(enc))
	hash, err := st.SetEncodedObject(enc)
	require.NoError(t, err)
	return hash
}

func openWithOneFact(t *testing.T) (*Service, string) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))
	res, err := svc.Facts().WriteFact(context.Background(), "main", "kb/a.md", testFactBody("a", 0.9, nil), "learn fact a", "learn")
	require.NoError(t, err)
	return svc, res.CommitHash
}

// A commit that changed no file has no commit_log rows but is a commit like any
// other (kb/invariants/store/three-table-commit-index): recorded on branch_commits,
// it answers from the commit object with no files, dated as commit_log dates a
// commit, by the committer. A commit the repository does not hold is still an error.
// Sabotage: answer "not found" on no rows, answer every hash, or date it by the author.
func TestCommitDetail_EmptyCommitAnswersFromTheObject(t *testing.T) {
	ctx := context.Background()
	svc, tip := openWithOneFact(t)
	authored := time.Unix(1_700_000_000, 0).UTC()
	committed := time.Unix(1_790_000_000, 0).UTC()
	hash := emptyCommit(t, svc, tip, authored, committed)
	require.NoError(t, svc.rh.AppendCommitLog(ctx, "main", hash.String()))

	detail, err := svc.Search().CommitDetail(ctx, hash.String(), "kb")
	require.NoError(t, err)
	require.Equal(t, hash.String(), detail.Commit)
	require.Equal(t, "nothing changed", detail.Message)
	require.Equal(t, "update", detail.Operation)
	require.Equal(t, "someone", detail.Author.Name)
	require.Equal(t, committed.Format(time.RFC3339), detail.Date, "the committer's time, as commit_log records it")
	require.NotNil(t, detail.Files)
	require.Empty(t, detail.Files)

	_, err = svc.Search().CommitDetail(ctx, "0123456789012345678901234567890123456789", "kb")
	require.Error(t, err)
}

// A commit the object store holds but the index has not recorded may have real
// changes, so it must not read as empty. Sabotage: drop the branch_commits check.
func TestCommitDetail_UnrecordedCommitIsNotEmpty(t *testing.T) {
	ctx := context.Background()
	svc, tip := openWithOneFact(t)
	when := time.Unix(1_790_000_000, 0).UTC()
	hash := emptyCommit(t, svc, tip, when, when)
	_, err := svc.Search().CommitDetail(ctx, hash.String(), "kb")
	require.Error(t, err)
}

// Only "no rows" means a commit changed nothing. A row that cannot be read is a
// failure, not an empty commit. Sabotage: drop the ErrNoRows check, and this
// recorded commit answers from its object with no files.
func TestCommitDetail_AFailedReadIsNotAnEmptyCommit(t *testing.T) {
	ctx := context.Background()
	svc, tip := openWithOneFact(t)
	when := time.Unix(1_790_000_000, 0).UTC()
	hash := emptyCommit(t, svc, tip, when, when)
	require.NoError(t, svc.rh.AppendCommitLog(ctx, "main", hash.String()))
	// A value the Scan into a time refuses.
	_, err := svc.rh.DB().ExecContext(ctx,
		`INSERT INTO commit_log (commit_hash, path, message, operation, author_name, author_email, action, committed_at) VALUES (?, 'kb/a.md', 'm', 'x', 'n', 'e', 'modified', 'not-a-number')`,
		hash.String())
	require.NoError(t, err)
	_, err = svc.Search().CommitDetail(ctx, hash.String(), "kb")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found in history index")
}
