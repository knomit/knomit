package repos

import (
	"context"
	"errors"
	"os"
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

// The dangerous window: the remote is EMPTY when initSeed probes it and has
// refs by the time InitFromRemote actually fetches. initSeed's push uses the
// forced refspec "+refs/heads/%s:refs/heads/%s" (store/remote_sync.go) and a
// freshly-seeded repo has no origin/<branch> tracking ref to trip the
// up-to-date short circuit, so proceeding here would FORCE-OVERWRITE history on
// a remote we do not own — with no local copy of what was destroyed.
//
// The interleaving is driven deterministically, with no race: initSeed's own
// progress callback is synchronous, and it emits Step "ontology" strictly
// AFTER seedProbeErr accepts the probe and strictly BEFORE InitFromRemote runs.
// Pushing from inside that callback reproduces the window exactly, every run.
//
// What must happen: initSeed learns from InitFromRemote's remoteWasEmpty=false
// that the empty path was NOT taken, refuses with ErrRemoteNotEmpty, and never
// reaches the push — so the commit that arrived in the window is still the
// remote's tip afterwards.
func TestCreate_SeedRefusesRemoteThatGainedRefsAfterTheProbe(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	url := "file://" + remoteDir
	m := newSeedManager(t, dir)

	interloper := ""
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, func(e Event) {
		// After the probe said "empty", before InitFromRemote looks.
		if e.Step == "ontology" && interloper == "" {
			pushCommit(t, remoteDir)
			interloper = refHash(t, remoteDir, "refs/heads/main")
		}
	})
	require.NotEmpty(t, interloper, "the interleaving never ran — initSeed's step order changed")
	require.ErrorIs(t, err, ErrRemoteNotEmpty,
		"a remote that gained refs before the fetch must be refused, not force-pushed over")
	require.Nil(t, m.Get("seeded"), "a refused seed must leave no repo registered")

	// The proof that no push happened: the interloper's commit is still the tip,
	// unrewritten, and the seed's agent branch never appeared.
	require.Equal(t, []string{"refs/heads/main"}, remoteRefs(t, remoteDir),
		"initSeed pushed anyway — the remote's refs were rewritten")
	require.Equal(t, interloper, refHash(t, remoteDir, "refs/heads/main"),
		"the remote's history was force-overwritten by the seed push")
}

// refHash returns the commit a ref points at in a bare repo.
func refHash(t *testing.T, bare, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-parse", ref).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
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

// initSeed pushes BEFORE the origin record and the registry entry are durable,
// so a failure in either rolls the local repo back while the remote keeps the
// seed. initSeed's own agent-push failure path explains that state at length;
// the paths on the other side of the mode switch used to return a bare
// "persist origin: …" that named none of it — leaving the user with a remote
// that now refuses every later seed as ErrRemoteNotEmpty and a local copy that
// has just been deleted, and no way to work out why from the message.
//
// The failure is provoked with the one credential-shaped seam that reaches it
// on a file:// remote: a token to persist, on a Manager with no KeyPath, so
// Origins.Set refuses to store the credential (crypt unavailable). The
// mechanism does not matter — what is pinned is that the annotation reaches the
// caller from the far side of the push.
func TestCreate_SeedFailureAfterPushExplainsTheStrandedRemote(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	url := "file://" + remoteDir
	// No Deps.KeyPath, so control.db's Origins tenant has no Crypt to encrypt
	// with and Set refuses the token rather than storing it in plaintext.
	m := newSeedManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url, AuthMethod: "token", AuthToken: "secret"},
	}, nil)
	require.Error(t, err)
	require.Nil(t, m.Get("seeded"), "the failed create must leave no repo registered")

	// The precondition that makes the message necessary: the push landed, so the
	// remote is NOT recoverable by simply retrying the same seed.
	refs := remoteRefs(t, remoteDir)
	require.Contains(t, refs, "refs/heads/main")
	require.Contains(t, refs, "refs/heads/agent/test-abc")

	msg := err.Error()
	require.Contains(t, msg, "persist origin:", "the proximate cause must survive the annotation")
	require.Contains(t, msg, "no longer empty", "the message must say the remote is no longer a valid seed target")
	require.Contains(t, msg, url, "the message must name the remote it stranded")
	require.Contains(t, msg, "clone", "the message must name adopting the remote as one way out")
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

	err := m.CreatePreflight(context.Background(), CreateSpec{Name: "a", Mode: "seed", OntologyPreset: "code"})
	require.Error(t, err, "expected rejection when origin is missing")

	err = m.CreatePreflight(context.Background(), CreateSpec{
		Name: "a", Mode: "seed", Origin: &OriginSpec{URL: "/tmp/x"},
	})
	require.Error(t, err, "expected rejection when ontology is missing")
}

