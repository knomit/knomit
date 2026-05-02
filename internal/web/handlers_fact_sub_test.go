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

// stubFactSubProvider implements factSubProvider for tests.
type stubFactSubProvider struct {
	entries    []store.LogEntryWithTags
	next       string
	prev       string
	logErr     error
	explain    store.ExplainResult
	explainErr error
	incoming   []store.RefSummary
	incomingErr error
	outgoing   []store.RefSummary
	outgoingErr error
}

func (s *stubFactSubProvider) LogPaginatedForPath(
	_ *repos.RepoInstance, _, _ string, _ int, _, _, _ string,
) ([]store.LogEntryWithTags, string, string, error) {
	return s.entries, s.next, s.prev, s.logErr
}

func (s *stubFactSubProvider) ExplainFact(
	_ *repos.RepoInstance, _, _ string,
) (store.ExplainResult, error) {
	return s.explain, s.explainErr
}

func (s *stubFactSubProvider) IncomingAtCommit(
	_ *repos.RepoInstance, _, _, _ string,
) ([]store.RefSummary, error) {
	return s.incoming, s.incomingErr
}

func (s *stubFactSubProvider) OutgoingAtCommit(
	_ *repos.RepoInstance, _, _, _ string,
) ([]store.RefSummary, error) {
	return s.outgoing, s.outgoingErr
}

func TestHandleFactCommits_ReturnsHALCollection(t *testing.T) {
	provider := &stubFactSubProvider{
		entries: []store.LogEntryWithTags{
			{Commit: "abc123", Date: "2024-01-01T10:00:00Z", Message: "learn fact", Operation: "learn"},
			{Commit: "def456", Date: "2024-01-02T10:00:00Z", Message: "update fact", Operation: "update"},
		},
		next: "def456",
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts/know/a.md/commits", nil)
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
			Commits []struct {
				Commit    string      `json:"commit"`
				Operation string      `json:"operation"`
				Links     hal.LinkMap `json:"_links"`
			} `json:"commits"`
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
	if _, ok := body.Links["next"]; !ok {
		t.Error("missing next link when cursor present")
	}
	if len(body.Embedded.Commits) != 2 {
		t.Fatalf("embedded commits: %d, want 2", len(body.Embedded.Commits))
	}
	if body.Embedded.Commits[0].Commit != "abc123" {
		t.Errorf("first commit: %q", body.Embedded.Commits[0].Commit)
	}
	// Self link on the collection must point to /facts/.../commits
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/facts/know/a.md/commits"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleFactCommits_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:         newTestManagerWithRepos(t),
		factSubProvider: &stubFactSubProvider{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/missing/branches/main/facts/know/a.md/commits", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}

func TestHandleFactIncoming_ReturnsHALCollection(t *testing.T) {
	provider := &stubFactSubProvider{
		explain: store.ExplainResult{
			Incoming: []store.RefSummary{
				{Path: "know/b.md", Title: "Fact B", Deleted: false},
				{Path: "know/c.md", Title: "Fact C (deleted)", Deleted: true},
			},
		},
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts/know/a.md/incoming", nil)
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
				Title   string      `json:"title"`
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
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/facts/know/a.md/incoming"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
	refs := body.Embedded.Refs
	if len(refs) != 2 {
		t.Fatalf("refs: %d, want 2", len(refs))
	}
	// Non-deleted ref has self link.
	if _, ok := refs[0].Links["self"]; !ok {
		t.Error("non-deleted ref missing self link")
	}
	// Deleted ref has no self link.
	if _, ok := refs[1].Links["self"]; ok {
		t.Error("deleted ref should not have self link")
	}
}

func TestHandleFactOutgoing_ReturnsHALCollection(t *testing.T) {
	provider := &stubFactSubProvider{
		explain: store.ExplainResult{
			Outgoing: []store.RefSummary{
				{Path: "know/x.md", Title: "Fact X"},
			},
		},
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts/know/a.md/outgoing", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 1 {
		t.Errorf("count: %d, want 1", body.Count)
	}
}

func TestHandleFactIncoming_StoreError_Returns500(t *testing.T) {
	provider := &stubFactSubProvider{
		explainErr: errors.New("db error"),
	}
	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		factSubProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts/know/a.md/incoming", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500", rec.Code)
	}
}

func TestBuildGraphRefItems_AnchorsToSourceCommit(t *testing.T) {
	b := hal.URLBuilder{Base: "https://k.example.com"}
	a := hal.Anchor{Branch: "main"}
	refs := []store.RefSummary{
		{Path: "kb/d.md", Title: "D", Commit: "1234abc"},
		{Path: "kb/d.md", Title: "D v2", Commit: "1236def"},
	}

	got := buildGraphRefItems(b, "alpha", a, refs)
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	if got[0].Commit != "1234abc" {
		t.Errorf("got[0].Commit: %q, want %q", got[0].Commit, "1234abc")
	}
	if got[1].Commit != "1236def" {
		t.Errorf("got[1].Commit: %q, want %q", got[1].Commit, "1236def")
	}

	wantHref0 := "https://k.example.com/repos/alpha/branches/main/commits/1234abc/facts/kb/d.md"
	if got := got[0].Links["self"].Href; got != wantHref0 {
		t.Errorf("got[0] self: %q, want %q", got, wantHref0)
	}
	wantHref1 := "https://k.example.com/repos/alpha/branches/main/commits/1236def/facts/kb/d.md"
	if got := got[1].Links["self"].Href; got != wantHref1 {
		t.Errorf("got[1] self: %q, want %q", got, wantHref1)
	}
}
