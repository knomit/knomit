package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/store"
)

// --- handleBrowse error/edge cases ---

func TestHandleBrowse_ListDirError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ListDir("know/missing").Return(nil, fmt.Errorf("not found"))

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/browse?path=know/missing", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty list on error)", rr.Code)
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	children := resp["children"].([]any)
	if len(children) != 0 {
		t.Errorf("expected empty children on error, got %d", len(children))
	}
}

// --- handleFact error paths ---

func TestHandleFact_NotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ReadFile("know/missing.md").Return("", fmt.Errorf("not found"))

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/fact?path=know/missing.md", "")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandleFact_ParseError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	// Return content that will fail YAML parsing (invalid frontmatter).
	gs.EXPECT().ReadFile("know/bad.md").Return("---\ndomain: [[[invalid\n---\nbody\n", nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/fact?path=know/bad.md", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// --- handleSearch filter/error paths ---

func TestHandleSearch_WithFilters(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)

	mockIdx.EXPECT().Search(gomock.Any()).DoAndReturn(func(q store.SearchQuery) ([]store.SearchResult, error) {
		if len(q.Entities) != 2 || q.Entities[0] != "go" || q.Entities[1] != "chi" {
			t.Errorf("entities = %v, want [go chi]", q.Entities)
		}
		if len(q.Domain) != 1 || q.Domain[0] != "web" {
			t.Errorf("domain = %v, want [web]", q.Domain)
		}
		if q.MinConfidence != 0.8 {
			t.Errorf("min_confidence = %v, want 0.8", q.MinConfidence)
		}
		if q.Limit != 10 {
			t.Errorf("limit = %v, want 10", q.Limit)
		}
		if q.Path != "know/sub" {
			t.Errorf("path = %v, want know/sub", q.Path)
		}
		return []store.SearchResult{}, nil
	})

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet,
		"/api/v1/search?q=test&entities=go,chi&domain=web&min_confidence=0.8&limit=10&path=know/sub", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSearch_LimitCappedAt500(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)

	mockIdx.EXPECT().Search(gomock.Any()).DoAndReturn(func(q store.SearchQuery) ([]store.SearchResult, error) {
		if q.Limit != 500 {
			t.Errorf("limit = %d, want 500 (capped)", q.Limit)
		}
		return []store.SearchResult{}, nil
	})

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/search?q=test&limit=9999", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestHandleSearch_InvalidMinConfidence(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/search?q=test&min_confidence=bad", "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleSearch_InvalidLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/search?q=test&limit=abc", "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleSearch_IndexError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)
	mockIdx.EXPECT().Search(gomock.Any()).Return(nil, fmt.Errorf("index corrupt"))

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/search?q=test", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// --- handleHistory error path ---

func TestHandleHistory_Error(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().Log(gomock.Any()).Return(nil, fmt.Errorf("git error"))

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/history?path=know/fact.md", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// --- handleStats ---

func TestHandleStats(t *testing.T) {
	validFact := "---\ndomain: [go, web]\nconfidence: 0.9\nsources: 1\nentities: [chi]\nrefs: []\n---\n# Test\nBody.\n"
	validFact2 := "---\ndomain: [go]\nconfidence: 0.7\nsources: 1\nentities: [chi, mux]\nrefs: []\n---\n# Test2\nBody2.\n"

	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ListAll().Return([]string{"know/a.md", "know/b.md", "other/c.md"}, nil)
	gs.EXPECT().ReadFile("know/a.md").Return(validFact, nil)
	gs.EXPECT().ReadFile("know/b.md").Return(validFact2, nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/stats?path=know/", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)

	total := int(resp["total"].(float64))
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}

	domains := resp["domains"].(map[string]any)
	if domains["go"].(float64) != 2 {
		t.Errorf("domains[go] = %v, want 2", domains["go"])
	}
	if domains["web"].(float64) != 1 {
		t.Errorf("domains[web] = %v, want 1", domains["web"])
	}

	entities := resp["entities"].(map[string]any)
	if entities["chi"].(float64) != 2 {
		t.Errorf("entities[chi] = %v, want 2", entities["chi"])
	}

	avgConf := resp["avg_confidence"].(float64)
	if avgConf != 0.8 {
		t.Errorf("avg_confidence = %v, want 0.8", avgConf)
	}
}

func TestHandleStats_ListAllError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ListAll().Return(nil, fmt.Errorf("db error"))

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/stats", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestHandleStats_SkipsBadFiles(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ListAll().Return([]string{"know/good.md", "know/unreadable.md", "know/badparse.md"}, nil)
	gs.EXPECT().ReadFile("know/good.md").Return("---\ndomain: [go]\nconfidence: 0.9\nsources: 1\nentities: [x]\nrefs: []\n---\n# Good\nBody.\n", nil)
	gs.EXPECT().ReadFile("know/unreadable.md").Return("", fmt.Errorf("read error"))
	gs.EXPECT().ReadFile("know/badparse.md").Return("not valid frontmatter at all", nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/stats", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	total := int(resp["total"].(float64))
	if total != 1 {
		t.Errorf("total = %d, want 1 (only the good file)", total)
	}
}

func TestHandleStats_NoPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ListAll().Return([]string{"a.md", "b.md"}, nil)
	gs.EXPECT().ReadFile("a.md").Return("---\ndomain: [x]\nconfidence: 1.0\nsources: 1\nentities: [y]\nrefs: []\n---\n# A\nBody.\n", nil)
	gs.EXPECT().ReadFile("b.md").Return("---\ndomain: [x]\nconfidence: 0.5\nsources: 1\nentities: [z]\nrefs: []\n---\n# B\nBody.\n", nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/stats", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if int(resp["total"].(float64)) != 2 {
		t.Errorf("total = %v, want 2", resp["total"])
	}
}

// --- handleStatus error path ---

func TestHandleStatus_HeadCommitError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().HeadCommit().Return("", fmt.Errorf("no HEAD"))

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/status", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// --- handleSynthesizeStart ---

func TestHandleSynthesizeStart_NoDeps(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	handler := newTestRouter(gs, nil) // synthDeps is nil
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/synthesize", "")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestHandleSynthesizeStart_InvalidRecipe(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	hub := NewTaskHub(context.Background())
	synthDeps := &SynthDeps{Adapter: &fakeAdapter{}}
	handler := NewRouter(gs, nil, hub, synthDeps, nil, nil, false, "know")

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/synthesize", "not: valid: yaml: [[[")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

// --- handleSync ---

func TestHandleSync_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().Sync(nil).Return(git.SyncResult{Synced: true, Ahead: 3}, nil)
	gs.EXPECT().HeadCommit().Return("abcdef1234567890", nil)

	hub := NewTaskHub(context.Background())
	handler := NewRouter(gs, nil, hub, nil, nil, nil, false, "know")

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/sync", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}

	// Wait for task to finish.
	time.Sleep(100 * time.Millisecond)
}

func TestHandleSync_AlreadyUpToDate(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().Sync(nil).Return(git.SyncResult{Synced: false}, nil)
	gs.EXPECT().HeadCommit().Return("abcdef1234567890", nil)

	hub := NewTaskHub(context.Background())
	handler := NewRouter(gs, nil, hub, nil, nil, nil, false, "know")

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/sync", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	time.Sleep(100 * time.Millisecond)
}

func TestHandleSync_Error(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().Sync(nil).Return(git.SyncResult{}, fmt.Errorf("no remote"))

	hub := NewTaskHub(context.Background())
	handler := NewRouter(gs, nil, hub, nil, nil, nil, false, "know")

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/sync", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (async start)", rr.Code)
	}

	time.Sleep(100 * time.Millisecond)
}

func TestHandleSync_Conflict(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	// Block the first sync so the second hits 409.
	started := make(chan struct{})
	done := make(chan struct{})
	gs.EXPECT().Sync(nil).DoAndReturn(func(_ any) (git.SyncResult, error) {
		close(started)
		<-done
		return git.SyncResult{}, nil
	})
	gs.EXPECT().HeadCommit().Return("abc", nil).AnyTimes()

	hub := NewTaskHub(context.Background())
	handler := NewRouter(gs, nil, hub, nil, nil, nil, false, "know")

	// First sync starts.
	doRequest(t, handler, http.MethodPost, "/api/v1/sync", "")
	<-started

	// Second sync should get 409.
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/sync", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}

	close(done)
	time.Sleep(100 * time.Millisecond)
}

// --- handleEvents (SSE) ---

func TestHandleEvents_InitialStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().HeadCommit().Return("abc123", nil).AnyTimes()
	mockIdx := NewMockSearchIndex(ctrl)

	hub := NewTaskHub(context.Background())
	handler := NewRouter(gs, mockIdx, hub, nil, nil, nil, false, "know")

	// Use a context with timeout to end the SSE connection.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "event: status") {
		t.Errorf("expected SSE status event, got: %s", body)
	}
	if !strings.Contains(body, "abc123") {
		t.Errorf("expected head commit in status, got: %s", body)
	}
}

