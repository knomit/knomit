package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// stubFactsCollectionProvider implements factsCollectionProvider for tests.
type stubFactsCollectionProvider struct {
	entries []store.RecentFactEntry
	total   int
	err     error
}

func (s *stubFactsCollectionProvider) RecentFacts(
	_ *repos.RepoInstance, _, _, _ string, _, _ int, _, _, _, _, _ []string,
) ([]store.RecentFactEntry, int, error) {
	return s.entries, s.total, s.err
}

func TestHandleHALFactsCollection_ReturnsHALCollection(t *testing.T) {
	provider := &stubFactsCollectionProvider{
		entries: []store.RecentFactEntry{
			{Path: "know/a.md", Title: "Fact A", Type: "observation", CommittedAt: 1700000000, Operation: "learn"},
			{Path: "know/b.md", Title: "Fact B", Type: "hypothesis", CommittedAt: 1700000001, Operation: "update"},
		},
		total: 2,
	}
	s := &Server{
		Manager:                 newTestManagerWithRepos(t, "alpha"),
		factsCollectionProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts", nil)
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
				Path        string      `json:"path"`
				Title       string      `json:"title"`
				Type        string      `json:"type"`
				CommittedAt int64       `json:"committed_at"`
				Operation   string      `json:"operation"`
				Links       hal.LinkMap `json:"_links"`
			} `json:"facts"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count: %d, want 2", body.Count)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	if len(body.Embedded.Facts) != 2 {
		t.Fatalf("facts: %d, want 2", len(body.Embedded.Facts))
	}

	f := body.Embedded.Facts[0]
	if f.Path != "know/a.md" {
		t.Errorf("path: %q", f.Path)
	}
	if f.Operation != "learn" {
		t.Errorf("operation: %q", f.Operation)
	}
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/facts/know/a.md"
	if got := f.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALFactsCollection_Pagination(t *testing.T) {
	// 3 items total, return 1, offset=1 → both next and prev links expected.
	provider := &stubFactsCollectionProvider{
		entries: []store.RecentFactEntry{
			{Path: "know/b.md", Title: "Fact B"},
		},
		total: 3,
	}
	s := &Server{
		Manager:                 newTestManagerWithRepos(t, "alpha"),
		factsCollectionProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts?limit=1&offset=1", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}

	var body struct {
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body.Links["next"]; !ok {
		t.Error("missing next link")
	}
	if _, ok := body.Links["prev"]; !ok {
		t.Error("missing prev link")
	}
}

func TestHandleHALFactsCollection_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:                 newTestManagerWithRepos(t),
		factsCollectionProvider: &stubFactsCollectionProvider{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/facts", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

func TestHandleHALFactsCollection_StoreError_Returns500(t *testing.T) {
	s := &Server{
		Manager:                 newTestManagerWithRepos(t, "alpha"),
		factsCollectionProvider: &stubFactsCollectionProvider{err: errors.New("db error")},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500", rec.Code)
	}
}
