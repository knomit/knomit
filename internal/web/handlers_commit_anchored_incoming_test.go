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

// TestCommitAnchoredIncoming_FactNotLiveAtCommit returns 404 (not an empty 200
// or a 500) when the fact is retracted/absent as of the pinned commit — the
// edges 404 in lockstep with the (no-fallback) fact read. Regression for the
// UI browsing bug where navigating to a fact at a commit it isn't live at hit
// 404/500.
func TestCommitAnchoredIncoming_FactNotLiveAtCommit(t *testing.T) {
	provider := &stubFactSubProvider{
		notLive:     true,
		incoming:    []store.RefSummary{{Path: "know/b.md", Commit: "dead001"}},
		incomingErr: errors.New("IncomingAtCommit must not be called for a non-live fact"),
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
		factReader:      &stubFactReader{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/a.md/incoming", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no fact at path") {
		t.Errorf("body should explain the fact is absent at the commit: %s", rec.Body.String())
	}
}

// TestCommitAnchoredOutgoing_FactNotLiveAtCommit mirrors the incoming case for
// the /outgoing sub-resource.
func TestCommitAnchoredOutgoing_FactNotLiveAtCommit(t *testing.T) {
	provider := &stubFactSubProvider{
		notLive:     true,
		outgoingErr: errors.New("OutgoingAtCommit must not be called for a non-live fact"),
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
		factReader:      &stubFactReader{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/a.md/outgoing", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCommitAnchoredIncoming_LivenessCheckError surfaces a genuine lookup
// failure as 500, distinct from the 404 not-live case.
func TestCommitAnchoredIncoming_LivenessCheckError(t *testing.T) {
	provider := &stubFactSubProvider{
		liveErr: errors.New("db exploded"),
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
		factReader:      &stubFactReader{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/a.md/incoming", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCommitAnchoredIncoming_FallbackBefore_RetractedStillResolves: with
// ?fallback=before the gate uses FactExistsAt, so a fact that is retracted as
// of the commit (notLive) but DID exist earlier (exists) resolves to its
// last-valid version's edges — the edges follow the fact's fallback read
// rather than 404ing.
func TestCommitAnchoredIncoming_FallbackBefore_RetractedStillResolves(t *testing.T) {
	provider := &stubFactSubProvider{
		notLive:  true, // retracted as of the commit (no-fallback would 404)
		notExist: false,
		incoming: []store.RefSummary{{Path: "know/b.md", Commit: "dead001"}},
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
		factReader:      &stubFactReader{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/a.md/incoming?fallback=before", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCommitAnchoredIncoming_FallbackBefore_NeverExisted_404: even with
// fallback, a fact that never existed in the ancestry 404s.
func TestCommitAnchoredIncoming_FallbackBefore_NeverExisted_404(t *testing.T) {
	provider := &stubFactSubProvider{
		notExist:    true,
		incomingErr: errors.New("IncomingAtCommit must not be called when the fact never existed"),
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
		factReader:      &stubFactReader{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123/facts/know/a.md/incoming?fallback=before", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
