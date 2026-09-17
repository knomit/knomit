package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
)

// newSubscribeTestManager builds Deps inline because these tests need BOTH a
// LocalOriginRoot (the file:// remotes below) and DisableBackgroundSync — the
// never-pushes test depends on ActivateSync running one synchronous reconcile
// and starting no loop. newLifecycleManagerWithRoot sets only the first,
// newTestManager only the second.
func newSubscribeTestManager(t *testing.T, root string) *Manager {
	t.Helper()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestCreate_SubscribeMode_FollowsUpstreamReadOnly(t *testing.T) {
	root := t.TempDir()
	m := newSubscribeTestManager(t, root)
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	var steps []string
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url},
	}, func(e Event) { steps = append(steps, e.Step) })
	require.NoError(t, err)
	require.Contains(t, steps, "subscribe")
	require.Contains(t, steps, "done")

	require.True(t, ri.Subscribed())
	require.Equal(t, "", ri.AgentBranch())
	require.Equal(t, "main", ri.ReadBranch(), "resolved upstream, not a requested name")
	require.NotEmpty(t, ri.ID())
	require.False(t, ri.WritableBranch("main"))

	// Persisted as a subscription with the RESOLVED upstream.
	o, err := m.Origins().Get(ri.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, OriginModeSubscribe, o.Mode)
	require.Equal(t, "main", o.Branch)
	require.Equal(t, "sub", m.ActiveRepoWithOrigin(url))
}

func TestCreate_SubscribeMode_RefusesOntologyAndNonKB(t *testing.T) {
	root := t.TempDir()
	m := newSubscribeTestManager(t, root)

	kb := seedBareRemote(t, filepath.Join(root, "kb.git"))
	err := m.CreatePreflight(context.Background(), CreateSpec{
		Name: "a", Mode: "subscribe", OntologyPreset: "default", Origin: &OriginSpec{URL: kb},
	})
	require.ErrorIs(t, err, ErrInvalidName, "a subscription takes the remote's ontology; supplying one is refused")

	plain := seedBareRemoteNoOntology(t, filepath.Join(root, "plain.git"))
	err = m.CreatePreflight(context.Background(), CreateSpec{
		Name: "b", Mode: "subscribe", Origin: &OriginSpec{URL: plain, Branch: "main"},
	})
	require.ErrorIs(t, err, ErrRemoteNotInitialized)
	_, err = m.Create(context.Background(), CreateSpec{
		Name: "b", Mode: "subscribe", Origin: &OriginSpec{URL: plain, Branch: "main"},
	}, nil)
	require.ErrorIs(t, err, ErrRemoteNotInitialized)
	require.Nil(t, m.Get("b"))
}

// The preflight inspects the CONSENSUS branch for a subscription, never this
// machine's agent branch — a remote where only agent/<host> is a knowledge
// base is not subscribable.
func TestProbeInitializedOn_InspectsNamedBranch(t *testing.T) {
	root := t.TempDir()
	m := newSubscribeTestManager(t, root)
	plain := seedBareRemoteNoOntology(t, filepath.Join(root, "plain.git"))
	// Push an agent branch for THIS machine that does carry an ontology.
	work := t.TempDir()
	runGit(t, "", "clone", plainPath(plain), work)
	runGit(t, work, "checkout", "-b", m.deps.AgentBranch)
	ont, oerr := fact.DefaultOntology().Serialize()
	require.NoError(t, oerr)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "agent kb")
	runGit(t, work, "push", "origin", m.deps.AgentBranch)

	viaCreateRule, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: plain, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedYes, viaCreateRule.Initialized, "clone would adopt the agent branch")

	onMain, err := m.ProbeInitializedOn(context.Background(), OriginSpec{URL: plain, Branch: "main"}, "main")
	require.NoError(t, err)
	require.Equal(t, InitializedNo, onMain.Initialized)
	require.Equal(t, "main", onMain.Branch)
}

// After the upstream advances, one sync moves the read branch and nothing is
// pushed: the remote's ref set is unchanged.
func TestSubscription_SyncFollowsUpstreamAndNeverPushes(t *testing.T) {
	root := t.TempDir()
	m := newSubscribeTestManager(t, root)
	bare := filepath.Join(root, "remote.git")
	url := seedBareRemote(t, bare)
	ri, err := m.Create(context.Background(), CreateSpec{Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url}}, nil)
	require.NoError(t, err)

	var before string
	require.NoError(t, ri.WithRead(func(s *store.Service) {
		before, _ = s.Branches().HeadCommit(context.Background(), "main")
	}))
	require.NotEmpty(t, before)

	// Advance the remote's main.
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "more.txt"), []byte("more"), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "advance")
	runGit(t, work, "push", "origin", "main")

	refsBefore := gitRefs(t, bare)
	require.NoError(t, ri.ActivateSync(url)) // one synchronous reconcile; no loop under DisableBackgroundSync

	var after string
	require.NoError(t, ri.WithRead(func(s *store.Service) {
		after, _ = s.Branches().HeadCommit(context.Background(), "main")
	}))
	require.NotEqual(t, before, after, "read branch followed the upstream")
	require.Equal(t, refsBefore, gitRefs(t, bare), "a subscription never pushes: the remote's refs are untouched")
}

