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
	gs.EXPECT().ListDir("kb/missing").Return(nil, fmt.Errorf("not found"))

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/browse?path=kb/missing", "")

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
	gs.EXPECT().ReadFile("kb/missing.md").Return("", fmt.Errorf("not found"))

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/fact?path=kb/missing.md", "")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandleFact_NonFactFallsBackToRaw(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	// Return content without frontmatter (e.g. kb.md manifest).
	gs.EXPECT().ReadFile("kb.md").Return("# Knowledge Base\n\nRoot manifest.\n", nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/fact?path=kb.md", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["path"] != "kb.md" {
		t.Errorf("expected path kb.md, got %v", resp["path"])
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
		if q.Path != "kb/sub" {
			t.Errorf("path = %v, want kb/sub", q.Path)
		}
		return []store.SearchResult{}, nil
	})

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet,
		"/api/v1/knomit/search?q=test&entities=go,chi&domain=web&min_confidence=0.8&limit=10&path=kb/sub", "")

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
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/search?q=test&limit=9999", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestHandleSearch_InvalidMinConfidence(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/search?q=test&min_confidence=bad", "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleSearch_InvalidLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/search?q=test&limit=abc", "")

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
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/search?q=test", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// --- handleHistoryPaginated error path ---

func TestHandleHistory_Error(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().LogPaginated("kb/fact.md", 50, "").Return(nil, "", fmt.Errorf("git error"))

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/history?path=kb/fact.md", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestHandleHistoryPaginated(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	entries := []git.LogEntryWithTags{
		{Commit: "abc12345", Date: "2026-03-14T10:00:00Z", Message: "add fact", Operation: "learn"},
	}
	gs.EXPECT().LogPaginated("kb/test", 50, "").Return(entries, "def67890", nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/history?path=kb/test", "")

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["next"] != "def67890" {
		t.Errorf("expected next=def67890, got %v", resp["next"])
	}
	arr := resp["entries"].([]any)
	if len(arr) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(arr))
	}
}

func TestHandleCommitDetail(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().CommitDetail("abc12345").Return(&git.CommitDetailResult{
		Commit: "abc12345", Date: "2026-03-14T10:00:00Z", Message: "add fact",
		Operation: "learn",
		Files:     []git.ChangedFile{{Path: "kb/test.md", Action: "added"}},
	}, nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/commit?hash=abc12345", "")

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp git.CommitDetailResult
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Files) != 1 || resp.Files[0].Path != "kb/test.md" {
		t.Errorf("unexpected files: %v", resp.Files)
	}
}

func TestHandleFactAtCommit(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	factContent := "---\ndomain: [test]\nconfidence: 0.8\nsources: 1\nentities: [x]\nrefs: []\n---\n# Title\n\nBody at commit.\n"
	gs.EXPECT().ReadFileAtCommit("kb/test.md", "abc12345").Return(factContent, nil)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/fact?path=kb/test.md&commit=abc12345", "")

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// --- handleStats ---

func TestHandleStats(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)
	mockIdx.EXPECT().Stats("kb/").Return(store.StatsResult{
		Total:         2,
		AvgConfidence: 0.8,
		Domains:       map[string]int{"go": 2, "web": 1},
		Entities:      map[string]int{"chi": 2, "mux": 1},
	}, nil)

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/stats?path=kb/", "")

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

func TestHandleStats_IndexError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)
	mockIdx.EXPECT().Stats("").Return(store.StatsResult{}, fmt.Errorf("db error"))

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/stats", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestHandleStats_NoIndex(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	handler := newTestRouter(gs, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/stats", "")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestHandleStats_NoPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	mockIdx := NewMockSearchIndex(ctrl)
	mockIdx.EXPECT().Stats("").Return(store.StatsResult{
		Total:         2,
		AvgConfidence: 0.75,
		Domains:       map[string]int{"x": 2},
		Entities:      map[string]int{"y": 1, "z": 1},
	}, nil)

	handler := newTestRouter(gs, mockIdx)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/stats", "")

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
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/status", "")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// --- handleSynthesizeStart ---

