package store

import (
	"context"
	"path/filepath"
	"testing"

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
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

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
