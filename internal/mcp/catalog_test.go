package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// catalogOf calls knomit_catalog and decodes its envelope.
func catalogOf(t *testing.T, m *repos.Manager, ctx context.Context) map[string]any {
	t.Helper()
	res, err := CatalogHandler(m)(ctx, mcpgo.CallToolRequest{})
	require.NoError(t, err)
	require.NotNil(t, res)
	text := resultText(t, res)
	require.False(t, res.IsError, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), "payload: %s", text)
	return out
}

func TestCatalog_ListsReposAndLenses(t *testing.T) {
	m, _, _ := bindFixture(t) // alpha + beta writable, followed subscribed, lens "eng"

	out := catalogOf(t, m, context.Background())

	reposList, _ := out["repos"].([]any)
	require.Len(t, reposList, 3)
	// Sorted by name: alpha, beta, followed.
	names := []string{}
	for _, r := range reposList {
		names = append(names, r.(map[string]any)["name"].(string))
	}
	require.Equal(t, []string{"alpha", "beta", "followed"}, names)

	alpha := reposList[0].(map[string]any)
	require.Equal(t, "writable", alpha["mode"])
	require.Equal(t, "agent/test", alpha["agent_branch"])
	require.Equal(t, "agent/test", alpha["read_branch"])
	require.Equal(t, "kb", alpha["ontology_root"])
	require.Equal(t, "code", alpha["profile"])
	// The id is the 12-hex root-commit addressing id, never the registry uid.
	id, _ := alpha["id"].(string)
	require.Len(t, id, 12)
	require.NotEqual(t, "uid-alpha", id)

	followed := reposList[2].(map[string]any)
	require.Equal(t, "subscribe", followed["mode"])
	require.Equal(t, "main", followed["read_branch"])
	require.NotContains(t, followed, "agent_branch", "a subscription has none")

	lenses, _ := out["lenses"].([]any)
	require.Len(t, lenses, 1)
	eng := lenses[0].(map[string]any)
	require.Equal(t, "eng", eng["name"])
	require.Equal(t, "alpha", eng["write"], "write repo by NAME, not uid")
	mounts, _ := eng["mounts"].([]any)
	require.Len(t, mounts, 2)
	// Sorted by repo name, and an empty stored branch shows the member's
	// ReadBranch() at resolve time.
	require.Equal(t, "alpha", mounts[0].(map[string]any)["repo"])
	require.Equal(t, "agent/test", mounts[0].(map[string]any)["branch"])
	require.Equal(t, "beta", mounts[1].(map[string]any)["repo"])

	// No uid may appear anywhere in the payload.
	raw, _ := json.Marshal(out)
	require.NotContains(t, string(raw), "uid-", "registry uids must never be exposed")
}

// The whole point: it answers with no binding in the context at all.
func TestCatalog_WorksUnbound(t *testing.T) {
	m, _, _ := bindFixture(t)

	out := catalogOf(t, m, repos.WithSessionScoped(context.Background()))

	require.NotContains(t, out, "bound", "unbound sessions get no bound key")
	reposList, _ := out["repos"].([]any)
	require.Len(t, reposList, 3)
}

func TestCatalog_ReportsBound(t *testing.T) {
	m, _, _ := bindFixture(t)
	ri := m.Get("alpha")
	require.NotNil(t, ri)

	ctx := repos.WithBinding(repos.WithSessionScoped(context.Background()),
		repos.NewBindingOfRepo(ri, ""))
	out := catalogOf(t, m, ctx)

	bound, ok := out["bound"].(map[string]any)
	require.True(t, ok, "payload: %v", out)
	require.Equal(t, "repo", bound["kind"])
	require.Equal(t, "alpha", bound["name"])
}

func TestCatalog_ReportsBoundLens(t *testing.T) {
	m, _, _ := bindFixture(t)
	l, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	b, err := repos.NewBindingOfLens(m, l)
	require.NoError(t, err)

	out := catalogOf(t, m, repos.WithBinding(context.Background(), b))

	bound, _ := out["bound"].(map[string]any)
	require.Equal(t, "lens", bound["kind"])
	require.Equal(t, "eng", bound["name"])
}

