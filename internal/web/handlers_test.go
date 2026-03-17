package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/git"
	"knomit/internal/store"
)

// --- helpers ---

func newTestRouter(gs GitStore, idx SearchIndex) http.Handler {
	hub := NewTaskHub(context.Background())
	rm := NewRepoManager()
	rm.Set("knomit", &RepoInstance{
		Name: "knomit",
		GS:   gs,
		Idx:  idx,
		Hub:  hub,
	})
	return NewRouter(rm, nil, false, "kb")
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
			name:  "default path uses general",
			query: "/api/v1/knomit/browse",
			entries: []git.DirEntry{
				{Name: "subdir", IsDir: true},
				{Name: "fact.md", IsDir: false},
			},
			wantStatus: http.StatusOK,
			wantPath:   "kb",
			wantLen:    2,
		},
		{
			name:  "explicit path",
			query: "/api/v1/knomit/browse?path=kb/sub",
			entries: []git.DirEntry{
				{Name: "item.md", IsDir: false},
			},
			wantStatus: http.StatusOK,
			wantPath:   "kb/sub",
			wantLen:    1,
		},
		{
			name:       "empty directory",
			query:      "/api/v1/knomit/browse?path=kb/empty",
			entries:    []git.DirEntry{},
			wantStatus: http.StatusOK,
			wantPath:   "kb/empty",
			wantLen:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			gs := NewMockGitStore(ctrl)
			gs.EXPECT().ListDir(gomock.Any()).Return(tc.entries, nil).AnyTimes()

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
		name        string
		query       string
		content     string
		expectRead  bool
		wantStatus  int
		wantTitle   string
	}{
		{
			name:       "missing path returns 400",
			query:      "/api/v1/knomit/fact",
			expectRead: false,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid path returns parsed fact",
			query:      "/api/v1/knomit/fact?path=kb/chi.md",
			content:    validContent,
			expectRead: true,
			wantStatus: http.StatusOK,
			wantTitle:  "Chi Router",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			gs := NewMockGitStore(ctrl)
			if tc.expectRead {
				gs.EXPECT().ReadFile(gomock.Any()).Return(tc.content, nil)
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

func TestHandleFactParseError(t *testing.T) {
	// Content that fails ParseFact (missing frontmatter delimiters)
	badContent := "type: observation\ndomain: [go]\n---\n# Bad Fact\n\nBody.\n"

	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ReadFile("kb/bad.md").Return(badContent, nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/fact?path=kb/bad.md", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["parse_error"] == nil || resp["parse_error"] == "" {
		t.Errorf("expected parse_error field, got: %v", resp)
	}
	if resp["body"] != badContent {
		t.Errorf("body = %q, want raw content", resp["body"])
	}
}

func TestHandleFactWrite(t *testing.T) {
	validContent := "---\ndomain: [go]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Fixed Fact\n\nFixed body.\n"

	t.Run("write valid content returns parsed fact", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		gs := NewMockGitStore(ctrl)
		gs.EXPECT().WriteFile("kb/fact.md", validContent, gomock.Any()).Return("abc123", "def456", nil)

		handler := newTestRouter(gs, nil)
		body := `{"path":"kb/fact.md","content":` + string(mustJSON(validContent)) + `}`
		rr := doRequest(t, handler, http.MethodPut, "/api/v1/knomit/fact", body)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]any
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["title"] != "Fixed Fact" {
			t.Errorf("title = %q, want %q", resp["title"], "Fixed Fact")
		}
		if resp["parse_error"] != nil {
			t.Errorf("unexpected parse_error: %v", resp["parse_error"])
		}
	})

	t.Run("missing path returns 400", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		gs := NewMockGitStore(ctrl)
		handler := newTestRouter(gs, nil)
		rr := doRequest(t, handler, http.MethodPut, "/api/v1/knomit/fact", `{"content":"x"}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})
}

func mustJSON(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestHandleSearch(t *testing.T) {
	results := []store.SearchResult{
		{
			FactWithBody: store.FactWithBody{
				FactRecord: store.FactRecord{
					Path:  "kb/fact.md",
					Title: "Test Fact",
				},
				Body: "A test fact body.",
			},
			Score: 95.0,
		},
	}

	tests := []struct {
		name       string
		query      string
		useIdx     bool
		idxResults []store.SearchResult
		wantStatus int
		wantLen    int
	}{
		{
			name:       "search with q param returns results",
			query:      "/api/v1/knomit/search?q=test",
			useIdx:     true,
			idxResults: results,
			wantStatus: http.StatusOK,
			wantLen:    1,
		},
		{
			name:       "nil index returns 400",
			query:      "/api/v1/knomit/search?q=test",
			useIdx:     false,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty results returns empty array",
			query:      "/api/v1/knomit/search?q=nomatch",
			useIdx:     true,
			idxResults: nil,
			wantStatus: http.StatusOK,
			wantLen:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			gs := NewMockGitStore(ctrl)

			var idx SearchIndex
			if tc.useIdx {
				mockIdx := NewMockSearchIndex(ctrl)
				mockIdx.EXPECT().Search(gomock.Any()).Return(tc.idxResults, nil)
				idx = mockIdx
			}

			handler := newTestRouter(gs, idx)
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

func TestHandleSearchMinSimilarity(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)

	// Expect the Search call to receive a SearchQuery with MinSimilarity set.
	mockIdx.EXPECT().Search(gomock.Any()).DoAndReturn(func(q store.SearchQuery) ([]store.SearchResult, error) {
		if q.MinSimilarity != 0.75 {
			return nil, fmt.Errorf("expected MinSimilarity=0.75, got %v", q.MinSimilarity)
		}
		return []store.SearchResult{}, nil
	})

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/search?q=test&min_similarity=0.75", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestHandleSearchInvalidMinSimilarity(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/search?q=test&min_similarity=notanumber", "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestHandleHistory(t *testing.T) {
	logEntries := []git.LogEntryWithTags{
		{Commit: "abcd1234", Date: "2024-01-01T00:00:00Z", Message: "add fact"},
		{Commit: "efgh5678", Date: "2024-01-02T00:00:00Z", Message: "update fact"},
	}

	tests := []struct {
		name       string
		query      string
		path       string
		entries    []git.LogEntryWithTags
		wantStatus int
		wantLen    int
	}{
		{
			name:       "returns log entries",
			query:      "/api/v1/knomit/history?path=kb/fact.md",
			path:       "kb/fact.md",
			entries:    logEntries,
			wantStatus: http.StatusOK,
			wantLen:    2,
		},
		{
			name:       "empty path returns full log",
			query:      "/api/v1/knomit/history",
			path:       "",
			entries:    logEntries[:1],
			wantStatus: http.StatusOK,
			wantLen:    1,
		},
		{
			name:       "nil entries returns empty array",
			query:      "/api/v1/knomit/history?path=kb/missing.md",
			path:       "kb/missing.md",
			entries:    nil,
			wantStatus: http.StatusOK,
			wantLen:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			gs := NewMockGitStore(ctrl)
			gs.EXPECT().LogPaginated(tc.path, 50, "").Return(tc.entries, "", nil)

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
			ctrl := gomock.NewController(t)
			gs := NewMockGitStore(ctrl)
			gs.EXPECT().HeadCommit().Return(tc.head, nil)
			gs.EXPECT().Branch().Return(tc.branch)

			var idx SearchIndex
			if tc.hasIdx {
				mockIdx := NewMockSearchIndex(ctrl)
				mockIdx.EXPECT().GetLastCommit(tc.branch).Return(tc.indexCommit, nil)
				idx = mockIdx
			}

			handler := newTestRouter(gs, idx)
			rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/status", "")

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