// gitRefs returns `git show-ref` output for a bare repo.
func gitRefs(t *testing.T, bare string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "show-ref").CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

// plainPath turns the file: URL the seed helpers return back into a path.
// Not a TrimPrefix: the URL is percent-escaped, and on Windows it carries a
// slash before the drive letter that no OS call accepts.
func plainPath(u string) string {
	p, ok := fileuri.Path(u)
	if !ok {
		panic("plainPath: not a file URL: " + u)
	}
	return p
}

// seedBareRemoteHeadNotMain builds a remote whose HEAD is an ontology-LESS
// "develop", alongside a "main" that IS a knowledge base. The two differ, which
// is the only way to tell whether a caller resolves the branch by the
// prefer-main rule or just follows HEAD.
func seedBareRemoteHeadNotMain(t *testing.T, bare string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=develop", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)

	// develop: an ordinary branch, no ontology.
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed"), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "develop seed")
	runGit(t, work, "push", "origin", "develop")

	// main: the knowledge base.
	runGit(t, work, "checkout", "-b", "main")
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "main kb")
	runGit(t, work, "push", "origin", "main")

	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/develop")
	return fileuri.New(bare)
}

// With no branch requested, the preflight must resolve the branch by the SAME
// prefer-main rule the create uses (store.resolveUpstream), not by the remote's
// HEAD. Otherwise a remote whose HEAD is not main but whose main is a knowledge
// base gets a confident, wrong "not a knowledge base" — and the create that
// follows would have succeeded.
func TestCreate_SubscribeMode_PreflightResolvesByTheCreateRule(t *testing.T) {
	root := t.TempDir()
	m := newSubscribeTestManager(t, root)
	url := seedBareRemoteHeadNotMain(t, filepath.Join(root, "headnotmain.git"))

	require.NoError(t,
		m.CreatePreflight(context.Background(), CreateSpec{
			Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url},
		}),
		"main carries the ontology, so the preflight must not refuse")

	// The inverse, checked BEFORE the create below: naming the ontology-less
	// branch explicitly is still refused. (After the create it could not be
	// checked at all — the origin-in-use gate fires first and would mask this.)
	require.ErrorIs(t,
		m.CreatePreflight(context.Background(), CreateSpec{
			Name: "sub2", Mode: "subscribe", Origin: &OriginSpec{URL: url, Branch: "develop"},
		}),
		ErrRemoteNotInitialized,
		"an explicitly named ontology-less branch is still refused")

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "main", ri.ReadBranch(), "the create adopts main, not the remote's HEAD")
}

// seedBareRemoteMasterOnly builds a knowledge base on "master" with no "main"
// anywhere — the case where prefer-main has nothing to prefer and both rules
// must fall through to the remote's HEAD.
func seedBareRemoteMasterOnly(t *testing.T, bare string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=master", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	runGit(t, work, "checkout", "-B", "master")
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "master kb")
	runGit(t, work, "push", "origin", "master")
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/master")
	return fileuri.New(bare)
}

// The preflight and the create resolve the branch through TWIN rules that live
// in different packages and cannot share an implementation: the preflight uses
// resolveUpstream in probe.go (from an ls-remote listing), the create uses
// store.resolveUpstream in store/repo.go (from a fetched repo). Nothing else
// pins them together, and Task 7 shipped a bug that existed precisely because
// they disagreed.
//
// This asserts they agree on every shape that distinguishes them: the branch
// the preflight INSPECTS is the branch the create ADOPTS. The inspected branch
// comes from subscribeInspectBranch — the same function CreatePreflight calls —
// so the test cannot pass by re-deriving the rule it is checking.
func TestSubscribe_PreflightInspectsTheBranchTheCreateAdopts(t *testing.T) {
	cases := []struct {
		name     string
		seed     func(*testing.T, string) string
		branch   string // explicitly requested, empty for "let it resolve"
		wantRead string
	}{
		{"head is main", seedBareRemote, "", "main"},
		{"head is develop, main carries the ontology", seedBareRemoteHeadNotMain, "", "main"},
		{"master-only remote", seedBareRemoteMasterOnly, "", "master"},
		{"explicit master on a master-only remote", seedBareRemoteMasterOnly, "master", "master"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			m := newSubscribeTestManager(t, root)
			url := tc.seed(t, filepath.Join(root, "remote.git"))
			spec := CreateSpec{Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url, Branch: tc.branch}}

			// What the preflight will inspect, via the production helper.
			probe, perr := m.ProbeOriginRefs(context.Background(), *spec.Origin)
			require.NoError(t, perr)
			usable := probe.Reachable && !probe.AuthRequired
			require.True(t, usable, "fixture must be reachable for this test to mean anything")
			inspect := subscribeInspectBranch(spec, probe, usable)
			require.Equal(t, tc.wantRead, inspect, "preflight inspects the wrong branch")

			// The preflight accepts it, and the create adopts the same branch.
			require.NoError(t, m.CreatePreflight(context.Background(), spec))
			ri, err := m.Create(context.Background(), spec, nil)
			require.NoError(t, err)
			require.Equal(t, inspect, ri.ReadBranch(),
				"the create adopted a different branch than the preflight inspected")
			require.Equal(t, tc.wantRead, ri.ReadBranch())
		})
	}
}
