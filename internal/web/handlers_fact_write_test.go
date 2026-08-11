package web

import (
	"context"
	"encoding/json"
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

	// writeCalls and deleteCalls count invocations, so tests can assert that a
	// rejected request never reached git.
	writeCalls  int
	deleteCalls int
}

func (s *stubFactWriter) Write(_ context.Context, _ *repos.RepoInstance, _, _, _, _ string) (string, error) {
	s.writeCalls++
	return s.writeHash, s.writeErr
}

func (s *stubFactWriter) Delete(_ context.Context, _ *repos.RepoInstance, _, _, _ string) (string, error) {
	s.deleteCalls++
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

// Unlike the create handlers, which derive their target path from
// topic/category, PUT receives the path verbatim from the caller: the path is
// the one thing the client fully controls, and this is a create too — see
// PriorRefs' "no prior version" comment — so a private segment here would
// commit a fact to git that every reader (indexer, Verify, the OKF exporter)
// then permanently skips.
func TestHandleFactUpdate_RejectsPrivatePath(t *testing.T) {
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
		"/repos/alpha/branches/agent:test/facts/kb/.drafts/test.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "private") {
		t.Errorf("problem body should name the private-path rule, got %s", rec.Body.String())
	}
	if writer.writeCalls != 0 {
		t.Errorf("writer.Write called %d times for a private path; it must never reach git", writer.writeCalls)
	}
}

// TestFactWrite_AllowsWritablePrivatePath: PUT is the REST twin of
// knomit_update, taking a fully caller-supplied path, so it carries the same
// exception as the MCP guard (internal/mcp/update.go) — .knomit/<area>/ is
// knomit's own job-state namespace, writable though excluded from discovery.
// The handler upserts (PriorRefs returns nil for a fresh path, treated as "no
// prior version" rather than an error), so this PUT both creates the fact and
// proves the write reaches the writer.
func TestFactWrite_AllowsWritablePrivatePath(t *testing.T) {
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
		"/repos/alpha/branches/agent:test/facts/.knomit/jobs/ae/crawl-state.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if writer.writeCalls != 1 {
		t.Errorf("writer.Write called %d times, want 1", writer.writeCalls)
	}
	if !strings.Contains(rec.Body.String(), "Body text.") {
		t.Errorf("response should echo the written content back, got %s", rec.Body.String())
	}
}

// TestFactWrite_RefusesOtherPrivatePaths: a private path OUTSIDE
// .knomit/<area>/ must still be refused, with the same status/envelope as
// TestHandleFactUpdate_RejectsPrivatePath and a message that names
// .knomit/<area>/ as the exception rather than the removed word "jobs".
func TestFactWrite_RefusesOtherPrivatePaths(t *testing.T) {
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
		"/repos/alpha/branches/agent:test/facts/kb/.drafts/x.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	// Decode rather than substring-match the raw body: encoding/json HTML-escapes
	// '<' and '>' by default, so the wire form is ".knomit/<area>/".
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decoding problem body: %v, body=%s", err, rec.Body.String())
	}
	if !strings.Contains(problem.Detail, ".knomit/<area>/") {
		t.Errorf("problem detail should name .knomit/<area>/ as the exception, got %q", problem.Detail)
	}
	if writer.writeCalls != 0 {
		t.Errorf("writer.Write called %d times for a refused private path; it must never reach git", writer.writeCalls)
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

// DELETE is a write, and it is the REST twin of knomit_retract — which this
// branch gated on exactly that reasoning. The handler takes the path verbatim
// and the store performs no fact-shape check, so without this guard the
// endpoint will happily remove kb/.drafts/x.md or .knomit/ontology.yaml.
// Same condition, status and problem envelope as handleFactUpdate.
func TestHandleFactDelete_RejectsPrivatePath(t *testing.T) {
	for _, path := range []string{"kb/.drafts/x.md", ".knomit/ontology.yaml", ".github/workflows/ci.yml"} {
		t.Run(path, func(t *testing.T) {
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
				"/repos/alpha/branches/agent:test/facts/"+path, nil)
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
			// Decode rather than substring-match: encoding/json HTML-escapes
			// '<' and '>', so the wire form is ".knomit/<area>/".
			var problem struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decoding problem body: %v, body=%s", err, rec.Body.String())
			}
			if !strings.Contains(problem.Detail, ".knomit/<area>/") {
				t.Errorf("problem detail should name .knomit/<area>/ as the exception, got %q", problem.Detail)
			}
			if writer.deleteCalls != 0 {
				t.Errorf("writer.Delete called %d times for a private path; it must never reach git", writer.deleteCalls)
			}
		})
	}
}

// The exception the guard must preserve: a job's own state under
// .knomit/<area>/ is writable, and deleting it is a write like any other.
func TestHandleFactDelete_AllowsWritablePrivatePath(t *testing.T) {
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
		"/repos/alpha/branches/agent:test/facts/.knomit/jobs/ae/crawl-state.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if writer.deleteCalls != 1 {
		t.Errorf("writer.Delete called %d times, want 1", writer.deleteCalls)
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
