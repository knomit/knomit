package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/store"
)

// #384 through REST: the domain becomes topic/category, so a control
// character in it reaches the store as a tree path. The store refuses it
// before the ref moves, and the handler answers 400 (the caller's path), not
// 500 (a store failure).

func restHead(t *testing.T, svc *store.Service) string {
	t.Helper()
	h, err := svc.Branches().HeadCommit(context.Background(), "agent/test")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return h
}

func TestHandleFactCreate_ControlCharacterInDomain_Returns400(t *testing.T) {
	m, svc := newExperimentRESTManager(t, "alpha")
	s := &Server{Manager: m, OntologyRoot: "know"} // default, store-backed writer
	r := s.NewAPIRouter()
	before := restHead(t, svc)

	body := `{"title":"My Fact","body":"some body","type":"observation","domain":["a\nb"],"confidence":0.9,"sources":1}`
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
	if !strings.Contains(rec.Body.String(), "contains a control character") {
		t.Fatalf("body does not name the rule: %s", rec.Body.String())
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
}
