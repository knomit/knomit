package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	knomitfact "knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// TestHandleCommitAnchoredFact_ReturnsHALEnvelope verifies the basic
// commit-anchored fact response: as_of.commit is the pinned SHA, _links.self
// contains /commits/{sha}/, and _links.live points to the HEAD view.
func TestHandleCommitAnchoredFact_ReturnsHALEnvelope(t *testing.T) {
	f := knomitfact.NewFact("know/a.md")
	f.Title = "A Fact"
	f.Body = "Body text"
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	reader := &stubFactReader{
		fact: f,
		head: "abc123", // returned as-is for commit-anchored reads
	}
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factReader: reader,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/a.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: %q", got)
	}

	var body struct {
		Path  string      `json:"path"`
		AsOf  AsOf        `json:"as_of"`
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.AsOf.Commit != "abc123" {
		t.Errorf("as_of.commit: got %q, want %q", body.AsOf.Commit, "abc123")
	}
	if body.AsOf.Branch != "agent/test" {
		t.Errorf("as_of.branch: got %q, want %q", body.AsOf.Branch, "agent/test")
	}

	selfLink, ok := body.Links["self"]
	if !ok {
		t.Fatal("missing _links.self")
	}
	if !strings.Contains(selfLink.Href, "/commits/abc123/") {
		t.Errorf("self href %q should contain /commits/abc123/", selfLink.Href)
	}

	liveLink, ok := body.Links["live"]
	if !ok {
		t.Fatal("missing _links.live")
	}
	if strings.Contains(liveLink.Href, "/commits/") {
		t.Errorf("live href %q should not contain /commits/", liveLink.Href)
	}

	// Commit-anchored views must have both incoming and outgoing links.
	if _, ok := body.Links["incoming"]; !ok {
		t.Error("missing _links.incoming on commit-anchored view")
	}
	if _, ok := body.Links["outgoing"]; !ok {
		t.Error("missing _links.outgoing")
	}
}

// TestHandleCommitAnchoredFact_IncomingReturns200 verifies that hitting
// /commits/{sha}/facts/.../incoming returns a HAL collection (not 404).
func TestHandleCommitAnchoredFact_IncomingReturns200(t *testing.T) {
	provider := &stubFactSubProvider{
		incoming: []store.RefSummary{
			{Path: "know/b.md", Title: "B", Commit: "abc123"},
		},
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
		factReader:      &stubFactReader{readErr: errors.New("should not be called")},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/a.md/incoming", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: %q", got)
	}
}

// TestHandleCommitAnchoredFact_NotFound returns 404 problem+json when the
// reader signals the fact is absent at the pinned commit.
func TestHandleCommitAnchoredFact_NotFound(t *testing.T) {
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factReader: &stubFactReader{readErr: errFactNotFound},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/missing.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}

// TestHandleCommitAnchoredOutgoing_ReturnsCollection verifies the commit-
// anchored outgoing sub-resource returns a HAL collection with the correct
// self URL (containing /commits/{sha}/).
func TestHandleCommitAnchoredOutgoing_ReturnsCollection(t *testing.T) {
	provider := &stubFactSubProvider{
		outgoing: []store.RefSummary{
			{Path: "know/b.md", Title: "Fact B", Commit: "abc123"},
		},
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
		// factReader not needed — outgoing dispatch happens before reader call
		factReader: &stubFactReader{readErr: errors.New("should not be called")},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/a.md/outgoing", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: %q", got)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Refs []struct {
				Path  string      `json:"path"`
				Links hal.LinkMap `json:"_links"`
			} `json:"refs"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 1 {
		t.Errorf("count: got %d, want 1", body.Count)
	}

	selfLink, ok := body.Links["self"]
	if !ok {
		t.Fatal("missing _links.self")
	}
	if !strings.Contains(selfLink.Href, "/commits/abc123/") {
		t.Errorf("outgoing self href %q should contain /commits/abc123/", selfLink.Href)
	}

	// Each ref item's self link should also be commit-anchored.
	if len(body.Embedded.Refs) > 0 {
		refSelf := body.Embedded.Refs[0].Links["self"].Href
		if !strings.Contains(refSelf, "/commits/abc123/") {
			t.Errorf("ref self href %q should contain /commits/abc123/", refSelf)
		}
	}
}

// TestHandleCommitAnchoredOutgoing_UnknownRepo returns 404 problem+json.
func TestHandleCommitAnchoredOutgoing_UnknownRepo(t *testing.T) {
	s := &Server{
		Manager:         newTestManagerWithRepos(t),
		factSubProvider: &stubFactSubProvider{},
		factReader:      &stubFactReader{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/missing/branches/main/commits/abc123/facts/know/a.md/outgoing", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}
