package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// stubFactWriter is a test stub for FactWriter.
type stubFactWriter struct {
	writeHash string
	writeErr  error
	deleteErr error
}

func (s *stubFactWriter) Write(_ context.Context, _ *repos.RepoInstance, _, _, _, _ string) (string, error) {
	return s.writeHash, s.writeErr
}

func (s *stubFactWriter) Delete(_ context.Context, _ *repos.RepoInstance, _, _, _ string) (string, error) {
	return "", s.deleteErr
}

// testFactContent is valid fact markdown: frontmatter + # heading body.
const testFactContent = "---\\ntype: observation\\ndomain: [ai]\\nconfidence: 0.9\\nsources: 1\\n---\\n\\n# Test Fact\\n\\nBody text.\\n"

func TestHandleFactUpdate_Returns200WithHAL(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factWriter: writer,
	}
	r := s.NewAPIRouter()

	body := `{"content":"` + testFactContent + `"}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/repos/alpha/branches/agent:test/facts/know/ai/test.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
}

func TestHandleFactUpdate_UnknownRepo_Returns404(t *testing.T) {
	writer := &stubFactWriter{}
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factWriter: writer,
	}
	r := s.NewAPIRouter()

	body := `{"content":"anything"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/repos/missing/branches/agent:test/facts/know/ai/test.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

func TestHandleFactUpdate_WriteError_Returns500(t *testing.T) {
	writer := &stubFactWriter{writeErr: errors.New("disk full")}
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factWriter: writer,
	}
	r := s.NewAPIRouter()

	body := `{"content":"anything"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/repos/alpha/branches/agent:test/facts/know/ai/test.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
}

func TestHandleFactDelete_Returns204(t *testing.T) {
	writer := &stubFactWriter{}
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factWriter: writer,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete,
		"/repos/alpha/branches/agent:test/facts/know/ai/test.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleFactDelete_UnknownRepo_Returns404(t *testing.T) {
	writer := &stubFactWriter{}
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factWriter: writer,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete,
		"/repos/missing/branches/agent:test/facts/know/ai/test.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}
