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

// A commit that changed no file has no commit_log rows but is a commit like any
// other (kb/invariants/store/three-table-commit-index): it answers from the
// commit object with no files, while a commit the repository does not hold is
// still an error. Sabotage: answer "not found" on no rows, or answer every hash.
func TestCommitDetail_EmptyCommitAnswersFromTheObject(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(ctx, map[string]string{}, "main"))
	res, err := svc.Facts().WriteFact(ctx, "main", "kb/a.md", testFactBody("a", 0.9, nil), "learn fact a", "learn")
	require.NoError(t, err)

	st := svc.rh.Storer()
	parent, err := object.GetCommit(st, plumbing.NewHash(res.CommitHash))
	require.NoError(t, err)
	when := time.Unix(1_790_000_000, 0).UTC()
	author := object.Signature{Name: "someone", Email: "someone+update@agents.knomit.io", When: when}
	empty := &object.Commit{
		Author: author, Committer: author, TreeHash: parent.TreeHash, ParentHashes: []plumbing.Hash{parent.Hash},
		Message: "nothing changed\n\nmore words",
	}
	enc := st.NewEncodedObject()
	require.NoError(t, empty.Encode(enc))
	hash, err := st.SetEncodedObject(enc)
	require.NoError(t, err)

	detail, err := svc.Search().CommitDetail(ctx, hash.String(), "kb")
	require.NoError(t, err)
	require.Equal(t, hash.String(), detail.Commit)
	require.Equal(t, "nothing changed", detail.Message)
	require.Equal(t, "update", detail.Operation)
	require.Equal(t, "someone", detail.Author.Name)
	require.Equal(t, when.Format(time.RFC3339), detail.Date)
	require.NotNil(t, detail.Files)
	require.Empty(t, detail.Files)

	_, err = svc.Search().CommitDetail(ctx, "0123456789012345678901234567890123456789", "kb")
	require.Error(t, err)
}
