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

	// existing is the fact-path set the ref gate resolves against. Nil means
	// "nothing resolves", correct for every test that writes refs-free content.
	existing map[string]bool

	// priorRefs is what the stored version of the fact already cited, which the
	// gate exempts from re-checking.
	priorRefs []string

	// writeCalls counts Write invocations, so tests can assert that a
	// rejected request never reached git.
	writeCalls int
}

func (s *stubFactWriter) Write(_ context.Context, _ *repos.RepoInstance, _, _, _, _ string) (string, error) {
	s.writeCalls++
	return s.writeHash, s.writeErr
}

func (s *stubFactWriter) Delete(_ context.Context, _ *repos.RepoInstance, _, _, _ string) (string, error) {
	return "", s.deleteErr
}

func (s *stubFactWriter) FactResolves(_ context.Context, _ *repos.RepoInstance, _, path string) (bool, error) {
	return s.existing[strings.ToLower(path)], nil
}

func (s *stubFactWriter) PriorRefs(_ context.Context, _ *repos.RepoInstance, _, _ string) ([]string, error) {
	return s.priorRefs, nil
}

// testFactContent is valid fact markdown: frontmatter + # heading body.
const testFactContent = "---\\ntype: observation\\ndomain: [ai]\\nconfidence: 0.9\\nsources: 1\\n---\\n\\n# Test Fact\\n\\nBody text.\\n"

func TestHandleFactUpdate_Returns200WithHAL(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factWriter: writer,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factWriter: writer,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factWriter: writer,
		},
	}
	r := s.NewAPIRouter()

	// Content must parse: validation now runs before the write, so
	// unparseable content would short-circuit to 422 and never reach the
	// writer whose error this test is exercising.
	body := `{"content":"` + testFactContent + `"}`
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factWriter: writer,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factWriter: writer,
		},
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

// TestHandleFactUpdate_InvalidContent_RejectsBeforeWriting: content that
// ParseFact refuses must be rejected with 422 WITHOUT ever reaching git.
// Validating after the write commits the bad blob as the branch HEAD for that
// path while telling the client the write failed — every later read of that
// path then fails or silently drops it.
func TestHandleFactUpdate_InvalidContent_RejectsBeforeWriting(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			factWriter: writer,
		},
	}
	r := s.NewAPIRouter()

	// No frontmatter at all — ParseFact cannot build a fact from this.
	body := `{"content":"not a fact at all"}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/repos/alpha/branches/agent:test/facts/know/ai/test.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if writer.writeCalls != 0 {
		t.Errorf("writer.Write called %d times for unparseable content; the bad blob is now the branch HEAD for that path", writer.writeCalls)
	}
}
