package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		topicLister:  lister,
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
		topicLister:  &stubTopicLister{},
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
		topicLister:  &stubTopicLister{},
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
		topicLister:  &stubTopicLister{},
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

// TestHandleTopicNode_StillWorksAfterDispatch ensures the existing topic-node
// behaviour is unaffected when no sub-resource suffix is present.
func TestHandleTopicNode_StillWorksAfterDispatch(t *testing.T) {
	lister := &stubTopicLister{
		dirs: []store.DirEntry{{Name: "ml", IsDir: true}},
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
		topicLister:  lister,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/topics/ai", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
}
