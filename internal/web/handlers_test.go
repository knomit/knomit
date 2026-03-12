package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/git"
	"knomit/internal/store"
)

// --- mock implementations ---

type mockGitStore struct {
	listDirFn    func(path string) ([]git.DirEntry, error)
	readFileFn   func(path string) (string, error)
	logFn        func(path string) ([]git.LogEntry, error)
	headCommitFn func() (string, error)
	branchFn     func() string
	listAllFn    func() ([]string, error)
	syncFn       func(remoteAuth interface{}) (git.SyncResult, error)
}

func (m *mockGitStore) ListDir(path string) ([]git.DirEntry, error) {
	if m.listDirFn != nil {
		return m.listDirFn(path)
	}
	return nil, nil
}

func (m *mockGitStore) ReadFile(path string) (string, error) {
	if m.readFileFn != nil {
		return m.readFileFn(path)
	}
	return "", nil
}

func (m *mockGitStore) Log(path string) ([]git.LogEntry, error) {
	if m.logFn != nil {
		return m.logFn(path)
	}
	return nil, nil
}

func (m *mockGitStore) HeadCommit() (string, error) {
	if m.headCommitFn != nil {
		return m.headCommitFn()
	}
	return "abc123", nil
}

func (m *mockGitStore) Branch() string {
	if m.branchFn != nil {
		return m.branchFn()
	}
	return "agent/test"
}

func (m *mockGitStore) ListAll() ([]string, error) {
	if m.listAllFn != nil {
		return m.listAllFn()
	}
	return nil, nil
}

func (m *mockGitStore) Sync(remoteAuth interface{}) (git.SyncResult, error) {
	if m.syncFn != nil {
		return m.syncFn(remoteAuth)
	}
	return git.SyncResult{}, nil
}

type mockSearchIndex struct {
	searchFn        func(q store.SearchQuery) ([]store.SearchResult, error)
	getLastCommitFn func() (string, error)
}

func (m *mockSearchIndex) Search(q store.SearchQuery) ([]store.SearchResult, error) {
	if m.searchFn != nil {
		return m.searchFn(q)
	}
	return nil, nil
}

func (m *mockSearchIndex) GetLastCommit() (string, error) {
	if m.getLastCommitFn != nil {
		return m.getLastCommitFn()
	}
	return "idx123", nil
}

// --- helpers ---

func newTestRouter(gs GitStore, idx SearchIndex) http.Handler {
	return NewRouter(gs, idx, nil, nil, nil, false)
}

func doRequest(t *testing.T, handler http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else {
		reqBody = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, reqBody)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// --- tests ---

func TestHandleBrowse(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		entries    []git.DirEntry
		wantStatus int
		wantPath   string
		wantLen    int
	}{
		{
			name:  "default path uses know",
			query: "/api/v1/browse",
			entries: []git.DirEntry{
				{Name: "subdir", IsDir: true},
				{Name: "fact.md", IsDir: false},
			},
			wantStatus: http.StatusOK,
			wantPath:   "know",
			wantLen:    2,
		},
		{
			name:  "explicit path",
			query: "/api/v1/browse?path=know/sub",
			entries: []git.DirEntry{
				{Name: "item.md", IsDir: false},
			},
			wantStatus: http.StatusOK,
			wantPath:   "know/sub",
			wantLen:    1,
		},
		{
			name:       "empty directory",
			query:      "/api/v1/browse?path=know/empty",
			entries:    []git.DirEntry{},
			wantStatus: http.StatusOK,
			wantPath:   "know/empty",
			wantLen:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := &mockGitStore{
				listDirFn: func(path string) ([]git.DirEntry, error) {
					return tc.entries, nil
				},
			}
			handler := newTestRouter(gs, nil)
			rr := doRequest(t, handler, http.MethodGet, tc.query, "")

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tc.wantStatus)
			}

			var resp map[string]any
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if resp["path"] != tc.wantPath {
				t.Errorf("path = %q, want %q", resp["path"], tc.wantPath)
			}

			children, ok := resp["children"].([]any)
			if !ok {
				t.Fatalf("children is not an array")
			}
			if len(children) != tc.wantLen {
				t.Errorf("len(children) = %d, want %d", len(children), tc.wantLen)
			}
		})
	}
}

func TestHandleFact(t *testing.T) {
	validContent := "---\ndomain: [go]\nconfidence: 0.9\nsources: 1\nentities: [chi]\nrefs: []\n---\n# Chi Router\n\nChi is a router.\n"

	tests := []struct {
		name       string
		query      string
		content    string
		wantStatus int
		wantTitle  string
	}{
		{
			name:       "missing path returns 400",
			query:      "/api/v1/fact",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid path returns parsed fact",
			query:      "/api/v1/fact?path=know/chi.md",
			content:    validContent,
			wantStatus: http.StatusOK,
			wantTitle:  "Chi Router",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := &mockGitStore{
				readFileFn: func(path string) (string, error) {
					return tc.content, nil
				},
			}
			handler := newTestRouter(gs, nil)
			rr := doRequest(t, handler, http.MethodGet, tc.query, "")

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}

			if tc.wantTitle != "" {
				var resp map[string]any
				if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["title"] != tc.wantTitle {
					t.Errorf("title = %q, want %q", resp["title"], tc.wantTitle)
				}
			}
		})
	}
}

