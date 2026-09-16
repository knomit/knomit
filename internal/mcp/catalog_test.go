package mcp

import (
	"context"
	"encoding/json"
	"os"
	"testing"

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

// The catalogue is a cheap index: it must not touch any repo's store, so a
// closed store cannot make it fail.
func TestCatalog_NoPerRepoStoreRead(t *testing.T) {
	m, _, _ := bindFixture(t)
	ri := m.Get("alpha")
	require.NotNil(t, ri)
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	release()
	require.NoError(t, svc.Close()) // a README read would now fail

	out := catalogOf(t, m, context.Background())
	reposList, _ := out["repos"].([]any)
	require.Len(t, reposList, 3, "listing must not depend on any repo's store")
}