func TestHandleEvents_TaskEvents(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().HeadCommit().Return("abc123", nil).AnyTimes()
	gs.EXPECT().Sync(nil).Return(git.SyncResult{Synced: false}, nil)

	hub := NewTaskHub(context.Background())
	handler := NewRouter(gs, nil, hub, nil, nil, nil, false, "know")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	// Start a sync task after a small delay so the SSE connection is open.
	go func() {
		time.Sleep(50 * time.Millisecond)
		doRequest(t, handler, http.MethodPost, "/api/v1/sync", "")
	}()

	handler.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "event: task") {
		t.Errorf("expected SSE task event, got: %s", body)
	}
}

// --- OpenAPI / Swagger handlers ---

func TestHandleOpenAPISpec(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	handler := newTestRouter(gs, nil)

	rr := doRequest(t, handler, http.MethodGet, "/api/v1/openapi.yaml", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/yaml" {
		t.Errorf("content-type = %q, want application/yaml", ct)
	}
	if rr.Body.Len() == 0 {
		t.Error("expected non-empty body")
	}
}

func TestHandleSwaggerUI(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	handler := newTestRouter(gs, nil)

	rr := doRequest(t, handler, http.MethodGet, "/docs", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

// --- fakeAdapter for synthesize tests ---

type fakeAdapter struct{}

func (f *fakeAdapter) Complete(_ context.Context, _ string, _ []llm.Message, _ llm.CompletionOptions, _ func(string)) (string, error) {
	return "{}", nil
}

func (f *fakeAdapter) Model() string {
	return "fake-model"
}
