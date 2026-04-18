package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// stubTopicLister implements TopicLister for tests.
type stubTopicLister struct {
	dirs      []store.DirEntry
	listErr   error
	byPath    map[string]*store.FactWithBody
	getErr    error
}

func (s *stubTopicLister) ListDir(_ *repos.RepoInstance, _, _ string) ([]store.DirEntry, error) {
	return s.dirs, s.listErr
}

func (s *stubTopicLister) GetByPath(_ *repos.RepoInstance, _, path string) (*store.FactWithBody, error) {
	if s.byPath != nil {
		if fb, ok := s.byPath[path]; ok {
			return fb, nil
		}
	}
	return nil, s.getErr
}

func TestHandleTopics_ReturnsCollection(t *testing.T) {
	lister := &stubTopicLister{
		dirs: []store.DirEntry{
			{Name: "ai", IsDir: true},
			{Name: "intro.md", IsDir: false},
		},
		byPath: map[string]*store.FactWithBody{
			"ontology/intro.md": {
				FactRecord: store.FactRecord{
					Title: "Introduction",
					Type:  "observation",
				},
			},
		},
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
		topicLister:  lister,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/agent:test/topics", nil)
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
			Topics []struct {
				Name  string      `json:"name"`
				IsDir bool        `json:"is_dir"`
				Type  string      `json:"type,omitempty"`
				Title string      `json:"title,omitempty"`
				Links hal.LinkMap `json:"_links"`
			} `json:"topics"`
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
	if len(body.Embedded.Topics) != 2 {
		t.Fatalf("embedded topics: %d, want 2", len(body.Embedded.Topics))
	}

	// directory entry links to /topics/{name}
	dirEntry := body.Embedded.Topics[0]
	if dirEntry.Name != "ai" || !dirEntry.IsDir {
		t.Errorf("dir entry: %+v", dirEntry)
	}
	wantDirSelf := APIBase + "/repos/alpha/branches/agent:test/topics/ai"
	if got := dirEntry.Links["self"].Href; got != wantDirSelf {
		t.Errorf("dir self: got %q, want %q", got, wantDirSelf)
	}

	// file entry links to a fact URL and has type/title enrichment
	fileEntry := body.Embedded.Topics[1]
	if fileEntry.Name != "intro.md" || fileEntry.IsDir {
		t.Errorf("file entry: %+v", fileEntry)
	}
	if fileEntry.Type != "observation" {
		t.Errorf("type: %q, want observation", fileEntry.Type)
	}
	if fileEntry.Title != "Introduction" {
		t.Errorf("title: %q, want Introduction", fileEntry.Title)
	}
	wantFileSelf := APIBase + "/repos/alpha/branches/agent:test/facts/ontology/intro.md"
	if got := fileEntry.Links["self"].Href; got != wantFileSelf {
		t.Errorf("file self: got %q, want %q", got, wantFileSelf)
	}
}

func TestHandleTopicNode_ReturnsChildren(t *testing.T) {
	lister := &stubTopicLister{
		dirs: []store.DirEntry{
			{Name: "ml", IsDir: true},
			{Name: "overview.md", IsDir: false},
		},
		byPath: map[string]*store.FactWithBody{
			"ontology/ai/overview.md": {
				FactRecord: store.FactRecord{
					Title: "AI Overview",
					Type:  "claim",
				},
			},
		},
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
		topicLister:  lister,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/agent:test/topics/ai", nil)
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
			Topics []struct {
				Name  string      `json:"name"`
				IsDir bool        `json:"is_dir"`
				Links hal.LinkMap `json:"_links"`
			} `json:"topics"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.Count != 2 {
		t.Errorf("count: %d, want 2", body.Count)
	}

	// self link on collection
	branchURL := APIBase + "/repos/alpha/branches/agent:test"
	wantSelf := branchURL + "/topics/ai"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("collection self: got %q, want %q", got, wantSelf)
	}

	// dir child links to /topics/ai/ml
	dirChild := body.Embedded.Topics[0]
	wantChildDir := branchURL + "/topics/ai/ml"
	if got := dirChild.Links["self"].Href; got != wantChildDir {
		t.Errorf("child dir self: got %q, want %q", got, wantChildDir)
	}

	// file child links to fact
	fileChild := body.Embedded.Topics[1]
	wantChildFile := branchURL + "/facts/ontology/ai/overview.md"
	if got := fileChild.Links["self"].Href; got != wantChildFile {
		t.Errorf("child file self: got %q, want %q", got, wantChildFile)
	}
}

func TestHandleTopics_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "ontology",
		topicLister:  &stubTopicLister{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/topics", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

func TestHandleTopicNode_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "ontology",
		topicLister:  &stubTopicLister{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/topics/ai", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

func TestHandleTopics_MissingBranch_Returns404(t *testing.T) {
	lister := &stubTopicLister{
		listErr: fmt.Errorf("ListDir: ref: %w", store.ErrBranchNotFound),
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "ontology",
		topicLister:  lister,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/does-not-exist/topics", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["title"] != "Branch not found" {
		t.Errorf("title = %q, want Branch not found", body["title"])
	}
}
