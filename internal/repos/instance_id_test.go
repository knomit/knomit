package repos

import (
	"context"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepoInstanceID_StableAndDistinct(t *testing.T) {
	m := newLifecycleManager(t)
	core := m.Get(testRepoName)
	require.NotNil(t, core)

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
	core := m.Get(testRepoName)
	require.NotNil(t, core)

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
