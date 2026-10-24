package web

import (
	"encoding/json"
	"errors"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
)

// listDirSeen records, per store, every path the topic browse handlers asked
// ListDir for. It is an observer: its hook returns nil, so the real listing
// runs and what the handler serves is the store's own answer. The lens fan-out
// lists every mount on its own goroutine, hence the mutex.
type listDirSeen struct {
	mu    sync.Mutex
	paths map[string][]string
}

func (s *listDirSeen) of(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths[name]...)
}

// observeListDir installs a pass-through ListDir hook on each named store.
func observeListDir(t *testing.T, svcs map[string]*store.Service) *listDirSeen {
	t.Helper()
	seen := &listDirSeen{paths: map[string][]string{}}
	for name, svc := range svcs {
		name := name
		t.Cleanup(svc.Facts().SetListDirFaultForTest(func(path string) error {
			seen.mu.Lock()
			seen.paths[name] = append(seen.paths[name], path)
			seen.mu.Unlock()
			return nil
		}))
	}
	return seen
}

// failListDir fails ListDir of exactly path on svc with err and fails no other
// listing; hits counts how often that path reached the hook.
func failListDir(t *testing.T, svc *store.Service, path string, err error, hits *atomic.Int64) {
	t.Helper()
	t.Cleanup(svc.Facts().SetListDirFaultForTest(func(got string) error {
		if got != path {
			return nil
		}
		hits.Add(1)
		return err
	}))
}

// typed is a factSpec mod that sets the fact's (epistemic) type.
func typed(ty fact.Type) func(f *fact.Fact) {
	return func(f *fact.Fact) { f.Type = ty }
}

// topicsRepo is a real-store repo "alpha" whose store and server both use the
// ontology root "ontology", as the repos builder configures a store from the
// one global root. It is deliberately not the default "kb", so a handler that
// hard-codes the default lists the wrong directory.
func topicsRepo(t *testing.T) (*repos.RepoInstance, *store.Service, *Server) {
	t.Helper()
	m, svcs := newRealStoreManager(t, "alpha")
	svcs["alpha"].SetOntologyRoot("ontology")
	return m.Get("alpha"), svcs["alpha"], &Server{Manager: m, OntologyRoot: "ontology"}
}

type topicsBody struct {
	Count    int         `json:"count"`
	Links    hal.LinkMap `json:"_links"`
	Embedded struct {
		Topics []struct {
			Name  string      `json:"name"`
			IsDir bool        `json:"is_dir"`
			Type  string      `json:"type,omitempty"`
			Title string      `json:"title,omitempty"`
			Links hal.LinkMap `json:"_links"`
		} `json:"topics"`
	} `json:"_embedded"`
}

func decodeTopics(t *testing.T, rec *httptest.ResponseRecorder) topicsBody {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	var body topicsBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body
}

// The root listing lists the ontology root: a directory links to its topic
// node, a fact links to its fact resource and carries the type and title of
// THAT fact (the facts under ai/ have other titles and types, so enrichment
// from the wrong path is caught).
func TestHandleTopics_ReturnsCollection(t *testing.T) {
	ri, svc, s := topicsRepo(t)
	writeFactsInOneCommit(t, ri,
		plainFact("ontology/ai/nested.md", "Nested", typed(fact.Concept)),
		plainFact("ontology/intro.md", "Introduction", typed(fact.Principle)),
		plainFact("kb/elsewhere.md", "Not under the root", nil),
	)
	seen := observeListDir(t, map[string]*store.Service{"alpha": svc})

	body := decodeTopics(t, getAPI(t, s, "/repos/alpha/branches/agent:test/topics"))

	require.Equal(t, []string{"ontology"}, seen.of("alpha"), "the root listing lists the ontology root, once")
	branchURL := httpx.APIBase + "/repos/alpha/branches/agent:test"
	require.Equal(t, branchURL+"/topics", body.Links["self"].Href)
	require.Equal(t, 2, body.Count)
	require.Len(t, body.Embedded.Topics, 2)

	dir := body.Embedded.Topics[0]
	require.Equal(t, "ai", dir.Name)
	require.True(t, dir.IsDir)
	require.Empty(t, dir.Type, "a directory is not enriched")
	require.Empty(t, dir.Title, "a directory is not enriched")
	require.Equal(t, branchURL+"/topics/ai", dir.Links["self"].Href)

	file := body.Embedded.Topics[1]
	require.Equal(t, "intro.md", file.Name)
	require.False(t, file.IsDir)
	require.Equal(t, "principle", file.Type)
	require.Equal(t, "Introduction", file.Title)
	require.Equal(t, branchURL+"/facts/ontology/intro.md", file.Links["self"].Href)
}

// A topic node lists ontology/<node>: its child directory links one level
// deeper, and its fact is enriched from ontology/<node>/<name>, not from the
// same-named fact at the root (a decoy with another title and type).
func TestHandleTopicNode_ReturnsChildren(t *testing.T) {
	ri, svc, s := topicsRepo(t)
	writeFactsInOneCommit(t, ri,
		plainFact("ontology/ai/ml/deep.md", "Deep", nil),
		plainFact("ontology/ai/overview.md", "AI Overview", typed(fact.Hypothesis)),
		plainFact("ontology/overview.md", "Root overview decoy", typed(fact.Insight)),
	)
	seen := observeListDir(t, map[string]*store.Service{"alpha": svc})

	body := decodeTopics(t, getAPI(t, s, "/repos/alpha/branches/agent:test/topics/ai"))

	require.Equal(t, []string{"ontology/ai"}, seen.of("alpha"))
	branchURL := httpx.APIBase + "/repos/alpha/branches/agent:test"
	require.Equal(t, branchURL+"/topics/ai", body.Links["self"].Href)
	require.Equal(t, 2, body.Count)
	require.Len(t, body.Embedded.Topics, 2)

	dir := body.Embedded.Topics[0]
	require.Equal(t, "ml", dir.Name)
	require.True(t, dir.IsDir)
	require.Equal(t, branchURL+"/topics/ai/ml", dir.Links["self"].Href)

	file := body.Embedded.Topics[1]
	require.Equal(t, "overview.md", file.Name)
	require.Equal(t, "hypothesis", file.Type)
	require.Equal(t, "AI Overview", file.Title)
	require.Equal(t, branchURL+"/facts/ontology/ai/overview.md", file.Links["self"].Href)
}

