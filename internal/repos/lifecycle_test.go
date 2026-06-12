package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
)

func newLifecycleManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestCreate_PresetMode_RegistersRepo(t *testing.T) {
	m := newLifecycleManager(t)
	var steps []string
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, func(e Event) { steps = append(steps, e.Step) })
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.Equal(t, "work", ri.Name())
	require.NotNil(t, m.Get("work"))
	require.Contains(t, steps, "done")
}

func TestCreate_RejectsExistingActiveName(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: config.DefaultRepoName, Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.ErrorIs(t, err, ErrRepoExists)
}

func TestCreate_RejectsInvalidName(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "Bad Name", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.ErrorIs(t, err, ErrInvalidName)
}

func TestCreate_CustomOntology(t *testing.T) {
	m := newLifecycleManager(t)
	y, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "cust", Mode: "custom", OntologyYAML: string(y),
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)
}

func TestActiveRepoWithOrigin_EmptyWhenNone(t *testing.T) {
	m := newLifecycleManager(t)
	require.Equal(t, "", m.ActiveRepoWithOrigin("https://example.com/x.git"))
}
