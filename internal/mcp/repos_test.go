package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"

	"knomit/internal/config"
)

func TestReposHandler_ListsMounts(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, "agent/test"))

	var req mcpgo.CallToolRequest
	m, _, _ := bindFixture(t)
	result, err := ReposHandler(m)(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))

	var resp struct {
		Binding string `json:"binding"`
		Mounts  []struct {
			Name        string `json:"name"`
			ID          string `json:"id"`
			Branch      string `json:"branch"`
			Role        string `json:"role"`
			Source      string `json:"source,omitempty"`
			WriteBranch string `json:"write_branch,omitempty"`
		} `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal(boundJSON(t, result), &resp))
	require.Equal(t, "test", resp.Binding)
	require.Len(t, resp.Mounts, 1)
	require.Equal(t, "test", resp.Mounts[0].Name)
	require.Equal(t, "agent/test", resp.Mounts[0].Branch)
	require.Equal(t, "read+write", resp.Mounts[0].Role)
	require.Equal(t, "agent/test", resp.Mounts[0].WriteBranch,
		"the read+write row surfaces the write target (agent branch)")
	// The mount id is the 12-hex wire form (federate.ID12), matching kb://<id>/… paths
	// and the AfterInitialize instructions mount table — not the full hash (M-3).
	require.Len(t, resp.Mounts[0].ID, 12)
	require.Equal(t, federate.ID12(ri.ID()), resp.Mounts[0].ID)
}

// TestReposHandler_ReadOnlyView pins the discovery contract for a read-only
// view: a lens-of-one bound to a non-agent branch (here "main") is not
// writable, so its single mount must advertise role "read" — never
// "read+write". Regression guard for the WriteOK() omission in the role rule.
func TestReposHandler_ReadOnlyView(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, "main"))

	var req mcpgo.CallToolRequest
	m, _, _ := bindFixture(t)
	result, err := ReposHandler(m)(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))

	var resp struct {
		Binding string `json:"binding"`
		Mounts  []struct {
			Branch      string `json:"branch"`
			Role        string `json:"role"`
			WriteBranch string `json:"write_branch,omitempty"`
		} `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal(boundJSON(t, result), &resp))
	require.Len(t, resp.Mounts, 1)
	require.Equal(t, "main", resp.Mounts[0].Branch)
	require.Equal(t, "read", resp.Mounts[0].Role)
	require.Empty(t, resp.Mounts[0].WriteBranch, "a read-only view has no write target")
}

// TestReposHandler_WriteBranchSurfacesAgentTarget covers the misleading case
// M-4 addresses: when a lens pins the write repo's READ mount to a non-agent
// branch, the branch column shows that pinned read branch, but writes still
// land on the write repo's agent branch (RFC decision 19). The read+write row
// must surface both — write_branch = agent branch, branch = the pinned read
// branch — while a foreign read mount carries no write_branch at all.
func TestReposHandler_WriteBranchSurfacesAgentTarget(t *testing.T) {
	writeRepo := newLearnTestRepo(t, fact.CodeOntology())
	readRepo := newLearnTestRepo(t, fact.CodeOntology())
	// Write repo's own read mount is pinned to "main", NOT its agent branch.
	b := repos.NewBindingForTest(writeRepo,
		repos.ReadTarget{RI: writeRepo, Branch: "main"},
		repos.ReadTarget{RI: readRepo, Branch: "agent/test", Source: "core-src"},
	)
	ctx := repos.WithBinding(context.Background(), b)

	var req mcpgo.CallToolRequest
	m, _, _ := bindFixture(t)
	result, err := ReposHandler(m)(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))

	type mountRow struct {
		ID          string `json:"id"`
		Branch      string `json:"branch"`
		Role        string `json:"role"`
		WriteBranch string `json:"write_branch,omitempty"`
	}
	var resp struct {
		Mounts []mountRow `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal(boundJSON(t, result), &resp))
	require.Len(t, resp.Mounts, 2)

	byID := map[string]mountRow{}
	for _, m := range resp.Mounts {
		byID[m.ID] = m
	}

	w := byID[federate.ID12(writeRepo.ID())]
	require.Equal(t, "read+write", w.Role)
	require.Equal(t, "main", w.Branch, "branch column shows the pinned READ branch")
	require.Equal(t, writeRepo.AgentBranch(), w.WriteBranch,
		"write_branch shows the agent branch writes actually target, not the read branch")
	require.NotEqual(t, w.Branch, w.WriteBranch, "the misleading case: read branch != write target")

	r := byID[federate.ID12(readRepo.ID())]
	require.Equal(t, "read", r.Role)
	require.Empty(t, r.WriteBranch, "read mounts carry no write_branch")
}

// A subscription mount reports what a caller needs to know before trying to
// write: it is a read-only follower, and the branch it is read at is the
// upstream — which an EMPTY pin must resolve to, not the (absent) agent branch.
func TestReposHandler_SubscriptionMountCarriesMode(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	sub := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "sub", UID: nextTestRepoUID(), Svc: svc, Subscribed: true, ReadBranch: "main",
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(sub, ""))

	var req mcpgo.CallToolRequest
	m, _, _ := bindFixture(t)
	result, err := ReposHandler(m)(ctx, req)
	require.NoError(t, err)
	var resp struct {
		Mounts []struct {
			Branch, Role, Mode, WriteBranch string
		} `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal(boundJSON(t, result), &resp))
	require.Len(t, resp.Mounts, 1)
	require.Equal(t, "main", resp.Mounts[0].Branch, "empty pin resolves to the READ branch")
	require.Equal(t, "read", resp.Mounts[0].Role)
	require.Equal(t, "subscribe", resp.Mounts[0].Mode)
	require.Empty(t, resp.Mounts[0].WriteBranch)
}

