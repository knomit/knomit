package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRebuild_RepopulatesCommitLogAuthorFromGit guards that :rebuild actually
// rebuilds commit_log from git — not just facts/embeddings/graph. The trap:
// CommitLogSync dedups on branch_commits and commit_log uses INSERT OR IGNORE,
// so re-running populateCommitLog over rows that already exist is a no-op and
// leaves stale data (e.g. rows written before author_name was captured) in
// place. Rebuild must clear this branch's rows and re-walk so the author
// identity is re-read from the source of truth.
func TestRebuild_RepopulatesCommitLogAuthorFromGit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/a.md", testFactBody("a", 0.9, nil), "learn a", "learn")
	require.NoError(t, err)

	// Simulate the pre-column state: every commit_log row has lost its author
	// name. A plain re-walk would skip these (dedup), so they must be cleared.
	_, err = svc.rh.db.Exec(`UPDATE commit_log SET author_name = ''`)
	require.NoError(t, err)

	pre, _, _, err := svc.Search().LogPaginated(ctx, "main", "kb/a.md", 10, "", "", "")
	require.NoError(t, err)
	require.Len(t, pre, 1)
	require.Empty(t, pre[0].Author.Name, "precondition: author name blanked")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))

	post, _, _, err := svc.Search().LogPaginated(ctx, "main", "kb/a.md", 10, "", "", "")
	require.NoError(t, err)
	require.Len(t, post, 1)
	require.Equal(t, "main", post[0].Author.Name, "rebuild must refill author name from git")
	require.Equal(t, "main+learn@agents.knomit.io", post[0].Author.Email)
}
