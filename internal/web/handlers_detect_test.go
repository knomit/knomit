package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"knomit/internal/detect"
)

// scorerStub returns a fixed BlockResult slice for any input.
type scorerStub struct {
	results []detect.BlockResult
}

func (s *scorerStub) ScoreBlocks(blocks []detect.Block, intents []string) []detect.BlockResult {
	return s.results
}

func (s *scorerStub) ScoreBlocksWithNovelty(blocks []detect.Block, intents []string, _ detect.FactSearcher) []detect.BlockResult {
	return s.results
}

func TestDetectHandler_KnownProfile_ReturnsScores(t *testing.T) {
	stub := &scorerStub{results: []detect.BlockResult{
		{Index: 0, Signals: []detect.Signal{{Intent: "correction", Score: 0.87}}},
	}}
	h := handleDetect(map[string]ScorerLike{"code": stub}, nil)

	body := `{"blocks":[{"role":"user","text":"that's wrong"}],"intents":["correction"]}`
	req := httptest.NewRequest("POST", "/api/v1/profiles/code/detect", strings.NewReader(body))
	req = withURLParam(req, "profile", "code")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Blocks []detect.BlockResult `json:"blocks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Signals[0].Intent != "correction" {
		t.Errorf("unexpected response: %+v", resp.Blocks)
	}
}

func TestDetectHandler_UnknownProfile_Returns404(t *testing.T) {
	h := handleDetect(map[string]ScorerLike{"code": &scorerStub{}}, nil)

	body := `{"blocks":[],"intents":[]}`
	req := httptest.NewRequest("POST", "/api/v1/profiles/chat/detect", bytes.NewBufferString(body))
	req = withURLParam(req, "profile", "chat")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDetectHandler_MalformedBody_Returns400(t *testing.T) {
	h := handleDetect(map[string]ScorerLike{"code": &scorerStub{}}, nil)

	req := httptest.NewRequest("POST", "/api/v1/profiles/code/detect", strings.NewReader("not json"))
	req = withURLParam(req, "profile", "code")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDetectHandler_WithNovelty_PopulatesSimilarFacts(t *testing.T) {
	// Verifies that novelty_context in the request body causes the handler to
	// call ScoreBlocksWithNovelty (even when mgr is nil, searcher will be nil).
	captured := struct {
		called bool
	}{}
	stub := &scorerCaptureNovelty{flag: &captured.called}
	h := handleDetect(map[string]ScorerLike{"code": stub}, nil)

	body := `{
		"blocks":[{"role":"user","text":"hi"}],
		"intents":["correction"],
		"novelty_context":{"repo":"knomit","branch":"main"}
	}`
	req := httptest.NewRequest("POST", "/api/v1/profiles/code/detect", strings.NewReader(body))
	req = withURLParam(req, "profile", "code")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !captured.called {
		t.Error("ScoreBlocksWithNovelty was not called when novelty_context present")
	}
}

type scorerCaptureNovelty struct {
	flag *bool
}

func (s *scorerCaptureNovelty) ScoreBlocks(_ []detect.Block, _ []string) []detect.BlockResult {
	return nil
}

func (s *scorerCaptureNovelty) ScoreBlocksWithNovelty(_ []detect.Block, _ []string, _ detect.FactSearcher) []detect.BlockResult {
	*s.flag = true
	return nil
}

// withURLParam injects a chi URL param into the request context so handlers
// can read it via chi.URLParam without going through the router.
func withURLParam(r *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}
