package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// newRemoteModeManager returns a started Manager rooted at dir with
// LocalOriginRoot set to dir, so file:// origin fixtures created under dir are
// permitted — newTestManager (this package's default fixture) leaves
// LocalOriginRoot unset, which disables filesystem origins entirely, and every
// test here needs a file:// remote.
func newRemoteModeManager(t *testing.T, dir string) *Manager {
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

// refHash returns the commit a ref points at in a bare repo.
func refHash(t *testing.T, bare, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-parse", ref).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// bareOf turns the file:// URL these fixtures return back into a path, so the
// git plumbing helpers above can be pointed at the same repo.
func bareOf(url string) string { return strings.TrimPrefix(url, "file://") }

// ── the mode's whole point ────────────────────────────────────────────────
//
// A remote with a branch and no ontology becomes a knowledge base WITHOUT the
// consensus branch being touched. That second half is not a nicety: pushing
// the consensus branch is what made the deleted "seed" mode fail outright on
// every host that protects the default branch of a new project, which is all
// of them by default.
func TestCreate_InitializeWritesOntologyOnTheAgentBranchOnly(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	bare := bareOf(url)
	mainBefore := refHash(t, bare, "refs/heads/main")

	m := newRemoteModeManager(t, dir)
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "source-code", ri.Ontology().ID,
		"the repo must carry the ontology the caller chose, not a default")

	// The ontology landed on the agent branch, on the remote.
	refs := remoteRefs(t, bare)
	require.Contains(t, refs, "refs/heads/agent/test-abc",
		"initialize must PUSH the agent branch — the store's InitFromRemote only ever writes locally")
	require.Contains(t, refs, "refs/heads/main")

	// THE LOAD-BEARING ASSERTION: the consensus branch is byte-identical.
	require.Equal(t, mainBefore, refHash(t, bare, "refs/heads/main"),
		"initialize must never write to the consensus branch — that push is what needed protected-branch access")

	// And the agent branch genuinely carries the file, not merely a commit.
	require.Equal(t, "1", gitCount(t, bare, "agent/test-abc", OntologyPath),
		"the agent branch must carry %s", OntologyPath)

	// The origin record must hold the RESOLVED upstream, never the empty string
	// the spec requested — the same invariant initClone's doc comment pins.
	org, oerr := m.Origins().Get(ri.UID())
	require.NoError(t, oerr)
	require.NotNil(t, org, "an initialized repo must have a persisted origin record")
	require.Equal(t, url, org.URL)
	require.Equal(t, "main", org.Branch)
}

// gitCount reports how many entries at path exist in branch's tip tree, as a
// string ("1" when present, "0" when not) so the assertion message reads well.
func gitCount(t *testing.T, bare, branch, path string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "ls-tree", "--name-only", branch, path).Output()
	require.NoError(t, err)
	if strings.TrimSpace(string(out)) == "" {
		return "0"
	}
	return "1"
}

// Identity is the remote's EXISTING root commit, so two machines initializing
// the same remote agree about which knowledge base it is. The deleted seed mode
// minted a nonce per machine instead, which is the split-brain race documented
// at store/repo.go's initFromEmptyRemote — two machines, one remote, two
// identities, and no push-time signal that they had diverged.
func TestCreate_InitializeTakesIdentityFromTheRemoteRootCommit(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	rootCommit := strings.TrimSpace(gitOut(t, bareOf(url), "rev-list", "--max-parents=0", "main"))

	m := newRemoteModeManager(t, dir)
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "default",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, rootCommit, ri.ID(),
		"the repo id must be the remote's own root commit, so a second machine agrees")
}

func gitOut(t *testing.T, bare string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", bare}, args...)...).Output()
	require.NoError(t, err)
	return string(out)
}

// ── refusals ──────────────────────────────────────────────────────────────

