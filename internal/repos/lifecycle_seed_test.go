package repos

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// newSeedManager returns a started Manager rooted at dir with LocalOriginRoot
// set to dir, so file:// origin fixtures created under dir are permitted —
// newTestManager (this package's default fixture) leaves LocalOriginRoot
// unset, which disables filesystem origins entirely, and every seed test
// needs a file:// remote.
func newSeedManager(t *testing.T, dir string) *Manager {
	t.Helper()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: dir, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentBranch:           "agent/test-abc",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	return m
}

// remoteRefs returns the ref names a bare repo at dir currently holds, or nil
// if it has none. `git show-ref` exits non-zero on a genuinely empty repo, so
// a non-nil error here just means "still empty" rather than a real failure.
func remoteRefs(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "show-ref").CombinedOutput()
	if err != nil {
		return nil
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		refs = append(refs, fields[len(fields)-1])
	}
	return refs
}

// The whole point of the mode: an empty remote gets the ontology the user
// chose, not the hardcoded default that clone mode seeds.
//
// This also verifies the load-bearing side effects R12/finding-4 call out:
// the seeded repo ends up with a persisted origin record (URL AND the
// RESOLVED branch — org.Branch must be the branch InitFromRemote actually
// adopted, "main", never the empty string the caller requested, per the same
// invariant initClone's doc comment pins), and the remote genuinely receives
// both the consensus branch and the agent branch — proof that initSeed's
// push runs, not just that Create reached the ActivateSync call. Without
// that push, InitFromRemote alone leaves the remote ref-less forever (it
// never pushes on its own) and ActivateSync's first fetch would fail against
// it, exactly the disaster-recovery gap seed mode exists to close.
func TestCreate_SeedModeWritesChosenOntology(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	url := "file://" + remoteDir
	m := newSeedManager(t, dir)

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "source-code", ri.Ontology().ID)

	org, oerr := m.Origins().Get(ri.UID())
	require.NoError(t, oerr)
	require.NotNil(t, org, "seeded repo must have a persisted origin record")
	require.Equal(t, url, org.URL)
	require.Equal(t, "main", org.Branch,
		"origin.Branch must be the RESOLVED upstream InitFromRemote adopted, not the empty string the spec requested")

	refs := remoteRefs(t, remoteDir)
	require.Contains(t, refs, "refs/heads/main", "seed must push the consensus branch, not just create it locally")
	require.Contains(t, refs, "refs/heads/agent/test-abc", "seed must push the agent branch, not just create it locally")
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
	m := newSeedManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.True(t, errors.Is(err, ErrRemoteNotEmpty), "err = %v, want ErrRemoteNotEmpty", err)
	require.Nil(t, m.Get("seeded"), "a refused seed must leave no repo registered")
}

// Finding 1 regression: Create is also reachable directly (tests, future CLI
// paths), bypassing CreatePreflight entirely, so initSeed must re-assert the
// "ontology required" rule itself rather than trust the preflight check —
// mirroring initClone's own authoritative re-assertion of
// rejectOntologySpecForClone. Without it, Create(seed, no ontology fields)
// would silently seed fact.DefaultOntology() — the exact silent default this
// mode exists to prevent.
func TestCreate_SeedModeRequiresOntology_AuthoritativeInCreate(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	url := "file://" + remoteDir
	m := newSeedManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.ErrorIs(t, err, ErrInvalidName)
	require.Nil(t, m.Get("seeded"), "a seed request with no ontology must leave no repo registered")
}

// initSeed's failure paths are only two of the four fully covered above and
// below (empty ontology, non-empty remote); these pin the remaining two:
// a remote the probe can't reach at all, and an ontology_yaml that fails to
// parse — both of which must fail WITHOUT leaving a registered repo behind
// (cleanup() must run).
func TestCreate_SeedModeFailsOnUnreachableRemote(t *testing.T) {
	dir := t.TempDir()
	// Deliberately does not exist: initSeed's probe must report it unreachable.
	url := "file://" + filepath.Join(dir, "does-not-exist.git")
	m := newSeedManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not reachable")
	require.Nil(t, m.Get("seeded"), "an unreachable-remote seed must leave no repo registered")
}

func TestCreate_SeedModeInvalidOntologyYAMLRollsBack(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	url := "file://" + remoteDir
	m := newSeedManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyYAML: "not: [valid ontology yaml",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.Error(t, err)
	require.Nil(t, m.Get("seeded"), "a seed with invalid ontology_yaml must leave no repo registered (cleanup() must run)")
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

// Finding 3 regression, pinned as a pure unit test rather than against a live
// auth-required server: an auth-required remote must be reported as an auth
// failure, NOT as ErrRemoteNotEmpty. ProbeOrigin reports auth-required
// remotes as {Reachable:true, AuthRequired:true, Empty:false}, so checking
// Empty before AuthRequired would send a private-remote seed with a missing
// or wrong token down the ErrRemoteNotEmpty path — the wrong cause, steering
// the caller toward mode "clone", which fails against the exact same missing
// credential.
func TestSeedProbeErr_AuthRequiredCheckedBeforeEmpty(t *testing.T) {
	err := seedProbeErr(ProbeResult{Reachable: true, AuthRequired: true, Empty: false, Detail: "authentication required"})
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrRemoteNotEmpty), "an auth-required remote must not be reported as ErrRemoteNotEmpty")
	require.Contains(t, err.Error(), "authentication required")
}

func TestSeedProbeErr_NotEmptyWithoutAuthIsErrRemoteNotEmpty(t *testing.T) {
	err := seedProbeErr(ProbeResult{Reachable: true, Empty: false})
	require.ErrorIs(t, err, ErrRemoteNotEmpty)
}

func TestSeedProbeErr_UnreachableReportsBeforeEitherOtherFlag(t *testing.T) {
	err := seedProbeErr(ProbeResult{Reachable: false, Detail: "dial: connection refused"})
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrRemoteNotEmpty))
	require.Contains(t, err.Error(), "not reachable")
}

func TestSeedProbeErr_EmptyReachableIsOK(t *testing.T) {
	require.NoError(t, seedProbeErr(ProbeResult{Reachable: true, Empty: true}))
}
