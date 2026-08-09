package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/store"
	"knomit/internal/web/hal"
)

func TestHandleTopicFacts_ReturnsOnlyFiles(t *testing.T) {
	lister := &stubTopicLister{
		dirs: []store.DirEntry{
			{Name: "sub", IsDir: true},
			{Name: "fact1.md", IsDir: false},
			{Name: "fact2.md", IsDir: false},
		},
		byPath: map[string]*store.FactWithBody{
			"ontology/ai/fact1.md": {FactRecord: store.FactRecord{Title: "Fact One", Type: "observation"}},
			"ontology/ai/fact2.md": {FactRecord: store.FactRecord{Title: "Fact Two", Type: "concept"}},
		},
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
		providers: storeProviders{
			topicLister: lister,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/ai/facts", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
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
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Only 2 file entries (directory excluded).
	if body.Count != 2 {
		t.Errorf("count: got %d, want 2", body.Count)
	}
	if len(body.Embedded.Facts) != 2 {
		t.Fatalf("embedded facts: got %d, want 2", len(body.Embedded.Facts))
	}

	// Self link present on collection.
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link on collection")
	}

	// Each fact has a self link.
	for _, f := range body.Embedded.Facts {
		if _, ok := f.Links["self"]; !ok {
			t.Errorf("fact %q missing self link", f.Name)
		}
	}
}

func TestHandleTopicFacts_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "ontology",
		providers: storeProviders{
			topicLister: &stubTopicLister{},
		},
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
		providers: storeProviders{
			topicLister: &stubTopicLister{},
		},
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
		providers: storeProviders{
			topicLister: &stubTopicLister{},
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/ai/stats", nil)
	r.ServeHTTP(rec, req)

	// With no store the provider returns zero stats — still 200, valid JSON.
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
// defaultStatsProvider{}.Stats directly and shares statsView, but has its own
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
		providers: storeProviders{
			topicLister: &stubTopicLister{},
		},
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
	lister := &stubTopicLister{
		dirs: []store.DirEntry{
			{Name: "secret.md", IsDir: false},
		},
		byPath: map[string]*store.FactWithBody{
			"ontology/.drafts/secret.md": {FactRecord: store.FactRecord{Title: "Secret", Type: "observation"}},
		},
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
		providers: storeProviders{
			topicLister: lister,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/.drafts/facts", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Facts []struct {
				Name string `json:"name"`
			} `json:"facts"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 0 {
		t.Fatalf("count: %d, want 0 — a private topic's facts must not be listed", body.Count)
	}
}

// TestHandleTopicNode_StillWorksAfterDispatch ensures the existing topic-node
// behaviour is unaffected when no sub-resource suffix is present.
func TestHandleTopicNode_StillWorksAfterDispatch(t *testing.T) {
	lister := &stubTopicLister{
		dirs: []store.DirEntry{{Name: "ml", IsDir: true}},
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
		providers: storeProviders{
			topicLister: lister,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/ai", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
}
