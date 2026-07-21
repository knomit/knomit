package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// stubDomainsProvider implements domainsProvider for tests.
type stubDomainsProvider struct {
	domains        map[string]int
	domainStatsErr error
	facts          []store.SearchResult
	domainFactsErr error
}

func (s *stubDomainsProvider) DomainStats(_ context.Context, _ *repos.RepoInstance, _ string) (map[string]int, error) {
	return s.domains, s.domainStatsErr
}

func (s *stubDomainsProvider) DomainFacts(_ context.Context, _ *repos.RepoInstance, _, _ string) ([]store.SearchResult, error) {
	return s.facts, s.domainFactsErr
}

func TestHandleHALDomains_ReturnsCollection(t *testing.T) {
	provider := &stubDomainsProvider{
		domains: map[string]int{"ai": 5, "go": 3},
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			domains: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/domains", nil)
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
			Domains []struct {
				Name  string      `json:"name"`
				Count int         `json:"count"`
				Links hal.LinkMap `json:"_links"`
			} `json:"domains"`
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
	if len(body.Embedded.Domains) != 2 {
		t.Fatalf("domains: %d, want 2", len(body.Embedded.Domains))
	}
	// Domains should be sorted alphabetically.
	if body.Embedded.Domains[0].Name != "ai" {
		t.Errorf("first domain: %q, want ai", body.Embedded.Domains[0].Name)
	}
	// Each domain should have a self link to /domains/{name}.
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/domains/ai"
	if got := body.Embedded.Domains[0].Links["self"].Href; got != wantSelf {
		t.Errorf("domain self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALDomains_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t),
		providers: storeProviders{
			domains: &stubDomainsProvider{},
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/domains", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

func TestHandleHALDomainFacts_ReturnsCollection(t *testing.T) {
	provider := &stubDomainsProvider{
		facts: []store.SearchResult{
			{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "know/ai/a.md", Title: "AI Fact", Type: "observation", Confidence: 0.9}}},
			{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "know/ai/b.md", Title: "AI Fact 2", Type: "hypothesis", Confidence: 0.7}}},
		},
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			domains: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/domains/ai", nil)
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
				Path  string      `json:"path"`
				Title string      `json:"title"`
				Links hal.LinkMap `json:"_links"`
			} `json:"facts"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count: %d, want 2", body.Count)
	}
	if len(body.Embedded.Facts) != 2 {
		t.Fatalf("facts: %d, want 2", len(body.Embedded.Facts))
	}
	// Each fact should have a self link to the fact URL.
	wantFactSelf := APIBase + "/repos/alpha/branches/agent:test/facts/know/ai/a.md"
	if got := body.Embedded.Facts[0].Links["self"].Href; got != wantFactSelf {
		t.Errorf("fact self: got %q, want %q", got, wantFactSelf)
	}
}

func TestHandleHALDomainFacts_StoreError_Returns500(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			domains: &stubDomainsProvider{domainFactsErr: errors.New("db error")},
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/domains/ai", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500", rec.Code)
	}
}
