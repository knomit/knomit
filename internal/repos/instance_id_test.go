package repos

import (
	"context"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

func TestRepoInstanceID_StableAndDistinct(t *testing.T) {
	m := newLifecycleManager(t)
	core := m.Get(config.DefaultRepoName)
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

func TestRepoInstanceID_NoSvcIsEmpty(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "bare", AgentBranch: "agent/test"})
	require.Equal(t, "", ri.ID())
}
