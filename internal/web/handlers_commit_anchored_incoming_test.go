package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// TestCommitAnchoredIncoming_HappyPath verifies the commit-anchored /incoming
// endpoint returns a HAL collection whose self URL is commit-pinned and whose
// embedded refs carry their own commit-pinned self links.
func TestCommitAnchoredIncoming_HappyPath(t *testing.T) {
	provider := &stubFactSubProvider{
		incoming: []store.RefSummary{
			{Path: "know/b.md", Title: "Fact B", Type: "principle", Commit: "dead001"},
			{Path: "know/c.md", Title: "Fact C (deleted)", Type: "concept", Commit: "dead002", Deleted: true},
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
				Path    string      `json:"path"`
				Type    string      `json:"type"`
				Commit  string      `json:"commit"`
				Deleted bool        `json:"deleted"`
				Links   hal.LinkMap `json:"_links"`
			} `json:"refs"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count: %d, want 2", body.Count)
	}

	// Self URL must be commit-anchored.
	selfLink, ok := body.Links["self"]
	if !ok {
		t.Fatal("missing _links.self")
	}
	if !strings.Contains(selfLink.Href, "/commits/abc123/") {
		t.Errorf("self href %q should contain /commits/abc123/", selfLink.Href)
	}
	if !strings.HasSuffix(selfLink.Href, "/incoming") {
		t.Errorf("self href %q should end with /incoming", selfLink.Href)
	}

	// Non-deleted ref carries its own commit-pinned self link.
	if len(body.Embedded.Refs) < 1 {
		t.Fatal("no embedded refs")
	}
	ref0 := body.Embedded.Refs[0]
	if ref0.Commit != "dead001" {
		t.Errorf("ref[0].commit: %q, want dead001", ref0.Commit)
	}
	if ref0.Type != "principle" {
		t.Errorf("ref[0].type: %q, want \"principle\"", ref0.Type)
	}
	refSelf, ok := ref0.Links["self"]
	if !ok {
		t.Fatal("non-deleted ref missing self link")
	}
	if !strings.Contains(refSelf.Href, "/commits/dead001/") {
		t.Errorf("ref self href %q should contain /commits/dead001/", refSelf.Href)
	}

	// Deleted ref has no self link.
	ref1 := body.Embedded.Refs[1]
	if _, ok := ref1.Links["self"]; ok {
		t.Error("deleted ref should not have self link")
	}
}

// TestCommitAnchoredIncoming_UnknownRepo returns 404 problem+json.
func TestCommitAnchoredIncoming_UnknownRepo(t *testing.T) {
	s := &Server{
		Manager:         newTestManagerWithRepos(t),
		factSubProvider: &stubFactSubProvider{},
		factReader:      &stubFactReader{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/missing/branches/main/commits/abc123/facts/know/a.md/incoming", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}
