package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// stubCommitsProvider implements commitsProvider for tests.
type stubCommitsProvider struct {
	entries    []store.LogEntryWithTags
	next       string
	prev       string
	err        error
	detail     *store.CommitDetailResult
	detailFiles []commitFileView
	detailErr  error
}

func (s *stubCommitsProvider) LogPaginated(
	_ *repos.RepoInstance, _, _ string, _ int, _, _, _ string,
) ([]store.LogEntryWithTags, string, string, error) {
	return s.entries, s.next, s.prev, s.err
}

func (s *stubCommitsProvider) CommitDetail(
	_ *repos.RepoInstance, _, _, _ string,
) (*store.CommitDetailResult, []commitFileView, error) {
	return s.detail, s.detailFiles, s.detailErr
}

func TestHandleCommitsList_ReturnsHALCollection(t *testing.T) {
	provider := &stubCommitsProvider{
		entries: []store.LogEntryWithTags{
			{
				Commit:    "abc123",
				Date:      "2024-01-01T10:00:00Z",
				Message:   "add fact about Go",
				Operation: "learn",
				Files:     store.FileCounts{Added: 1},
			},
			{
				Commit:    "def456",
				Date:      "2024-01-02T10:00:00Z",
				Message:   "update fact",
				Operation: "update",
				Files:     store.FileCounts{Modified: 2},
			},
		},
		next: "def456",
	}

	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		commitsProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits",
		nil,
	)
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
				Commit    string         `json:"commit"`
				Date      string         `json:"date"`
				Message   string         `json:"message"`
				Operation string         `json:"operation"`
				Files     store.FileCounts `json:"files"`
				Links     hal.LinkMap    `json:"_links"`
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
		t.Error("missing self link on collection")
	}
	// next cursor present → next link must exist
	if _, ok := body.Links["next"]; !ok {
		t.Error("missing next link when cursor is non-empty")
	}
	// prev cursor empty → no prev link
	if _, ok := body.Links["prev"]; ok {
		t.Error("unexpected prev link when cursor is empty")
	}
	if len(body.Embedded.Commits) != 2 {
		t.Fatalf("embedded commits: %d, want 2", len(body.Embedded.Commits))
	}

	item := body.Embedded.Commits[0]
	if item.Commit != "abc123" {
		t.Errorf("commit: %q", item.Commit)
	}
	if item.Message != "add fact about Go" {
		t.Errorf("message: %q", item.Message)
	}
	if item.Operation != "learn" {
		t.Errorf("operation: %q", item.Operation)
	}
	if item.Files.Added != 1 {
		t.Errorf("files.added: %d, want 1", item.Files.Added)
	}

	// Each item must have a _links.self pointing to the commit URL.
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/commits/abc123"
	if got := item.Links["self"].Href; got != wantSelf {
		t.Errorf("item self link: got %q, want %q", got, wantSelf)
	}
}

func TestHandleCommitDetail_ReturnsHAL(t *testing.T) {
	provider := &stubCommitsProvider{
		detail: &store.CommitDetailResult{
			Commit:    "abc123",
			Date:      "2024-01-01T10:00:00Z",
			Message:   "add fact about Go",
			Operation: "learn",
			Files: []store.ChangedFile{
				{Path: "know/go/abc123.md", Action: "added"},
			},
		},
		detailFiles: []commitFileView{
			{
				Path:   "know/go/abc123.md",
				Action: "added",
				Title:  "Go Generics",
			},
		},
	}

	s := &Server{
		Manager:         newTestManagerWithRepos(t, "alpha"),
		commitsProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/commits/abc123",
		nil,
	)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Commit    string      `json:"commit"`
		Date      string      `json:"date"`
		Message   string      `json:"message"`
		Operation string      `json:"operation"`
		Files     []struct {
			Path   string      `json:"path"`
			Action string      `json:"action"`
			Title  string      `json:"title,omitempty"`
			Links  hal.LinkMap `json:"_links,omitempty"`
		} `json:"files"`
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.Commit != "abc123" {
		t.Errorf("commit: %q", body.Commit)
	}
	if body.Message != "add fact about Go" {
		t.Errorf("message: %q", body.Message)
	}
	if body.Operation != "learn" {
		t.Errorf("operation: %q", body.Operation)
	}
	if len(body.Files) != 1 {
		t.Fatalf("files: %d, want 1", len(body.Files))
	}
	f := body.Files[0]
	if f.Path != "know/go/abc123.md" {
		t.Errorf("file path: %q", f.Path)
	}
	if f.Action != "added" {
		t.Errorf("file action: %q", f.Action)
	}
	if f.Title != "Go Generics" {
		t.Errorf("file title: %q", f.Title)
	}
	// File self link → fact URL.
	wantFactSelf := APIBase + "/repos/alpha/branches/agent:test/facts/know/go/abc123.md"
	if got := f.Links["self"].Href; got != wantFactSelf {
		t.Errorf("file self link: got %q, want %q", got, wantFactSelf)
	}

	// Response must have self and branch links.
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	if _, ok := body.Links["branch"]; !ok {
		t.Error("missing branch link")
	}
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/commits/abc123"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleCommits_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:         newTestManagerWithRepos(t),
		commitsProvider: &stubCommitsProvider{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/commits", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}
