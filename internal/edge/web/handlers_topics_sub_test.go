package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
)

type topicFactsBody struct {
	Count    int         `json:"count"`
	Links    hal.LinkMap `json:"_links"`
	Embedded struct {
		Facts []struct {
			Name  string      `json:"name"`
			Title string      `json:"title,omitempty"`
			Type  string      `json:"type,omitempty"`
			Links hal.LinkMap `json:"_links"`
		} `json:"facts"`
	} `json:"_embedded"`
}

func decodeTopicFacts(t *testing.T, rec *httptest.ResponseRecorder) topicFactsBody {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, hal.ContentType, rec.Header().Get("Content-Type"))
	var body topicFactsBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body
}

// /topics/<node>/facts lists only the facts directly at ontology/<node>: the
// sub-directory is skipped, and each fact carries its own type and title (the
// two differ in both) and links to its fact resource.
func TestHandleTopicFacts_ReturnsOnlyFiles(t *testing.T) {
	ri, svc, s := topicsRepo(t)
	writeFactsInOneCommit(t, ri,
		plainFact("ontology/ai/sub/deeper.md", "Deeper", nil),
		plainFact("ontology/ai/fact1.md", "Fact One", typed(fact.Observation)),
		plainFact("ontology/ai/fact2.md", "Fact Two", typed(fact.Concept)),
		plainFact("ontology/fact1.md", "Root decoy", typed(fact.Insight)),
	)
	seen := observeListDir(t, map[string]*store.Service{"alpha": svc})

	body := decodeTopicFacts(t, getAPI(t, s, "/repos/alpha/branches/agent:test/topics/ai/facts"))

	require.Equal(t, []string{"ontology/ai"}, seen.of("alpha"))
	branchURL := APIBase + "/repos/alpha/branches/agent:test"
	require.Equal(t, branchURL+"/topics/ai/facts", body.Links["self"].Href)
	require.Equal(t, 2, body.Count, "the directory is excluded")
	require.Len(t, body.Embedded.Facts, 2)
	one, two := body.Embedded.Facts[0], body.Embedded.Facts[1]
	require.Equal(t, "fact1.md", one.Name)
	require.Equal(t, "Fact One", one.Title)
	require.Equal(t, "observation", one.Type)
	require.Equal(t, branchURL+"/facts/ontology/ai/fact1.md", one.Links["self"].Href)
	require.Equal(t, "fact2.md", two.Name)
	require.Equal(t, "Fact Two", two.Title)
	require.Equal(t, "concept", two.Type)
	require.Equal(t, branchURL+"/facts/ontology/ai/fact2.md", two.Links["self"].Href)
}

// A ListDir failure is a 500 carrying the store's error.
func TestHandleTopicFacts_ListDirErrorIs500(t *testing.T) {
	ri, svc, s := topicsRepo(t)
	writeFactsInOneCommit(t, ri, plainFact("ontology/ai/x.md", "X", nil))
	injected := errors.New("topic facts tree unreadable")
	var hits atomic.Int64
	failListDir(t, svc, "ontology/ai", injected, &hits)

	rec := getAPI(t, s, "/repos/alpha/branches/agent:test/topics/ai/facts")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Failed to list topic facts")
	require.Contains(t, rec.Body.String(), injected.Error())
	require.EqualValues(t, 1, hits.Load())
}

func TestHandleTopicFacts_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "ontology",
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/topics/ai/facts", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

func TestHandleTopicStats_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "ontology",
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/topics/ai/stats", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

func TestHandleTopicStats_ReturnsStatsView(t *testing.T) {
	// With no real store, Stats returns zero values — just verify the shape.
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/ai/stats", nil)
	r.ServeHTTP(rec, req)

	// With no store the stats read yields zero stats — still 200, valid JSON.
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Total int         `json:"total"`
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
}

