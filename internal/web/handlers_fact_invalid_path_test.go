package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/store"
)

// #384 through REST. The store refuses a path go-git cannot read back before
// the ref moves; the handlers answer 400 (the caller's path), not 500 (a store
// failure), and nothing is committed.

func restHead(t *testing.T, svc *store.Service) string {
	t.Helper()
	h, err := svc.Branches().HeadCommit(context.Background(), "agent/test")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return h
}

func TestHandleFactCreate_UnreadablePathInDomain_Returns400(t *testing.T) {
	for name, domain := range map[string]string{
		"line feed": `"a\nb"`,
		"git~1":     `"git~1"`,
	} {
		t.Run(name, func(t *testing.T) {
			m, svc := newExperimentRESTManager(t, "alpha")
			s := &Server{Manager: m, OntologyRoot: "know"} // default, store-backed writer
			r := s.NewAPIRouter()
			before := restHead(t, svc)

			body := `{"title":"My Fact","body":"some body","type":"observation","domain":[` + domain + `],"confidence":0.9,"sources":1}`
			rec := httptest.NewRecorder()
			req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(body)))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)

			if got := restHead(t, svc); got != before {
				t.Fatalf("a refused create moved the ref: %s -> %s", before, got)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
			}

			// The branch is not poisoned: a valid create still succeeds.
			ok := `{"title":"Good","body":"some body","type":"observation","domain":["ai"],"confidence":0.9,"sources":1}`
			rec = httptest.NewRecorder()
			req = fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/facts", strings.NewReader(ok)))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("valid create after a refused one: got %d, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// PUT reads the prior version first, so go-git's own refusal of the path
// surfaces there, before the store's write-side refusal. Both are the
// client's path, so both are 400.
func TestHandleFactUpdate_UnreadablePath_Returns400(t *testing.T) {
	for name, urlPath := range map[string]string{
		"line feed": "know/architecture/a%0Ab/x.md",
		"git~1":     "know/architecture/git~1/x.md",
	} {
		t.Run(name, func(t *testing.T) {
			m, svc := newExperimentRESTManager(t, "alpha")
			s := &Server{Manager: m, OntologyRoot: "know"}
			r := s.NewAPIRouter()
			before := restHead(t, svc)

			body := `{"content":"` + testFactContent + `"}`
			rec := httptest.NewRecorder()
			req := fromLoopback(httptest.NewRequest(http.MethodPut,
				"/repos/alpha/branches/agent:test/facts/"+urlPath, strings.NewReader(body)))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)

			if got := restHead(t, svc); got != before {
				t.Fatalf("a refused PUT moved the ref: %s -> %s", before, got)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