// TestHandleTopics_HidesPrivateDirectory pins spec/mbekg.md §3.8: a
// dot-prefixed directory under the ontology root is machinery, not a topic,
// and readers MUST skip it during discovery. ListDir (a raw git tree walk)
// returns it unfiltered, so this is the handler's own responsibility, not
// the store's — the same call site the reviewer flagged as listing
// ".drafts" as an ordinary topic.
func TestHandleTopics_HidesPrivateDirectory(t *testing.T) {
	ri, svc, s := topicsRepo(t)
	writeFactsInOneCommit(t, ri,
		plainFact("ontology/ai/x.md", "X", nil),
		plainFact("ontology/.drafts/y.md", "Draft", nil),
	)
	// The store itself does list the private directory: the handler's guard
	// is what hides it.
	entries, err := svc.Facts().ListDir(t.Context(), "agent/test", "ontology")
	require.NoError(t, err)
	require.Len(t, entries, 2, "fixture: ListDir must return .drafts for the guard to have work")

	body := decodeTopics(t, getAPI(t, s, "/repos/alpha/branches/agent:test/topics"))

	require.Equal(t, 1, body.Count, "the private dir must be hidden")
	require.Len(t, body.Embedded.Topics, 1)
	require.Equal(t, "ai", body.Embedded.Topics[0].Name)
}

func TestHandleTopics_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "ontology",
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/topics", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

func TestHandleTopicNode_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "ontology",
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/topics/ai", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// A branch the store does not have fails ListDir with ErrBranchNotFound, which
// the handler serves as a 404 naming the branch, not as a 500.
func TestHandleTopics_MissingBranch_Returns404(t *testing.T) {
	_, svc, s := topicsRepo(t)
	seen := observeListDir(t, map[string]*store.Service{"alpha": svc})

	rec := getAPI(t, s, "/repos/alpha/branches/does-not-exist/topics")

	require.Equal(t, []string{"ontology"}, seen.of("alpha"), "the 404 must come from the store's ListDir")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "Branch not found", body["title"])
	require.Equal(t, `no branch named "does-not-exist"`, body["detail"])
}

// A ListDir failure is a 500 carrying the store's error, for the root listing
// and for a topic node.
func TestHandleTopics_ListDirErrorIs500(t *testing.T) {
	for _, tc := range []struct{ url, dir string }{
		{"/repos/alpha/branches/agent:test/topics", "ontology"},
		{"/repos/alpha/branches/agent:test/topics/ai", "ontology/ai"},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			ri, svc, s := topicsRepo(t)
			writeFactsInOneCommit(t, ri, plainFact("ontology/ai/x.md", "X", nil))
			injected := errors.New("topics tree unreadable")
			var hits atomic.Int64
			failListDir(t, svc, tc.dir, injected, &hits)

			rec := getAPI(t, s, tc.url)

			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
			require.Contains(t, rec.Body.String(), "Failed to list topics")
			require.Contains(t, rec.Body.String(), injected.Error())
			require.EqualValues(t, 1, hits.Load())
		})
	}
}

// The repo browse handlers list AND enrich at the branch the request names,
// not at the repo's agent branch. The same path exists on both branches with
// different titles and types, so enrichment from the agent branch shows, for
// the topic node and for its /facts sub-resource alike.
func TestHandleTopics_EnrichesAtTheRequestedBranch(t *testing.T) {
	ri, svc, s := topicsRepo(t)
	require.NoError(t, svc.CreateBranch(t.Context(), "agent/elsewhere", "agent/test"))
	writeOnBranch(t, ri, "agent/elsewhere",
		plainFact("ontology/ai/x.md", "Elsewhere copy", typed(fact.Hypothesis)))
	writeFactsInOneCommit(t, ri,
		plainFact("ontology/ai/x.md", "Agent-branch decoy", typed(fact.Insight)))
	require.Equal(t, "agent/test", ri.AgentBranch(), "fixture: the requested branch must not be the agent branch")

	node := decodeTopics(t, getAPI(t, s, "/repos/alpha/branches/agent:elsewhere/topics/ai"))
	require.Len(t, node.Embedded.Topics, 1)
	require.Equal(t, "Elsewhere copy", node.Embedded.Topics[0].Title)
	require.Equal(t, "hypothesis", node.Embedded.Topics[0].Type)
	require.Equal(t, httpx.APIBase+"/repos/alpha/branches/agent:elsewhere/facts/ontology/ai/x.md",
		node.Embedded.Topics[0].Links["self"].Href)

	facts := decodeTopicFacts(t, getAPI(t, s, "/repos/alpha/branches/agent:elsewhere/topics/ai/facts"))
	require.Len(t, facts.Embedded.Facts, 1)
	require.Equal(t, "Elsewhere copy", facts.Embedded.Facts[0].Title)
	require.Equal(t, "hypothesis", facts.Embedded.Facts[0].Type)
}
