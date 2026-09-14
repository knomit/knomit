package repos

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

func TestRepoInstanceID_StableAndDistinct(t *testing.T) {
	m := newLifecycleManager(t)
	core := createRepo(t, m, testRepoName)

	id := core.ID()
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{40}$`), id)
	require.Equal(t, id, core.ID(), "ID must be cached/stable")

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)
	work := m.Get("work")
	require.NotNil(t, work)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{40}$`), work.ID())
	require.NotEqual(t, id, work.ID(), "independently created repos have distinct IDs")
}

func TestRepoInstance_ShortID(t *testing.T) {
	m := newLifecycleManager(t)
	core := createRepo(t, m, testRepoName)

	id := core.ID()
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{40}$`), id)

	short := core.ShortID()
	require.Len(t, short, 12, "ShortID is the 12-hex wire form")
	require.Equal(t, id[:12], short, "ShortID is the first 12 chars of ID")
}

func TestRepoInstanceID_NoSvcIsEmpty(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "bare", AgentBranch: "agent/test"})
	// A bare instance never resolves an ID. Two calls both return "" without
	// panicking, exercising the retry path (failure is never cached as latched).
	require.Equal(t, "", ri.ID())
	require.Equal(t, "", ri.ID())
}

// ID() resolves on the READ branch: a subscription with no agent branch must
// still have an identity, or it cannot be addressed by kb:// or mounted.
func TestID_ResolvesOnReadBranchForSubscription(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	sub := NewTestInstanceWithDeps(TestInstanceConfig{Name: "sub", Svc: svc, Subscribed: true, ReadBranch: "main"})
	full := NewTestInstanceWithDeps(TestInstanceConfig{Name: "full", Svc: svc, AgentBranch: "agent/test"})
	require.NotEmpty(t, sub.ID())
	require.Equal(t, full.ID(), sub.ID())
}