// THE BUG THIS PLAN EXISTS TO CLOSE. Creating the repository with a README made
// it non-empty, so the wizard routed to clone; clone refused the ontology the
// user had chosen and then let repoBuilder.loadOntology silently substitute
// fact.DefaultOntology() at the next open. They picked "Code", got "General",
// and were never told — permanently, since the ontology is immutable after
// creation.
//
// Refusing at CREATE is the fix. The open-path fallback deliberately stays as
// it is: it serves repos that already exist, and hard-failing there would
// strand exactly the users this bug already hurt.
func TestCreate_CloneRefusesARemoteThatIsNotAKnowledgeBase(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "clone",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.ErrorIs(t, err, ErrRemoteNotInitialized,
		"a remote with no ontology must be REFUSED, never defaulted")
	require.Contains(t, err.Error(), "initialize", "the refusal must name the mode that would have worked")
	require.Nil(t, m.Get("kb"), "a refused clone must leave no repo registered")
}

// The mirror: initialize must not write a second ontology over the one that
// already governs the branch. Same stake — the ontology is immutable, so a
// wrong one here is not correctable later.
func TestCreate_InitializeRefusesARemoteThatIsAlreadyAKnowledgeBase(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git")) // WITH an ontology
	m := newRemoteModeManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.ErrorIs(t, err, ErrRemoteAlreadyInitialized)
	require.Contains(t, err.Error(), "clone", "the refusal must name the mode that would have worked")
	require.Nil(t, m.Get("kb"), "a refused initialize must leave no repo registered")
}

// A remote with NO branches has nothing to cut an agent branch from, and knomit
// never creates a branch on a remote other than its own. The message must be
// the one instruction that fixes it, not a diagnosis.
func TestCreate_InitializeRefusesAnEmptyRemote(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	url := "file://" + remoteDir
	m := newRemoteModeManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.ErrorIs(t, err, ErrRemoteNoBranches)
	require.Contains(t, err.Error(), "one commit is enough",
		"the refusal must tell the user how to fix it, not merely name the state")
	require.Nil(t, m.Get("kb"), "a refused initialize must leave no repo registered")
	require.Nil(t, remoteRefs(t, remoteDir), "a refused initialize must not write to the remote")
}

// Clone mode meets the same empty remote: it too must refuse rather than take
// InitFromRemote's empty path, which mints a fresh root commit and therefore a
// repo identity no other machine shares.
func TestCreate_CloneRefusesAnEmptyRemote(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	m := newRemoteModeManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.ErrorIs(t, err, ErrRemoteNoBranches)
	require.Nil(t, m.Get("kb"), "a refused clone must leave no repo registered")
}

// A failed agent push must leave the remote BYTE-IDENTICAL and add no repo.
// Nothing is stranded: the consensus branch was never a target, so this is
// simply retryable — which is the improvement over seed, whose consensus push
// landed first and left a half-seeded remote that refused every later attempt.
func TestCreate_InitializeFailedPushLeavesTheRemoteUntouched(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	bare := bareOf(url)
	// Refuse exactly the branch initialize pushes — the same shape a host's
	// branch protection takes, aimed at the one ref knomit actually writes.
	hook := filepath.Join(bare, "hooks", "update")
	require.NoError(t, os.WriteFile(hook,
		[]byte("#!/bin/sh\ncase \"$1\" in refs/heads/agent/*) echo 'agent branch refused' >&2; exit 1;; esac\nexit 0\n"), 0o755))
	before := remoteRefs(t, bare)
	mainBefore := refHash(t, bare, "refs/heads/main")

	m := newRemoteModeManager(t, dir)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "agent/test-abc", "the failure must name the branch that was refused")

	require.Equal(t, before, remoteRefs(t, bare), "a failed initialize must not change the remote's refs")
	require.Equal(t, mainBefore, refHash(t, bare, "refs/heads/main"))
	require.Nil(t, m.Get("kb"), "a failed initialize must leave no repo registered")
}

// Create is reachable directly (tests, future CLI paths), bypassing
// CreatePreflight entirely, so initInitialize must re-assert the
// "ontology required" rule itself rather than trust the preflight check —
// mirroring initClone's own authoritative re-assertion of
// rejectOntologySpecForClone. Without it, Create(initialize, no ontology
// fields) would silently write fact.DefaultOntology().
func TestCreate_InitializeRequiresOntology_AuthoritativeInCreate(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.ErrorIs(t, err, ErrInvalidName)
	require.Nil(t, m.Get("kb"), "an initialize with no ontology must leave no repo registered")
}

