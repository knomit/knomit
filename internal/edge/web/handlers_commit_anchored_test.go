package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factReader: reader,
		},
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

// TestHandleCommitAnchoredFact_NotFound returns 404 problem+json when the
// reader signals the fact is absent at the pinned commit.
func TestHandleCommitAnchoredFact_NotFound(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factReader: &stubFactReader{readErr: errFactNotFound},
		},
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

// The commit-anchored /outgoing is what the version live at the anchor cites
// (newFactSubFixture): at x0 kb/t.md v1 cites o1, o2; at e14 v2 cites o1, o2
// and the retracted o5, o6. The collection is pinned to the anchor, each item
// to its target's own commit. The FactReader is a tripwire: the outgoing
// dispatch happens before the fact read. Sabotage: drop the anchor (v2 at
// x0), serve incoming, link items at the anchor, or reverse them.
func TestHandleCommitAnchoredOutgoing_ReturnsCollection(t *testing.T) {
	f := newFactSubFixture(t, false)
	s := f.anchoredServer()

	url := f.anchoredURL(fsOtherURL, "x0", "kb/t.md", "outgoing")
	body := getRefs(t, s, url)
	if got, want := body.Links["self"].Href, APIBase+url; got != want {
		t.Errorf("self: got %q, want %q", got, want)
	}
	f.requireRefs(t, body, fsOtherURL, outO12...)

	f.requireRefs(t, getRefs(t, s, f.anchoredURL(fsOtherURL, "e14", "kb/t.md", "outgoing")), fsOtherURL, outT2...)
}

// TestHandleCommitAnchoredOutgoing_UnknownRepo returns 404 problem+json.
func TestHandleCommitAnchoredOutgoing_UnknownRepo(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t),
		providers: storeProviders{
			factReader: &stubFactReader{},
		},
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

// TestCommitAnchoredFact_FallbackBefore_FactExistsAtCommit verifies that when
// ?fallback=before is set AND the fact exists at the pinned commit, the
// response uses the URL's sha as as_of.commit (no fallback fired) AND the
// reader was called with fallback=true (the handler always forwards the flag,
// even when the primary read succeeds — the reader decides whether to use it).
func TestCommitAnchoredFact_FallbackBefore_FactExistsAtCommit(t *testing.T) {
	f := knomitfact.NewFact("know/a.md")
	f.Title = "A"
	f.Body = "live body"
	f.Domain = []string{}
	f.Entities = []string{}
	f.Refs = []string{}

	reader := &stubFactReader{
		fact: f,
		head: "abc1234", // matches the URL sha — primary read succeeded
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factReader: reader,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc1234/facts/know/a.md?fallback=before", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if !reader.lastFallback {
		t.Errorf("reader.lastFallback: got %v, want true (handler must forward ?fallback=before)", reader.lastFallback)
	}

	var body struct {
		AsOf AsOf   `json:"as_of"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.AsOf.Commit != "abc1234" {
		t.Errorf("as_of.commit: got %q, want %q (fallback should not fire)", body.AsOf.Commit, "abc1234")
	}
	if body.Body != "live body" {
		t.Errorf("body: got %q, want primary content", body.Body)
	}
}

// TestCommitAnchoredFact_FallbackBefore_FactRetractedAtCommit verifies that
// when ?fallback=before is set AND the fact is missing at the pinned commit,
// the reader's fallback path returns the pre-retraction version, and the
// response stamps as_of.commit with that earlier source commit (not the URL
// sha). This is the History view's retract-commit happy path.
func TestCommitAnchoredFact_FallbackBefore_FactRetractedAtCommit(t *testing.T) {
	pre := knomitfact.NewFact("know/a.md")
	pre.Title = "A (pre-retraction)"
	pre.Body = "version before retract"
	pre.Domain = []string{}
	pre.Entities = []string{}
	pre.Refs = []string{}

	reader := &stubFactReader{
		// Primary read fails (file gone at the retract commit).
		readErr: errFactNotFound,
		// Fallback path returns the pre-retraction content keyed to an
		// earlier commit.
		fallbackFact:    pre,
		fallbackHead:    "deadbee", // the actual content's source commit
		useFallbackData: true,
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factReader: reader,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc1234/facts/know/a.md?fallback=before", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if !reader.lastFallback {
		t.Errorf("reader.lastFallback: got %v, want true", reader.lastFallback)
	}

	var body struct {
		AsOf AsOf   `json:"as_of"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.AsOf.Commit != "deadbee" {
		t.Errorf("as_of.commit: got %q, want %q (should reflect the fallback's source commit, not the URL sha)", body.AsOf.Commit, "deadbee")
	}
	if body.Body != "version before retract" {
		t.Errorf("body: got %q, want pre-retraction content", body.Body)
	}
}

// TestCommitAnchoredFact_NoFallback_404OnMissing verifies the default
// behavior is unchanged: without ?fallback=before, a missing fact at the
// pinned commit returns 404 problem+json. The handler must pass fallback=false
// to the reader in this case.
func TestCommitAnchoredFact_NoFallback_404OnMissing(t *testing.T) {
	reader := &stubFactReader{readErr: errFactNotFound}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factReader: reader,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc1234/facts/know/missing.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if reader.lastFallback {
		t.Errorf("reader.lastFallback: got %v, want false (no ?fallback=before in URL)", reader.lastFallback)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}

// TestCommitAnchoredFact_FallbackBefore_StillNotFound verifies that when
// ?fallback=before is set AND the fact never existed at any ancestor, the
// reader's fallback also returns errFactNotFound, and the handler returns
// 404 problem+json.
func TestCommitAnchoredFact_FallbackBefore_StillNotFound(t *testing.T) {
	reader := &stubFactReader{
		readErr:         errFactNotFound,
		fallbackErr:     errFactNotFound,
		useFallbackData: true,
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factReader: reader,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc1234/facts/know/never.md?fallback=before", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if !reader.lastFallback {
		t.Errorf("reader.lastFallback: got %v, want true", reader.lastFallback)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}