// A repo registered but with no live instance is still listed, with the reason
// a bind would fail — mirroring the REST list, which keeps broken repos visible.
//
// Built through the PRODUCTION path rather than a test backdoor: create a real
// repo, delete its store file, and start a second manager over the same home,
// which is exactly how openRegistered comes to call markUnavailable.
func TestCatalog_UnavailableRepoListed(t *testing.T) {
	home := t.TempDir()
	newMgr := func() *repos.Manager {
		m := repos.New(context.Background(), repos.Deps{
			Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
			AgentBranch: "agent/test",
		})
		t.Cleanup(func() { _ = m.Close() })
		return m
	}

	m := newMgr()
	require.NoError(t, m.Start())
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "core", Mode: "preset"}, nil)
	require.NoError(t, err)
	uid := ri.UID()
	path := m.RepoPath(uid)
	require.NoError(t, m.Close())
	require.NoError(t, os.Remove(path))

	m2 := newMgr()
	require.NoError(t, m2.Start())
	require.Nil(t, m2.Get("core"), "no live instance for a missing file")

	out := catalogOf(t, m2, context.Background())

	reposList, _ := out["repos"].([]any)
	var broken map[string]any
	for _, r := range reposList {
		if r.(map[string]any)["name"] == "core" {
			broken = r.(map[string]any)
		}
	}
	require.NotNil(t, broken, "an unavailable repo must still be listed")
	require.Equal(t, "unavailable", broken["status"])
	require.Equal(t, "missing", broken["reason"])
	require.NotEmpty(t, broken["detail"])
	// No id key at all — not "" and certainly not the registry uid. The id
	// exists only once the store has opened, which this repo's never did.
	require.NotContains(t, broken, "id")
	raw, _ := json.Marshal(broken)
	require.NotContains(t, string(raw), uid, "the registry uid must not leak as a fallback id")
}

// The catalogue is a cheap index: the fields it reports come from memory and
// the control-plane registry, so a repo whose STORE is dead is still listed
// with its name, mode, branches, ontology root and profile intact.
//
// Asserting only len(repos)==3 would be self-deceiving: it passes precisely
// BECAUSE a failed id read is swallowed, so it would keep passing if someone
// added a genuine per-repo store read whose failure is also swallowed. These
// assertions name the fields that must survive, and check the OTHER repos still
// carry ids so a blanket id failure cannot masquerade as success.
func TestCatalog_NoPerRepoStoreRead(t *testing.T) {
	m, _, _ := bindFixture(t)
	ri := m.Get("alpha")
	require.NotNil(t, ri)
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	release()
	require.NoError(t, svc.Close()) // any per-repo store read would now fail

	out := catalogOf(t, m, context.Background())
	reposList, _ := out["repos"].([]any)
	require.Len(t, reposList, 3, "listing must not depend on any repo's store")

	byName := map[string]map[string]any{}
	for _, r := range reposList {
		row := r.(map[string]any)
		byName[row["name"].(string)] = row
	}

	// The dead-store repo keeps every memory- and registry-resident field.
	alpha := byName["alpha"]
	require.Equal(t, "writable", alpha["mode"])
	require.Equal(t, "agent/test", alpha["agent_branch"])
	require.Equal(t, "agent/test", alpha["read_branch"])
	require.Equal(t, "kb", alpha["ontology_root"])
	require.Equal(t, "code", alpha["profile"], "profile comes from control.db, not the repo store")

	// And the repos whose stores are still open keep their ids, so this test
	// cannot pass by everything failing at once.
	for _, name := range []string{"beta", "followed"} {
		id, _ := byName[name]["id"].(string)
		require.Len(t, id, 12, "%s should still carry its 12-hex id", name)
	}
}