func TestCreate_InitializeFailsOnUnreachableRemote(t *testing.T) {
	dir := t.TempDir()
	url := "file://" + filepath.Join(dir, "does-not-exist.git")
	m := newRemoteModeManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not reachable")
	require.Nil(t, m.Get("kb"), "an unreachable-remote initialize must leave no repo registered")
}

func TestCreate_InitializeInvalidOntologyYAMLRollsBack(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyYAML: "not: [valid ontology yaml",
		Origin: &OriginSpec{URL: url},
	}, nil)
	require.Error(t, err)
	require.Nil(t, m.Get("kb"),
		"an initialize with invalid ontology_yaml must leave no repo registered (cleanup() must run)")
	require.NotContains(t, remoteRefs(t, bareOf(url)), "refs/heads/agent/test-abc",
		"a create that failed before the push must not have pushed")
}

// ── preflight ─────────────────────────────────────────────────────────────

func TestCreatePreflight_InitializeRequiresOriginAndOntology(t *testing.T) {
	m := newTestManager(t)

	err := m.CreatePreflight(context.Background(), CreateSpec{Name: "a", Mode: "initialize", OntologyPreset: "code"})
	require.Error(t, err, "expected rejection when origin is missing")

	err = m.CreatePreflight(context.Background(), CreateSpec{
		Name: "a", Mode: "initialize", Origin: &OriginSpec{URL: "/tmp/x"},
	})
	require.Error(t, err, "expected rejection when ontology is missing")
}

// CreatePreflight must catch a branch-less remote BEFORE the caller starts
// streaming, or ErrRemoteNoBranches could only ever arrive as a
// {"type":"error"} line inside an already-committed 200 stream and the
// documented 409 would be unreachable.
func TestCreatePreflight_InitializeRefusesAnEmptyRemote(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	m := newRemoteModeManager(t, dir)

	err := m.CreatePreflight(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "code",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	})
	require.ErrorIs(t, err, ErrRemoteNoBranches)
}

// The counterpart: a remote WITH a branch must sail through preflight. A guard
// that refused both would be a wall in front of the mode's whole reason to
// exist.
func TestCreatePreflight_InitializeAcceptsARemoteWithABranch(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	require.NoError(t, m.CreatePreflight(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "code",
		Origin: &OriginSpec{URL: url},
	}))
}

// A probe that could not SEE the remote establishes nothing, so preflight must
// let it through rather than inventing a 409 out of a failed lookup — Create
// then reports the real cause (unreachable) through the stream, which is where
// a recoverable, retryable failure belongs.
func TestCreatePreflight_InitializeAllowsUnreachableRemoteThrough(t *testing.T) {
	dir := t.TempDir()
	m := newRemoteModeManager(t, dir)

	require.NoError(t, m.CreatePreflight(context.Background(), CreateSpec{
		Name: "kb", Mode: "initialize", OntologyPreset: "code",
		Origin: &OriginSpec{URL: "file://" + filepath.Join(dir, "does-not-exist.git")},
	}))
}

// Regression guard: relaxing clone mode is exactly what the initialize mode
// exists to avoid. A clone that accepted an ontology would apply it only when
// the remote happened not to have one, which is a request obeyed half the time.
func TestCreate_CloneModeStillRejectsOntologySpec(t *testing.T) {
	m := newTestManager(t)
	err := m.CreatePreflight(context.Background(), CreateSpec{
		Name: "c", Mode: "clone", OntologyPreset: "code",
		Origin: &OriginSpec{URL: "/tmp/x"},
	})
	require.Error(t, err, "clone mode accepted an ontology spec — rejectOntologySpecForClone was weakened")
}

// ── initializeProbeErr ────────────────────────────────────────────────────