// Regression guard: relaxing clone mode is exactly what mode "seed" exists to
// avoid. This must keep failing.
func TestCreate_CloneModeStillRejectsOntologySpec(t *testing.T) {
	m := newTestManager(t)
	err := m.CreatePreflight(context.Background(), CreateSpec{
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

// CreatePreflight must catch a non-empty seed target BEFORE the caller starts
// streaming. Until it did, ErrRemoteNotEmpty could only be produced inside
// Create — i.e. after the handler had already committed to a 200 NDJSON
// stream — so createErrStatus's ErrRemoteNotEmpty case was dead code and the
// 409 the OpenAPI documents for this case was unreachable.
func TestCreatePreflight_SeedRefusesNonEmptyRemote(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	pushCommit(t, remoteDir)
	m := newSeedManager(t, dir)

	err := m.CreatePreflight(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	})
	require.ErrorIs(t, err, ErrRemoteNotEmpty)
}

// The counterpart: an EMPTY remote must sail through preflight. A guard that
// refused both would turn the 409 into a wall in front of the mode's whole
// reason to exist.
func TestCreatePreflight_SeedAcceptsEmptyRemote(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	m := newSeedManager(t, dir)

	require.NoError(t, m.CreatePreflight(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}))
}

// A probe that could not SEE the remote establishes nothing, so preflight must
// let it through rather than inventing a 409 out of a failed lookup — Create
// then reports the real cause (unreachable) through the stream, which is where
// a recoverable, retryable failure belongs.
func TestCreatePreflight_SeedAllowsUnreachableRemoteThrough(t *testing.T) {
	dir := t.TempDir()
	m := newSeedManager(t, dir)

	require.NoError(t, m.CreatePreflight(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: "file://" + filepath.Join(dir, "does-not-exist.git")},
	}))
}

// A seed whose consensus push lands but whose agent push does not leaves the
// remote HALF seeded, and Create's cleanup then deletes the only local copy.
// Every later seed of that URL hits ErrRemoteNotEmpty and a clone of it yields
// a knowledge base with no agent branch — a dead end the bare push error named
// none of. The error must state the resulting remote state and both ways out.
func TestCreate_SeedPartialPushNamesTheRemoteStateAndRecovery(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	// Reject only the agent branch, so the consensus push succeeds first and
	// the remote is genuinely left half-seeded — the exact state under test.
	hook := filepath.Join(remoteDir, "hooks", "update")
	require.NoError(t, os.WriteFile(hook,
		[]byte("#!/bin/sh\ncase \"$1\" in refs/heads/agent/*) echo 'agent branch refused' >&2; exit 1;; esac\nexit 0\n"), 0o755))

	url := "file://" + remoteDir
	m := newSeedManager(t, dir)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "seeded", Mode: "seed", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "agent/test-abc")
	require.Contains(t, err.Error(), "not the agent branch",
		"the failure must name the state the remote was left in")
	require.Contains(t, err.Error(), "delete and recreate the empty remote",
		"the failure must name the recovery")
	require.Contains(t, err.Error(), `mode "clone"`,
		"the failure must name the other recovery")

	// The state the message describes must be the state that actually exists.
	refs := remoteRefs(t, remoteDir)
	require.Contains(t, refs, "refs/heads/main")
	require.NotContains(t, refs, "refs/heads/agent/test-abc")
	require.Nil(t, m.Get("seeded"), "a failed seed must leave no repo registered")
}

// pushCommit gives a bare repo a single commit on main, so it reads as
// non-empty to a probe.
func pushCommit(t *testing.T, bare string) {
	t.Helper()
	work := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("commit", "--allow-empty", "-m", "init")
	run("remote", "add", "origin", bare)
	run("push", "origin", "main")
}
