package kb

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
	"knomit/internal/foundation/auth"
)

// postResolvePrefer calls the resolver with handles and a prefer list.
func postResolvePrefer(t *testing.T, m *repos.Manager, reach func(string) bool, prefer []string, handles ...string) []resolvedFact {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"handles": handles, "prefer": prefer})
	require.NoError(t, err)
	code, out := postResolveBody(t, m, reach, string(raw))
	require.Equal(t, 200, code)
	return out
}

// collisionManager is alpha and beta holding the SAME short id (the realistic
// case: one KB registered twice), and alpha holding one id of its own.
func collisionManager(t *testing.T) *repos.Manager {
	m, svcs := newRealStoreManager(t, "alpha", "beta")
	writeResolveFact(t, svcs["alpha"], "kb/x/y/dddddddd.md", "observation", "Alpha copy", "a", "0.7")
	writeResolveFact(t, svcs["beta"], "kb/x/y/dddddddd.md", "observation", "Beta copy", "b", "0.7")
	writeResolveFact(t, svcs["alpha"], "kb/x/y/eeeeeeee.md", "observation", "Only alpha", "a", "0.7")
	return m
}

func titles(rs []resolvedFact) map[string]string {
	out := map[string]string{}
	for _, r := range rs {
		out[r.Handle] = r.Repo + ":" + r.Title
	}
	return out
}

// A handle that matches in two repos the caller did not prefer is ambiguous:
// it is OMITTED (stays code), never picked by repo name order. A handle that
// matches in one repo still resolves.
func TestFactsResolve_CollisionWithoutPreferIsOmitted(t *testing.T) {
	m := collisionManager(t)
	got := postResolvePrefer(t, m, nil, nil, "dddddddd", "x/y/dddddddd", "eeeeeeee")
	require.Equal(t, map[string]string{"eeeeeeee": "alpha:Only alpha"}, titles(got))
}

// prefer names the repos the turn worked in: they are searched first, in the
// order given, and the first that holds the handle answers. prefer never
// widens the caller's reach.
func TestFactsResolve_CollisionResolvesInThePreferredRepo(t *testing.T) {
	m := collisionManager(t)
	got := postResolvePrefer(t, m, nil, []string{"beta"}, "dddddddd", "kb/x/y/dddddddd.md", "eeeeeeee")
	require.Equal(t, map[string]string{
		"dddddddd": "beta:Beta copy", "kb/x/y/dddddddd.md": "beta:Beta copy", "eeeeeeee": "alpha:Only alpha",
	}, titles(got))

	got = postResolvePrefer(t, m, nil, []string{"alpha", "beta"}, "dddddddd")
	require.Equal(t, map[string]string{"dddddddd": "alpha:Alpha copy"}, titles(got))
	got = postResolvePrefer(t, m, nil, []string{"beta", "alpha"}, "dddddddd")
	require.Equal(t, map[string]string{"dddddddd": "beta:Beta copy"}, titles(got))

	// A preferred repo by its kb:// id.
	got = postResolvePrefer(t, m, nil, []string{fact.ID12(m.Get("beta").ID())}, "dddddddd")
	require.Equal(t, map[string]string{"dddddddd": "beta:Beta copy"}, titles(got))

	// Preferring a repo the caller cannot read changes nothing: among what it
	// reaches the id is unique.
	onlyAlpha := func(r string) bool { return r == "alpha" }
	got = postResolvePrefer(t, m, onlyAlpha, []string{"beta"}, "dddddddd")
	require.Equal(t, map[string]string{"dddddddd": "alpha:Alpha copy"}, titles(got))
}

// A lens in prefer stands for its members, the write repo first.
func TestFactsResolve_PreferALens(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	for _, name := range []string{"alpha", "beta"} {
		ri := m.Get(name)
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), "kb/technology/x/dddddddd.md",
				"---\ntype: observation\nconfidence: 0.7\n---\n# "+name+" copy\n\nb\n", "add", "")
			require.NoError(t, err)
		}))
	}
	_, err := m.CreateLens(context.Background(), control.Lens{Name: "eng", WriteUID: m.Get("beta").UID(),
		Reads: []control.LensRead{{RepoUID: m.Get("alpha").UID()}}})
	require.NoError(t, err)

	got := postResolvePrefer(t, m, nil, nil, "dddddddd")
	require.Empty(t, got, "fixture: the id must collide without a preference")
	got = postResolvePrefer(t, m, nil, []string{"eng"}, "dddddddd")
	require.Equal(t, map[string]string{"dddddddd": "beta:beta copy"}, titles(got))
}