// AuthRequired must be read BEFORE Empty. ProbeOrigin reports an auth-required
// remote as {Reachable:true, AuthRequired:true, Empty:false} — it has no way to
// know whether a remote it cannot authenticate against has branches. Checking
// Empty first would tell a user with a wrong token to go create a branch they
// already have.
func TestInitializeProbeErr_AuthRequiredCheckedBeforeEmpty(t *testing.T) {
	err := initializeProbeErr(ProbeResult{Reachable: true, AuthRequired: true, Empty: true, Detail: "authentication required"})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRemoteNoBranches, "an auth-required remote must not be reported as branch-less")
	require.Contains(t, err.Error(), "authentication required")
}

func TestInitializeProbeErr_EmptyWithoutAuthIsErrRemoteNoBranches(t *testing.T) {
	require.ErrorIs(t, initializeProbeErr(ProbeResult{Reachable: true, Empty: true}), ErrRemoteNoBranches)
}

func TestInitializeProbeErr_UnreachableReportsBeforeEitherOtherFlag(t *testing.T) {
	err := initializeProbeErr(ProbeResult{Reachable: false, Empty: true, Detail: "dial: connection refused"})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRemoteNoBranches)
	require.Contains(t, err.Error(), "not reachable")
}

func TestInitializeProbeErr_ReachableWithBranchesIsOK(t *testing.T) {
	require.NoError(t, initializeProbeErr(ProbeResult{Reachable: true, Empty: false}))
}

// The other half of the re-create dead end: once the probe answers "yes" for a
// remote this machine already initialized, the wizard derives mode "clone" —
// and that mode has to actually work here. It does, because initClone runs its
// ontology check against the same adopted branch the probe now inspects.
//
// Without this, fixing the probe would only move the dead end one step later.
func TestCreate_CloneMode_AdoptsThisMachinesExistingAgentBranch(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManagerWithRoot(t, root)
	url := seedRemoteInitializedByThisMachine(t, filepath.Join(root, "remote.git"), m.deps.AgentBranch)

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "rejoined", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"},
	}, func(Event) {})
	require.NoError(t, err,
		"re-creating a repo this machine initialized must JOIN its own agent branch, not refuse")
	require.NotNil(t, ri)
	require.NotNil(t, ri.Ontology())
}

// THE CHOKE POINT.
//
// The handlers refuse a remote governed by a different ontology before they
// write anything, which is how a caller gets a 409 naming both taxonomies. But
// a guarantee that lives only in handlers is one a future attach path forgets —
// which is exactly the shape of the bug it was added for, since Create enforced
// the invariant and the attach paths did not.
//
// So ActivateSync, which EVERY path that points a repo at a remote calls,
// re-asserts it itself. This test goes around the handlers entirely.
func TestActivateSync_RefusesARemoteGovernedByADifferentOntology(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManagerWithRoot(t, root)

	// A repo on the code taxonomy...
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "preset", OntologyPreset: "code",
	}, func(Event) {})
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.Equal(t, "source-code", ri.Ontology().ID)

	// ...pointed at a knowledge base on a different one, with the origin
	// written straight into the store so no handler is involved.
	url := seedBareRemote(t, filepath.Join(root, "remote.git")) // carries the DEFAULT ontology
	svc := testService(t, ri)
	svc.SetOrigin(&store.Origin{URL: url, Branch: "main"})
	require.NoError(t, svc.ConfigureRemote(url, "main", ri.AgentBranch()))

	err = ri.ActivateSync(url)
	require.Error(t, err, "sync must not start against a remote that would overwrite this repo's taxonomy")
	require.ErrorIs(t, err, ErrOriginOntologyConflict)
}

// And the case that must keep working: the SAME knowledge base. Re-attaching a
// repo to its own remote is the ordinary path after a machine is rebuilt, and
// a gate that refused it would be worse than the hole it closes.
func TestActivateSync_AllowsTheSameKnowledgeBase(t *testing.T) {
	root := t.TempDir()
	m := newLifecycleManagerWithRoot(t, root)
	url := seedBareRemote(t, filepath.Join(root, "remote.git")) // the DEFAULT ontology

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"},
	}, func(Event) {})
	require.NoError(t, err)
	require.NotNil(t, ri)

	require.NoError(t, ri.ActivateSync(url),
		"a repo must be able to sync with the knowledge base it came from")
}
