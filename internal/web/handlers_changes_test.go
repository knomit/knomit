package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

type changesPage struct {
	Count    int                           `json:"count"`
	Head     string                        `json:"head"`
	HasMore  bool                          `json:"has_more"`
	Links    map[string]map[string]string  `json:"_links"`
	Embedded map[string][]store.PathChange `json:"_embedded"`
}

func changesRepo(t *testing.T) (*repos.RepoInstance, http.Handler) {
	t.Helper()
	m, _ := newTestLensManager(t, "alpha")
	ri := m.Get("alpha")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	return ri, r
}

func seedOn(t *testing.T, ri *repos.RepoInstance, branch string, paths ...string) string {
	t.Helper()
	var head string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		for _, p := range paths {
			res, err := svc.Facts().WriteFact(context.Background(), branch, p,
				"---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# "+p+"\n\nbody\n", "add "+p, "learn")
			require.NoError(t, err)
			head = res.CommitHash
		}
	}))
	return head
}

func getChanges(t *testing.T, r http.Handler, url string) (*httptest.ResponseRecorder, changesPage) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var p changesPage
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	}
	return rec, p
}

// The REST twin reads the branch in its URL: the agent branch shows the
// agent's own writes; main does not.
func TestREST_Changes_ReadsTheURLBranch(t *testing.T) {
	ri, r := changesRepo(t)
	agent := ri.AgentBranch()
	since := seedOn(t, ri, agent, "kb/tasks/a/seed.md")
	seedOn(t, ri, agent, "kb/tasks/a/new.md")

	rec, p := getChanges(t, r, "/repos/alpha/branches/"+urlBranch(agent)+"/changes?prefix=tasks/a&since="+since)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/new.md", Change: store.ChangeAdded}}, p.Embedded["changes"])
	require.Equal(t, 1, p.Count)
	require.Len(t, p.Head, 40)
	require.NotContains(t, p.Links, "next")
}

func TestREST_Changes_PagesWithNextLink(t *testing.T) {
	ri, r := changesRepo(t)
	agent := ri.AgentBranch()
	since := seedOn(t, ri, agent, "kb/other/seed.md")
	paths := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		paths = append(paths, fmt.Sprintf("kb/tasks/a/t%d.md", i))
	}
	seedOn(t, ri, agent, paths...)

	base := "/repos/alpha/branches/" + urlBranch(agent) + "/changes"
	rec, p1 := getChanges(t, r, base+"?prefix=tasks/a&limit=2&since="+since)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, p1.Embedded["changes"], 2)
	require.True(t, p1.HasMore)
	next := p1.Links["next"]["href"]
	require.Contains(t, next, "cursor=")
	require.NotContains(t, next, "since=", "the cursor carries since; a stale since beside it would be misleading")

	seedOn(t, ri, agent, "kb/tasks/a/zz-late.md") // lands between pages

	rec, p2 := getChanges(t, r, strings.TrimPrefix(next, "/api/v1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/t2.md", Change: store.ChangeAdded}}, p2.Embedded["changes"])
	require.False(t, p2.HasMore)
	require.Equal(t, p1.Head, p2.Head)
}

// Every refusal is a 4xx with its remedy — never a 200 with an empty page.
func TestREST_Changes_RefusalsAre4xx(t *testing.T) {
	ri, r := changesRepo(t)
	agent := ri.AgentBranch()
	seedOn(t, ri, agent, "kb/tasks/a/t1.md")
	onAgent := seedOn(t, ri, agent, "kb/tasks/a/t2.md")
	base := "/repos/alpha/branches/" + urlBranch(agent) + "/changes"

	for _, c := range []struct {
		name, url string
		code      int
		says      string
	}{
		{"unknown since", base + "?since=0123456789abcdef0123456789abcdef01234567", http.StatusBadRequest, "omit since"},
		{"short since", base + "?since=abc", http.StatusBadRequest, "omit since"},
		{"since not behind", "/repos/alpha/branches/main/changes?since=" + onAgent, http.StatusConflict, "not behind"},
		{"private prefix", base + "?prefix=.knomit/inbox", http.StatusBadRequest, "private"},
		{"escaping prefix", base + "?prefix=../x", http.StatusBadRequest, ".."},
		{"bad cursor", base + "?cursor=garbage", http.StatusBadRequest, "cursor"},
		{"bad limit", base + "?limit=101", http.StatusBadRequest, "limit"},
		{"unknown branch", "/repos/alpha/branches/nope/changes", http.StatusNotFound, "nope"},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.url, nil))
		require.Equal(t, c.code, rec.Code, "%s: %s", c.name, rec.Body.String())
		require.Contains(t, rec.Body.String(), c.says, c.name)
	}
}