// The fleet repository is never searched, whatever the reach.
func TestFactsResolve_SkipsTheFleetRepo(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	svc, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	writeResolveFact(t, svc, "kb/x/y/ffffffff.md", "observation", "Fleet fact", "f", "0.7")
	m.Set("fleet", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "fleet", AgentBranch: "agent/test", Svc: svc, Ontology: fact.FleetOntology(),
	}))
	require.True(t, m.Get("fleet").IsFleet(), "fixture: the repo must be the fleet repo")

	got := postResolvePrefer(t, m, nil, []string{"fleet"}, "ffffffff", "kb/x/y/ffffffff.md", "kb://fleet/kb/x/y/ffffffff.md")
	require.Empty(t, got)
}

// Cost: a handle resolved in a preferred repo is not looked for in the others,
// and only the facts RETURNED are read; an omitted collision reads nothing.
func TestFactsResolve_ReadsOnlyWhatItReturns(t *testing.T) {
	m := collisionManager(t)
	var lookups []string
	var reads []string
	origLookup, origRead := lookupIndex, readResolved
	t.Cleanup(func() { lookupIndex, readResolved = origLookup, origRead })
	lookupIndex = func(ctx context.Context, ri *repos.RepoInstance, branch string, opts index.SearchOptions) ([]index.RecentFactEntry, int, error) {
		lookups = append(lookups, ri.Name())
		return origLookup(ctx, ri, branch, opts)
	}
	readResolved = func(ctx context.Context, ri *repos.RepoInstance, a hal.Anchor, path string, fb bool) (fact.Fact, string, error) {
		reads = append(reads, ri.Name()+":"+path)
		return origRead(ctx, ri, a, path, fb)
	}

	got := postResolvePrefer(t, m, nil, []string{"beta"}, "dddddddd")
	require.Len(t, got, 1)
	require.Equal(t, []string{"beta"}, lookups, "resolved in the preferred repo: alpha is not searched")
	require.Equal(t, []string{"beta:kb/x/y/dddddddd.md"}, reads)

	lookups, reads = nil, nil
	got = postResolvePrefer(t, m, nil, nil, "dddddddd")
	require.Empty(t, got)
	require.ElementsMatch(t, []string{"alpha", "beta"}, lookups)
	require.Empty(t, reads, "an ambiguous handle is not read anywhere")
}

// A request whose context is done answers nothing more: no repo is searched.
func TestFactsResolve_StopsOnCancel(t *testing.T) {
	m := collisionManager(t)
	searched := 0
	orig := lookupIndex
	t.Cleanup(func() { lookupIndex = orig })
	lookupIndex = func(ctx context.Context, ri *repos.RepoInstance, branch string, opts index.SearchOptions) ([]index.RecentFactEntry, int, error) {
		searched++
		return orig(ctx, ri, branch, opts)
	}
	ctx, cancel := context.WithCancel(auth.WithRepoReach(context.Background(), auth.AllRepos))
	cancel()
	out := resolveHandles(ctx, m, "kb", []string{"eeeeeeee"}, nil)
	require.Empty(t, out)
	require.Zero(t, searched)
}

// A path handle is lowercased after the ontology root, as the store stores it.
func TestFactsResolve_LowercasesThePath(t *testing.T) {
	m := collisionManager(t)
	got := postResolvePrefer(t, m, nil, nil, "X/Y/EEEEEEEE", "kb/X/y/eeeeeeee.md")
	require.Equal(t, map[string]string{"X/Y/EEEEEEEE": "alpha:Only alpha", "kb/X/y/eeeeeeee.md": "alpha:Only alpha"}, titles(got))
}