// TestHandleTopicStats_EmptyHighlightsSerializeAsArrayNotNull regresses
// handleTopicStats specifically (not handleHALStats): handleTopicStats calls
// loadStats directly and shares statsView, but has its own
// nil-guard block for types/highlights/default_axis. With no real store
// backing this test's RepoInstance, ri.WithRead never runs the callback that
// would populate result, so Stats returns a zero-value StatsResult{} (nil
// Types map, nil Highlights slice) with a nil error — exactly the "store
// briefly unavailable" shape the guard exists for. Mirrors
// TestHandleHALStats_EmptyHighlightsSerializeAsArrayNotNull's exact-string
// assertion style so this proves array/object serialization, not mere
// non-nullness.
func TestHandleTopicStats_EmptyHighlightsSerializeAsArrayNotNull(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/ai/stats", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if !strings.Contains(got, `"highlights":[]`) {
		t.Errorf("highlights must serialize as [], got: %s", got)
	}
	if !strings.Contains(got, `"types":{}`) {
		t.Errorf("types must serialize as {}, got: %s", got)
	}
}

// TestHandleTopicFacts_HidesPrivateTopic pins spec/mbekg.md §3.8 for the
// direct-navigation case the reviewer called out: GET .../topics/.drafts/facts
// must not list the stash's contents just because the caller names it
// directly — private governs discovery "including under the ontology root",
// not just whether a parent listing exposes it.
func TestHandleTopicFacts_HidesPrivateTopic(t *testing.T) {
	ri, svc, s := topicsRepo(t)
	writeFactsInOneCommit(t, ri, plainFact("ontology/.drafts/secret.md", "Secret", nil))
	// The store does list the stash: the handler's guard is what hides it.
	entries, err := svc.Facts().ListDir(t.Context(), "agent/test", "ontology/.drafts")
	require.NoError(t, err)
	require.Len(t, entries, 1, "fixture: ListDir must return the draft for the guard to have work")
	seen := observeListDir(t, map[string]*store.Service{"alpha": svc})

	body := decodeTopicFacts(t, getAPI(t, s, "/repos/alpha/branches/agent:test/topics/.drafts/facts"))

	require.Equal(t, []string{"ontology/.drafts"}, seen.of("alpha"))
	require.Equal(t, 0, body.Count, "a private topic's facts must not be listed")
	require.Empty(t, body.Embedded.Facts)
}

// TestHandleTopicNode_StillWorksAfterDispatch ensures the existing topic-node
// behaviour is unaffected when no sub-resource suffix is present.
func TestHandleTopicNode_StillWorksAfterDispatch(t *testing.T) {
	ri, svc, s := topicsRepo(t)
	writeFactsInOneCommit(t, ri, plainFact("ontology/ai/ml/x.md", "X", nil))
	seen := observeListDir(t, map[string]*store.Service{"alpha": svc})

	body := decodeTopics(t, getAPI(t, s, "/repos/alpha/branches/agent:test/topics/ai"))

	require.Equal(t, []string{"ontology/ai"}, seen.of("alpha"))
	require.Len(t, body.Embedded.Topics, 1)
	require.Equal(t, "ml", body.Embedded.Topics[0].Name)
	require.True(t, body.Embedded.Topics[0].IsDir)
}

// /topics/<node>/facts on a branch the store does not have is the same 404 as
// the topic listing (writeStoreError maps ErrBranchNotFound), not a 500.
func TestHandleTopicFacts_MissingBranch_Returns404(t *testing.T) {
	_, svc, s := topicsRepo(t)
	seen := observeListDir(t, map[string]*store.Service{"alpha": svc})

	rec := getAPI(t, s, "/repos/alpha/branches/does-not-exist/topics/ai/facts")

	require.Equal(t, []string{"ontology/ai"}, seen.of("alpha"), "the 404 must come from the store's ListDir")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "Branch not found", body["title"])
	require.Equal(t, `no branch named "does-not-exist"`, body["detail"])
}
