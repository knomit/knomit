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
	"go.uber.org/mock/gomock"

	"knomit/internal/detect"
)

func TestDetectHandler_KnownProfile_ReturnsScores(t *testing.T) {
	ctrl := gomock.NewController(t)
	scorer := NewMockBlockScorer(ctrl)
	scorer.EXPECT().ScoreBlocks(gomock.Any(), gomock.Any()).Return([]detect.BlockResult{
		{Index: 0, Signals: []detect.Signal{{Intent: "correction", Score: 0.87}}},
	})

	h := handleDetect(map[string]detect.BlockScorer{"code": scorer}, nil)

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
	ctrl := gomock.NewController(t)
	scorer := NewMockBlockScorer(ctrl)
	// No EXPECT — unknown profile must short-circuit before any scoring.

	h := handleDetect(map[string]detect.BlockScorer{"code": scorer}, nil)

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
	ctrl := gomock.NewController(t)
	scorer := NewMockBlockScorer(ctrl)
	// No EXPECT — body never parses, scorer never invoked.

	h := handleDetect(map[string]detect.BlockScorer{"code": scorer}, nil)

	req := httptest.NewRequest("POST", "/api/v1/profiles/code/detect", strings.NewReader("not json"))
	req = withURLParam(req, "profile", "code")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDetectHandler_WithNovelty_RoutesToNoveltyScorer(t *testing.T) {
	// novelty_context in the body must steer the handler to
	// ScoreBlocksWithNovelty rather than ScoreBlocks. mgr is nil here so
	// searcher will be nil — the routing is what we're pinning.
	ctrl := gomock.NewController(t)
	scorer := NewMockBlockScorer(ctrl)
	scorer.EXPECT().
		ScoreBlocksWithNovelty(gomock.Any(), gomock.Any(), gomock.Nil()).
		Return(nil)

	h := handleDetect(map[string]detect.BlockScorer{"code": scorer}, nil)

	body := `{
		"blocks":[{"role":"user","text":"hi"}],
		"intents":["correction"],
		"novelty_context":{"repo":"knomit","branch":"main"}
	}`
	req := httptest.NewRequest("POST", "/api/v1/profiles/code/detect", strings.NewReader(body))
	req = withURLParam(req, "profile", "code")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// withURLParam injects a chi URL param into the request context so handlers
// can read it via chi.URLParam without going through the router.
func withURLParam(r *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}