// REGRESSION, and the reason the callback only snapshots: ForEach holds
// m.mu.RLock for the whole iteration, so any mgr call inside it takes m.mu a
// second time. sync.RWMutex is not reentrant and Go blocks new readers once a
// writer is pending, so a concurrent write wedges the Manager permanently —
// from an unauthenticated, binding-free tool call.
//
// This drives knomit_catalog while another goroutine repeatedly takes the
// manager's WRITE lock, and fails on a timeout rather than hanging the suite.
func TestCatalog_NoDeadlockUnderConcurrentWriter(t *testing.T) {
	m, _, _ := bindFixture(t)
	// Two REAL instances: the point is write-lock contention, not the nil-entry
	// quirk of Set(name, nil), which no production path uses.
	spareA := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "churn", UID: "uid-churn-a", AgentBranch: "agent/test", OntologyRoot: "kb",
	})
	spareB := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "churn", UID: "uid-churn-b", AgentBranch: "agent/test", OntologyRoot: "kb",
	})

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				m.Set("churn", spareA) // takes m.mu.Lock
				m.Set("churn", spareB)
			}
		}
	}()
	t.Cleanup(func() { close(stop) })

	// Assertions belong on the TEST goroutine: require.* calls t.FailNow, which
	// is unsupported off it, so a failure here would still close(done) and the
	// select below would take the done branch as though the run had succeeded.
	// Collect the outcome on a channel and assert after.
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			res, err := CatalogHandler(m)(context.Background(), mcpgo.CallToolRequest{})
			if err != nil {
				done <- err
				return
			}
			if res == nil {
				done <- errors.New("nil result from knomit_catalog")
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("knomit_catalog deadlocked against a concurrent manager writer")
	}
}

// Structural guard on the same property: a future edit that reaches for mgr
// inside the ForEach callback reintroduces the deadlock, and no ordinary test
// would catch it because it only fires under a concurrent writer.
func TestCatalog_ForEachCallbackTouchesNoManager(t *testing.T) {
	src, err := os.ReadFile("catalog.go")
	require.NoError(t, err)
	body := string(src)
	start := strings.Index(body, "mgr.ForEach(func(")
	require.Positive(t, start, "ForEach call not found — did catalogRepos change shape?")
	end := strings.Index(body[start:], "\n\t})")
	require.Positive(t, end, "could not find the end of the ForEach callback")
	callback := body[start+len("mgr.ForEach(func(") : start+end]
	// Check for `mgr` in ANY form, not just `mgr.` — the original defect was
	// profileFor(mgr, ri), which passes the manager to a helper that takes
	// m.mu. A `mgr.`-only check passes that mutation and is worse than no test,
	// because it looks like coverage.
	require.NotContains(t, callback, "mgr",
		"the ForEach callback must not use mgr at all, directly or as an argument: m.mu is not reentrant")
}

// A lens member with no LIVE INSTANCE is still named from its registry row —
// Manager.RepoLabel — not shown as a bare uid. The uid fallback applies only
// when the registry has no row for it either, which is why the other tests can
// assert no "uid-" appears: every member in those fixtures has a registry row.
//
// The member is registered but never instantiated, which is the real shape of
// this case (a repo whose store failed to open). Note it is built through the
// LensRegistry directly: Manager.CreateLens validates that members resolve, so
// it would refuse this lens by design.
func TestCatalog_LensMemberWithoutInstanceUsesRegistryName(t *testing.T) {
	m, _, _ := bindFixture(t)
	require.NoError(t, m.Repos().Insert(repos.RepoRecord{
		UID: "uid-ghost", Name: "ghost", State: repos.StateActive,
		Profile: "code", CreatedAt: 1,
	}))
	_, err := m.LensRegistry().Create(repos.Lens{
		Name: "haunted", WriteUID: "uid-alpha",
		Reads:     []repos.LensRead{{RepoUID: "uid-ghost"}},
		CreatedAt: 1, UpdatedAt: 1,
	})
	require.NoError(t, err)

	out := catalogOf(t, m, context.Background())

	lenses, _ := out["lenses"].([]any)
	var haunted map[string]any
	for _, l := range lenses {
		if l.(map[string]any)["name"] == "haunted" {
			haunted = l.(map[string]any)
		}
	}
	require.NotNil(t, haunted)
	mounts, _ := haunted["mounts"].([]any)

	var ghost map[string]any
	for _, mt := range mounts {
		if mt.(map[string]any)["repo"] == "ghost" {
			ghost = mt.(map[string]any)
		}
	}
	require.NotNil(t, ghost, "the member must be named from the registry, not shown as a uid")
	require.Equal(t, true, ghost["unavailable"])
	require.NotContains(t, ghost, "branch", "no live instance means no resolved branch")
}