// boundJSON returns the `bound` section of a knomit_repos result as raw JSON so
// a test can decode it into whatever shape it asserts on.
//
// The mount table moved under this key when knomit_catalog was folded into
// knomit_repos: the tool now always returns {repos, lenses, bound?}, and what
// it used to return at the top level is the bound section.
func boundJSON(t *testing.T, result *mcpgo.CallToolResult) []byte {
	t.Helper()
	var envelope struct {
		Bound json.RawMessage `json:"bound"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &envelope))
	require.NotEmpty(t, envelope.Bound, "expected a bound section: %s", resultText(t, result))
	return envelope.Bound
}

// catalogOf calls knomit_repos and decodes its envelope. Named for the
// catalogue half of the tool, which is what these tests exercise.
func catalogOf(t *testing.T, m *repos.Manager, ctx context.Context) map[string]any {
	t.Helper()
	res, err := ReposHandler(m)(ctx, mcpgo.CallToolRequest{})
	require.NoError(t, err)
	require.NotNil(t, res)
	text := resultText(t, res)
	require.False(t, res.IsError, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), "payload: %s", text)
	return out
}

func TestRepos_ListsReposAndLenses(t *testing.T) {
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
func TestRepos_WorksUnbound(t *testing.T) {
	m, _, _ := bindFixture(t)

	out := catalogOf(t, m, repos.WithSessionScoped(context.Background()))

	require.NotContains(t, out, "bound", "unbound sessions get no bound key")
	reposList, _ := out["repos"].([]any)
	require.Len(t, reposList, 3)
}

func TestRepos_ReportsBound(t *testing.T) {
	m, _, _ := bindFixture(t)
	ri := m.Get("alpha")
	require.NotNil(t, ri)

	ctx := repos.WithBinding(repos.WithSessionScoped(context.Background()),
		repos.NewBindingOfRepo(ri, ""))
	out := catalogOf(t, m, ctx)

	// A LIVE binding reports the mount table knomit_repos returned before the
	// catalogue was folded in — now nested under `bound`.
	bound, ok := out["bound"].(map[string]any)
	require.True(t, ok, "payload: %v", out)
	require.Equal(t, "alpha", bound["binding"])
	mounts, _ := bound["mounts"].([]any)
	require.Len(t, mounts, 1)
	require.Equal(t, "alpha", mounts[0].(map[string]any)["name"])
	require.Equal(t, "read+write", mounts[0].(map[string]any)["role"])
	require.Equal(t, "agent/test", mounts[0].(map[string]any)["write_branch"])
	// And the catalogue sections are present in the same answer.
	require.Len(t, out["repos"], 3)
	require.Len(t, out["lenses"], 1)
}

func TestRepos_ReportsBoundLens(t *testing.T) {
	m, _, _ := bindFixture(t)
	l, ok, err := m.LensRegistry().Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	b, err := repos.NewBindingOfLens(m, l)
	require.NoError(t, err)

	out := catalogOf(t, m, repos.WithBinding(context.Background(), b))

	bound, _ := out["bound"].(map[string]any)
	require.Equal(t, "eng", bound["binding"])
	mounts, _ := bound["mounts"].([]any)
	require.Len(t, mounts, 2, "a lens binding reports every mount")
}

// A repo registered but with no live instance is still listed, with the reason
// a bind would fail — mirroring the REST list, which keeps broken repos visible.
//
// Built through the PRODUCTION path rather than a test backdoor: create a real
// repo, delete its store file, and start a second manager over the same home,
// which is exactly how openRegistered comes to call markUnavailable.
func TestRepos_UnavailableRepoListed(t *testing.T) {
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
func TestRepos_NoPerRepoStoreRead(t *testing.T) {
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
// This drives knomit_repos while another goroutine repeatedly takes the
// manager's WRITE lock, and fails on a timeout rather than hanging the suite.
func TestRepos_NoDeadlockUnderConcurrentWriter(t *testing.T) {
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
			res, err := ReposHandler(m)(context.Background(), mcpgo.CallToolRequest{})
			if err != nil {
				done <- err
				return
			}
			if res == nil {
				done <- errors.New("nil result from knomit_repos")
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("knomit_repos deadlocked against a concurrent manager writer")
	}
}

// Structural guard on the same property: a future edit that reaches for mgr
// inside the ForEach callback reintroduces the deadlock, and no ordinary test
// would catch it because it only fires under a concurrent writer.
func TestRepos_ForEachCallbackTouchesNoManager(t *testing.T) {
	src, err := os.ReadFile("repos.go")
	require.NoError(t, err)
	body := string(src)
	start := strings.Index(body, "mgr.ForEach(func(")
	require.Positive(t, start, "ForEach call not found — did listRepos change shape?")
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
func TestRepos_LensMemberWithoutInstanceUsesRegistryName(t *testing.T) {
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
// It must also not abort the whole tool. Before the catalogue was folded in,
// this call could not fail at all, so a bound agent would lose its own mount
// table to an unrelated control-plane error. The key is OMITTED and the failure
// named, and everything that did resolve is still returned.
//
// This exercises the not-started path (a Manager that was never Start()ed).
// The sibling path — List() itself failing — takes the same branch, but cannot
// be induced from here: the Manager-owned LensRegistry shares the control.db
// handle, so its Close() is a no-op (owns == false) and the query keeps working.
func TestRepos_LensRegistryUnavailableDegrades(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	t.Cleanup(func() { _ = m.Close() })
	require.Nil(t, m.LensRegistry(), "an unstarted manager has no lens registry")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "u-alpha", AgentBranch: "agent/test", OntologyRoot: "kb",
	})
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, ""))

	res, err := ReposHandler(m)(ctx, mcpgo.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, res.IsError, "a lens failure must not abort the tool: %s", resultText(t, res))

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))

	require.NotContains(t, out, "lenses", "an empty list would read as 'no lenses exist'")
	require.Contains(t, out["lenses_error"], "lens registry")
	// The bound section survives — that is the point of degrading.
	bound, ok := out["bound"].(map[string]any)
	require.True(t, ok, "the binding's own mount table must survive: %v", out)
	require.Equal(t, "alpha", bound["binding"])
}

// The empty case stays distinguishable from the failed one: a server with no
// lenses returns an empty LIST, not an omitted key.
func TestRepos_NoLensesIsAnEmptyList(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	out := catalogOf(t, m, context.Background())

	lenses, ok := out["lenses"].([]any)
	require.True(t, ok, "a working registry with no lenses returns []: %v", out)
	require.Empty(t, lenses)
	require.NotContains(t, out, "lenses_error")
}

// A session whose STORED pin no longer resolves must be distinguishable from
// one that never bound. Every other tool is already telling this agent "bound
// repo X is not available — call knomit_bind again"; knomit_repos is the tool
// it calls to work out what happened, so it reports the reason rather than
// looking identical to a fresh session.
//
// The kind and name come from the pin carried on *SessionBindingError: the
// context holds only the error, so without that field neither is recoverable.
func TestRepos_ReportsUnresolvableBinding(t *testing.T) {
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
func TestRepos_UnresolvableBindingWithoutRegistryRow(t *testing.T) {
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
func TestRepos_UnresolvableBindingWithoutPin(t *testing.T) {
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
