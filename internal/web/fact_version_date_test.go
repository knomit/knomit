package web

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// openTestStore builds a real store with one fact on "main" and returns the
// service plus the commit that wrote the fact.
func openTestStore(t *testing.T) (*store.Service, string) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	res, err := svc.Facts().WriteFact(context.Background(), "main", "kb/t.md",
		"---\ntype: observation\nconfidence: 0.9\n---\n# t\n\nbody\n", "create t", "")
	require.NoError(t, err)
	return svc, res.CommitHash
}

func TestVersionDateFromService_ReturnsRFC3339(t *testing.T) {
	svc, commit := openTestStore(t)

	got := versionDateFromService(context.Background(), svc, "main", "kb/t.md", commit)

	require.NotEmpty(t, got, "a fact with a commit_log row must yield a date")
	parsed, err := time.Parse(time.RFC3339, got)
	require.NoError(t, err, "must be RFC3339, got %q", got)
	require.WithinDuration(t, time.Now(), parsed, time.Hour)
}

func TestVersionDateFromService_EmptyWhenUnresolvable(t *testing.T) {
	svc, commit := openTestStore(t)

	require.Empty(t, versionDateFromService(context.Background(), svc, "main", "kb/nope.md", commit),
		"unknown path yields no date, never the zero time")
	require.Empty(t, versionDateFromService(context.Background(), svc, "main", "kb/t.md", ""),
		"empty anchor yields no date")
}