func TestHandleSearch(t *testing.T) {
	results := []store.SearchResult{
		{
			FactRecord: store.FactRecord{
				Path:  "know/fact.md",
				Title: "Test Fact",
				Body:  "A test fact body.",
			},
			Score: 95.0,
		},
	}

	tests := []struct {
		name       string
		query      string
		idx        SearchIndex
		wantStatus int
		wantLen    int
	}{
		{
			name:  "search with q param returns results",
			query: "/api/v1/search?q=test",
			idx: &mockSearchIndex{
				searchFn: func(q store.SearchQuery) ([]store.SearchResult, error) {
					return results, nil
				},
			},
			wantStatus: http.StatusOK,
			wantLen:    1,
		},
		{
			name:       "nil index returns 400",
			query:      "/api/v1/search?q=test",
			idx:        nil,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:  "empty results returns empty array",
			query: "/api/v1/search?q=nomatch",
			idx: &mockSearchIndex{
				searchFn: func(q store.SearchQuery) ([]store.SearchResult, error) {
					return nil, nil
				},
			},
			wantStatus: http.StatusOK,
			wantLen:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := &mockGitStore{}
			handler := newTestRouter(gs, tc.idx)
			rr := doRequest(t, handler, http.MethodGet, tc.query, "")

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}

			if tc.wantStatus == http.StatusOK {
				var resp map[string]any
				if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				arr, ok := resp["results"].([]any)
				if !ok {
					t.Fatalf("results is not an array")
				}
				if len(arr) != tc.wantLen {
					t.Errorf("len(results) = %d, want %d", len(arr), tc.wantLen)
				}
			}
		})
	}
}

func TestHandleHistory(t *testing.T) {
	logEntries := []git.LogEntry{
		{Commit: "abcd1234", Date: "2024-01-01T00:00:00Z", Message: "add fact"},
		{Commit: "efgh5678", Date: "2024-01-02T00:00:00Z", Message: "update fact"},
	}

	tests := []struct {
		name       string
		query      string
		entries    []git.LogEntry
		wantStatus int
		wantLen    int
	}{
		{
			name:       "returns log entries",
			query:      "/api/v1/history?path=know/fact.md",
			entries:    logEntries,
			wantStatus: http.StatusOK,
			wantLen:    2,
		},
		{
			name:       "empty path returns full log",
			query:      "/api/v1/history",
			entries:    logEntries[:1],
			wantStatus: http.StatusOK,
			wantLen:    1,
		},
		{
			name:       "nil entries returns empty array",
			query:      "/api/v1/history?path=know/missing.md",
			entries:    nil,
			wantStatus: http.StatusOK,
			wantLen:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := &mockGitStore{
				logFn: func(path string) ([]git.LogEntry, error) {
					return tc.entries, nil
				},
			}
			handler := newTestRouter(gs, nil)
			rr := doRequest(t, handler, http.MethodGet, tc.query, "")

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}

			var resp map[string]any
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			arr, ok := resp["entries"].([]any)
			if !ok {
				t.Fatalf("entries is not an array; got: %v", resp["entries"])
			}
			if len(arr) != tc.wantLen {
				t.Errorf("len(entries) = %d, want %d", len(arr), tc.wantLen)
			}
		})
	}
}

func TestHandleStatus(t *testing.T) {
	tests := []struct {
		name          string
		head          string
		branch        string
		indexCommit   string
		hasIdx        bool
		wantStatus    int
		wantHead      string
		wantBranch    string
		wantIdxCommit string
	}{
		{
			name:          "returns head and branch with index",
			head:          "deadbeef",
			branch:        "agent/laptop",
			indexCommit:   "cafebabe",
			hasIdx:        true,
			wantStatus:    http.StatusOK,
			wantHead:      "deadbeef",
			wantBranch:    "agent/laptop",
			wantIdxCommit: "cafebabe",
		},
		{
			name:          "nil index returns empty index_commit",
			head:          "deadbeef",
			branch:        "agent/laptop",
			hasIdx:        false,
			wantStatus:    http.StatusOK,
			wantHead:      "deadbeef",
			wantBranch:    "agent/laptop",
			wantIdxCommit: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := &mockGitStore{
				headCommitFn: func() (string, error) { return tc.head, nil },
				branchFn:     func() string { return tc.branch },
			}

			var idx SearchIndex
			if tc.hasIdx {
				idx = &mockSearchIndex{
					getLastCommitFn: func() (string, error) { return tc.indexCommit, nil },
				}
			}

			handler := newTestRouter(gs, idx)
			rr := doRequest(t, handler, http.MethodGet, "/api/v1/status", "")

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}

			var resp map[string]any
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if resp["head"] != tc.wantHead {
				t.Errorf("head = %q, want %q", resp["head"], tc.wantHead)
			}
			if resp["branch"] != tc.wantBranch {
				t.Errorf("branch = %q, want %q", resp["branch"], tc.wantBranch)
			}
			if resp["index_commit"] != tc.wantIdxCommit {
				t.Errorf("index_commit = %q, want %q", resp["index_commit"], tc.wantIdxCommit)
			}
		})
	}
}