func TestHandleSynthesizeStart_NoDeps(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	handler := newTestRouter(gs, nil) // synthDeps is nil
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/synthesize", "")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestHandleSynthesizeStart_NoReviewer(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	hub := NewTaskHub(context.Background())
	synthDeps := &SynthDeps{Adapter: &fakeAdapter{}}
	rm := NewRepoManager()
	rm.Set("knomit", &RepoInstance{
		Name:      "knomit",
		GS:        gs,
		Hub:       hub,
		SynthDeps: synthDeps,
	})
	handler := NewRouter(rm, nil, false, "kb")

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/synthesize", "")

	// SynthDeps without Reviewer returns 503.
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rr.Code, rr.Body.String())
	}
}

// --- handleEvents (SSE) ---

func TestHandleEvents_InitialStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().HeadCommit().Return("abc123", nil).AnyTimes()
	mockIdx := NewMockSearchIndex(ctrl)

	hub := NewTaskHub(context.Background())
	rm := NewRepoManager()
	rm.Set("knomit", &RepoInstance{
		Name: "knomit",
		GS:   gs,
		Idx:  mockIdx,
		Hub:  hub,
	})
	handler := NewRouter(rm, nil, false, "kb")

	// Use a context with timeout to end the SSE connection.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knomit/events", nil).WithContext(ctx)
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

	hub := NewTaskHub(context.Background())
	rm := NewRepoManager()
	rm.Set("knomit", &RepoInstance{
		Name: "knomit",
		GS:   gs,
		Hub:  hub,
	})
	handler := NewRouter(rm, nil, false, "kb")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knomit/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	// Start a manual task after a small delay so the SSE connection is open.
	go func() {
		time.Sleep(50 * time.Millisecond)
		hub.Start("test", func(_ context.Context, emit func(TaskEvent)) {
			emit(TaskEvent{Status: "done", Message: "test task done"})
		})
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

// --- writeTaskStarted / writeTaskConflict ---

func TestWriteTaskStarted(t *testing.T) {
	rr := httptest.NewRecorder()
	writeTaskStarted(rr, "synth", "synth-1")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["op"] != "synth" {
		t.Errorf("op = %q, want %q", resp["op"], "synth")
	}
	if resp["id"] != "synth-1" {
		t.Errorf("id = %q, want %q", resp["id"], "synth-1")
	}
	if resp["status"] != "running" {
		t.Errorf("status = %q, want %q", resp["status"], "running")
	}
}

func TestWriteTaskConflict(t *testing.T) {
	rr := httptest.NewRecorder()
	writeTaskConflict(rr, "sync", fmt.Errorf("sync is already running (sync-1)"))

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["op"] != "sync" {
		t.Errorf("op = %q, want %q", resp["op"], "sync")
	}
	if resp["status"] != "error" {
		t.Errorf("status = %q, want %q", resp["status"], "error")
	}
	msg, ok := resp["message"].(string)
	if !ok || msg == "" {
		t.Errorf("expected non-empty message, got %v", resp["message"])
	}
	if !strings.Contains(msg, "already running") {
		t.Errorf("message = %q, expected it to contain 'already running'", msg)
	}
}

// --- handleEvents SSE with SyncEvent and PushEvent ---

func TestHandleEvents_SyncAndPushEvents(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().HeadCommit().Return("abc123", nil).AnyTimes()

	hub := NewTaskHub(context.Background())
	rm := NewRepoManager()
	rm.Set("knomit", &RepoInstance{
		Name: "knomit",
		GS:   gs,
		Hub:  hub,
	})
	handler := NewRouter(rm, nil, false, "kb")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knomit/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	go func() {
		time.Sleep(50 * time.Millisecond)
		hub.BroadcastSyncOK("origin", "merge123", false)
		time.Sleep(20 * time.Millisecond)
		hub.BroadcastPushError("origin", "push failed")
	}()

	handler.ServeHTTP(rr, req)

	body := rr.Body.String()

	// Verify initial status event.
	if !strings.Contains(body, "event: status") {
		t.Errorf("expected initial status event, got: %s", body)
	}

	// Verify SyncEvent arrived.
	if !strings.Contains(body, "event: sync_ok") {
		t.Errorf("expected sync_ok event, got: %s", body)
	}
	if !strings.Contains(body, "merge123") {
		t.Errorf("expected merge commit in sync event, got: %s", body)
	}

	// Verify PushEvent arrived.
	if !strings.Contains(body, "event: push_error") {
		t.Errorf("expected push_error event, got: %s", body)
	}
	if !strings.Contains(body, "push failed") {
		t.Errorf("expected error message in push event, got: %s", body)
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
