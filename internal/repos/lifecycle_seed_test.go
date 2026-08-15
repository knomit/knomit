package repos

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// The whole point of the mode: an empty remote gets the ontology the user
// chose, not the hardcoded default that clone mode seeds.
//
// This also verifies the two load-bearing side effects R12 calls out: the
// seeded repo ends up with a persisted origin record, and Create actually
// reaches the ActivateSync call (proven via the emitted "sync" progress
// event, which site 5's `spec.hasRemote()` guard controls — the same guard
// that gates the ActivateSync call immediately after it). ActivateSync's own
// success against a remote InitFromRemote never pushed to is a separate,
// pre-existing property shared identically by clone mode of an empty
// remote (see TestCreate_cloneOfEmptyRemoteWritesOntology, which hits the
// same "remote repository is empty" warning) and is out of scope here.
func TestCreate_SeedModeWritesChosenOntology(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	url := "file://" + remoteDir

	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: dir, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentBranch:           "agent/test-abc",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })

	var events []Event
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, func(e Event) { events = append(events, e) })
	require.NoError(t, err)
	require.Equal(t, "source-code", ri.Ontology().ID)

	org, oerr := m.Origins().Get(ri.UID())
	require.NoError(t, oerr)
	require.NotNil(t, org, "seeded repo must have a persisted origin record")
	require.Equal(t, url, org.URL)

	var sawSync bool
	for _, e := range events {
		if e.Step == "sync" {
			sawSync = true
		}
	}
	require.True(t, sawSync, "Create must reach the ActivateSync call for seed mode (site 5's hasRemote() guard)")
}

// Never half-obey: if the remote turns out NOT to be empty, the request is
// refused outright rather than silently falling back to the remote's ontology.
func TestCreate_SeedModeRefusesNonEmptyRemote(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	work := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		require.NoError(t, cmd.Run())
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("commit", "--allow-empty", "-m", "init")
	run("remote", "add", "origin", remoteDir)
	run("push", "origin", "main")
	url := "file://" + remoteDir

	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: dir, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentBranch:           "agent/test-abc",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.True(t, errors.Is(err, ErrRemoteNotEmpty), "err = %v, want ErrRemoteNotEmpty", err)
	require.Nil(t, m.Get("seeded"), "a refused seed must leave no repo registered")
}

func TestCreatePreflight_SeedRequiresOriginAndOntology(t *testing.T) {
	m := newTestManager(t)

	err := m.CreatePreflight(CreateSpec{Name: "a", Mode: "seed", OntologyPreset: "code"})
	require.Error(t, err, "expected rejection when origin is missing")

	err = m.CreatePreflight(CreateSpec{
		Name: "a", Mode: "seed", Origin: &OriginSpec{URL: "/tmp/x"},
	})
	require.Error(t, err, "expected rejection when ontology is missing")
}

// Regression guard: relaxing clone mode is exactly what mode "seed" exists to
// avoid. This must keep failing.
func TestCreate_CloneModeStillRejectsOntologySpec(t *testing.T) {
	m := newTestManager(t)
	err := m.CreatePreflight(CreateSpec{
		Name: "c", Mode: "clone", OntologyPreset: "code",
		Origin: &OriginSpec{URL: "/tmp/x"},
	})
	require.Error(t, err, "clone mode accepted an ontology spec — rejectOntologySpecForClone was weakened")
}
