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

	"knomit/internal/federate"
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
		{"malformed bookmark", base + "?since=alpha:" + onAgent, http.StatusBadRequest, "bookmark"},
		{"bookmark for another repo", base + "?since=0123456789ab:" + onAgent, http.StatusBadRequest, "bookmark is for repo 0123456789ab"},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.url, nil))
		require.Equal(t, c.code, rec.Code, "%s: %s", c.name, rec.Body.String())
		require.Contains(t, rec.Body.String(), c.says, c.name)
	}
}

// since also takes the MCP tool's bookmark form when it names this repo, and
// means exactly what the bare hash means.
func TestREST_Changes_AcceptsBookmarkForThisRepo(t *testing.T) {
	ri, r := changesRepo(t)
	agent := ri.AgentBranch()
	since := seedOn(t, ri, agent, "kb/tasks/a/seed.md")
	seedOn(t, ri, agent, "kb/tasks/a/new.md")
	base := "/repos/alpha/branches/" + urlBranch(agent) + "/changes?prefix=tasks/a&since="

	rec, bare := getChanges(t, r, base+since)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec, marked := getChanges(t, r, base+store.ChangesBookmark(federate.ID12(ri.ID()), since))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, bare.Embedded, marked.Embedded)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/new.md", Change: store.ChangeAdded}}, marked.Embedded["changes"])
}

// A cursor records the repo and branch it was minted for: replayed on another
// branch or another repo's route it is refused, never re-read there; a cursor
// with no scope was never minted and is refused too.
func TestREST_Changes_CursorIsScopedToRepoAndBranch(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	ri := m.Get("alpha")
	agent := ri.AgentBranch()
	seedOn(t, ri, agent, "kb/tasks/a/t1.md", "kb/tasks/a/t2.md", "kb/tasks/a/t3.md")

	base := "/repos/alpha/branches/" + urlBranch(agent) + "/changes"
	rec, p1 := getChanges(t, r, base+"?prefix=tasks/a&limit=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	next := p1.Links["next"]["href"]
	_, cursor, ok := strings.Cut(next, "cursor=")
	require.True(t, ok, next)
	if i := strings.Index(cursor, "&"); i >= 0 {
		cursor = cursor[:i]
	}

	for name, url := range map[string]string{
		"another branch": "/repos/alpha/branches/main/changes?cursor=" + cursor,
		"another repo":   "/repos/beta/branches/" + urlBranch(agent) + "/changes?cursor=" + cursor, // same branch name: only the repo half differs
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
		require.Contains(t, rec.Body.String(), "cursor is for branch", name) // the guard's own text, not the store's generic refusal
	}

	noScope := store.EncodeChangesCursor(store.ChangesScope{}, "", p1.Head, "tasks/a", p1.Embedded["changes"][0].Path)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"?limit=1&cursor="+noScope, nil))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Invalid cursor")
}

// The ancestry check on a cursor's pinned head cannot tell branches apart when
// one contains the other's head: a cursor minted on main replayed on the agent
// branch AFTER it merged main passes that check, so only the scope guard
// refuses it. (A different repo cannot contain this repo's head -- the repo id
// IS its root commit -- so the repo half of the guard is only observable by its
// message, asserted in the test above.)
func TestREST_Changes_CursorForMainRefusedOnABranchContainingItsHead(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	ri := m.Get("alpha")
	agent := ri.AgentBranch()
	seedOn(t, ri, "main", "kb/tasks/a/t1.md", "kb/tasks/a/t2.md", "kb/tasks/a/t3.md")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().MergeBranch(context.Background(), "main", agent, store.StrategyLocalWins))
	}))

	rec, p1 := getChanges(t, r, "/repos/alpha/branches/main/changes?prefix=tasks/a&limit=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, cursor, ok := strings.Cut(p1.Links["next"]["href"], "cursor=")
	require.True(t, ok)
	if i := strings.Index(cursor, "&"); i >= 0 {
		cursor = cursor[:i]
	}

	// Sanity: the same cursor pages main itself.
	rec, _ = getChanges(t, r, "/repos/alpha/branches/main/changes?limit=1&cursor="+cursor)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/"+urlBranch(agent)+"/changes?cursor="+cursor, nil))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "cursor is for branch")
}

// A bookmark with an empty or non-40-hex commit half is ErrUnknownSince, never
// read as "omit since".
func TestREST_Changes_MalformedBookmarkCommitIsRefused(t *testing.T) {
	ri, r := changesRepo(t)
	seedOn(t, ri, ri.AgentBranch(), "kb/tasks/a/t1.md")
	base := "/repos/alpha/branches/" + urlBranch(ri.AgentBranch()) + "/changes?since="
	for _, since := range []string{federate.ID12(ri.ID()) + ":", federate.ID12(ri.ID()) + ":abc"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+since, nil))
		require.Equal(t, http.StatusBadRequest, rec.Code, since+": "+rec.Body.String())
		require.Contains(t, rec.Body.String(), "Unknown since", since)
	}
}