// A lens registry that is unavailable must NOT render as "lenses": [] — an
// agent would read that as "this server has no lenses" and bind to a bare repo
// instead of the lens it needed, with nothing recording why.
//
// This exercises the not-started path (a Manager that was never Start()ed).
// The sibling path — List() itself failing — takes the same branch, but cannot
// be induced from here: the Manager-owned LensRegistry shares the control.db
// handle, so its Close() is a no-op (owns == false) and the query keeps working.
func TestCatalog_LensRegistryUnavailableIsAnError(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	t.Cleanup(func() { _ = m.Close() })
	require.Nil(t, m.LensRegistry(), "an unstarted manager has no lens registry")

	res, err := CatalogHandler(m)(context.Background(), mcpgo.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, res.IsError, "an unavailable registry must not look like an empty one")
	require.Contains(t, resultText(t, res), "lens registry")
}

// A session whose STORED pin no longer resolves must be distinguishable from
// one that never bound. Every other tool is already telling this agent "bound
// repo X is not available — call knomit_bind again"; knomit_catalog is the tool
// it calls to work out what happened, so it reports the reason rather than
// looking identical to a fresh session.
//
// The kind and name come from the pin carried on *SessionBindingError: the
// context holds only the error, so without that field neither is recoverable.
func TestCatalog_ReportsUnresolvableBinding(t *testing.T) {
	m, _, _ := bindFixture(t)
	// A repo the registry still knows, but with no live instance: resolution
	// fails, yet the name is real and worth showing.
	require.NoError(t, m.Repos().Insert(repos.RepoRecord{
		UID: "uid-departed", Name: "departed", State: repos.StateActive,
		Profile: "code", CreatedAt: 1,
	}))
	_, resolveErr := repos.ResolveSessionBinding(context.Background(), m, "repo:uid-departed")
	require.Error(t, resolveErr)

	ctx := repos.WithBindingError(repos.WithSessionScoped(context.Background()), resolveErr)
	out := catalogOf(t, m, ctx)

	bound, ok := out["bound"].(map[string]any)
	require.True(t, ok, "an unresolvable pin must still produce a bound key: %v", out)
	require.Equal(t, "unresolvable", bound["status"])
	require.Equal(t, "repo", bound["kind"], "the kind survives on the pin")
	require.Equal(t, "departed", bound["name"], "the registry still has a real name")
	require.NotEmpty(t, bound["error"])

	// And it stays distinct from the never-bound case.
	plain := catalogOf(t, m, repos.WithSessionScoped(context.Background()))
	require.NotContains(t, plain, "bound")
}

// With NO registry row the uid is all that is left — and a uid is not a name
// knomit_bind would accept, so the name key is omitted rather than filled with
// one. The kind still survives, because it comes from the pin's prefix.
func TestCatalog_UnresolvableBindingWithoutRegistryRow(t *testing.T) {
	m, _, _ := bindFixture(t)
	_, resolveErr := repos.ResolveSessionBinding(context.Background(), m, "repo:uid-never-existed")
	require.Error(t, resolveErr)

	out := catalogOf(t, m, repos.WithBindingError(context.Background(), resolveErr))

	bound, _ := out["bound"].(map[string]any)
	require.Equal(t, "unresolvable", bound["status"])
	require.Equal(t, "repo", bound["kind"])
	require.NotContains(t, bound, "name", "a uid is not a name; omit rather than show one")
	// The contract is about the NAME field, not the error prose: the error text
	// legitimately names what failed, and with no registry row the uid is all
	// there is to name it by (ResolveSessionBinding → RepoLabel's fallback).
	// What must never happen is that uid appearing as `name`, which knomit_bind
	// would then be offered and would reject.
	require.NotEqual(t, "uid-never-existed", bound["name"])
}

// A failure that is not about a specific pin (the store lookup itself breaking)
// carries no pin, so it reports the reason with no kind and no name.
func TestCatalog_UnresolvableBindingWithoutPin(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithBindingError(context.Background(),
		errors.New("session binding lookup failed — retry, or call knomit_bind again"))

	out := catalogOf(t, m, ctx)

	bound, _ := out["bound"].(map[string]any)
	require.Equal(t, "unresolvable", bound["status"])
	require.NotContains(t, bound, "kind")
	require.NotContains(t, bound, "name")
	require.Contains(t, bound["error"], "lookup failed")
}
