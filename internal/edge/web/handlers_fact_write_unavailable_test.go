package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// A REST fact write to a repo whose store cannot be reached answers 503 and
// writes nothing. RepoInstance.WithRead does not run its closure when Acquire
// refuses (ErrStoreUnavailable — which the populating refusal wraps — or
// ErrRepoClosed), so a helper that drops WithRead's error used to report a
// zero-valued success: 201/200/204 for a write that never happened, or a 422
// blaming the client's refs.
//
// Both states below are real ones reached without a fault hook: a writable
// instance with no store attached, and an instance whose lifecycle tore its
// store down (which the eligibility gate refuses first — see that test). The
// populating refusal is not reachable from this package
// without one (it needs a create held mid-populate, which only package repos'
// machine fixtures can do); it wraps ErrStoreUnavailable, so httpx.StoreUnavailable
// answers it by the same errors.Is — TestStoreUnavailable_MatchesEveryAcquireRefusal.
//
// The request branch is the agent branch here, deliberately and unavoidably:
// an experiment branch's writability is read from the store, which is the
// thing missing, so httpx.RefuseUnwritableBranch refuses it (403) before any of the
// helpers under test run.

const (
	unavailableTitle  = "Repo not ready"
	unavailableDetail = "the repo's store is not available; try again"
)

// requireStoreUnavailable asserts the one 503 problem every write answers.
func requireStoreUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	p := writeProblem(t, rec)
	require.Equal(t, unavailableTitle, p.Title)
	require.Equal(t, unavailableDetail, p.Detail)
	require.Equal(t, http.StatusServiceUnavailable, p.Status)
}

// unavailableWrites are the requests every case sends, one per store-touching
// step: the plain write (writeFact), a new local fact ref (the gate's
// factResolves), a new .knomit/ ref (the gate's systemFileResolver), a PUT
// (priorRefs, the first store read on that path) and a DELETE (deleteFact).
func unavailableWrites(branchURL, existing string) []struct{ name, method, url, body string } {
	base := "/repos/alpha/branches/" + branchURL + "/facts"
	return []struct{ name, method, url, body string }{
		{"POST plain", http.MethodPost, base, `{"title":"T","body":"b","domain":["ai"]}`},
		{"POST fact ref", http.MethodPost, base, `{"title":"T","body":"b","domain":["ai"],"refs":["kb/ai/other.md"]}`},
		{"POST system-file ref", http.MethodPost, base, `{"title":"T","body":"b","domain":["ai"],"refs":[".knomit/ontology.yaml"]}`},
		{"PUT", http.MethodPut, base + "/" + existing, `{"content":"` + testFactContent + `"}`},
		{"DELETE", http.MethodDelete, base + "/" + existing, ""},
	}
}

func serveWrite(t *testing.T, s *Server, method, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, url, nil)
	} else {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	s.NewAPIRouter().ServeHTTP(rec, fromLoopback(req))
	return rec
}

// A writable instance with no store attached: every write is 503. Before the
// fix POST answered 201, PUT 200 and DELETE 204 here, and the two ref-carrying
// POSTs 422.
func TestFactWrite_NoStore_Answers503(t *testing.T) {
	m := newTestManagerWithRepos(t, "alpha")
	_, _, aerr := m.Get("alpha").Acquire()
	require.ErrorIs(t, aerr, repos.ErrStoreUnavailable, "the precondition: this instance has no store")
	s := &Server{Manager: m, OntologyRoot: "kb"}

	for _, tc := range unavailableWrites("agent:test", "kb/ai/test.md") {
		t.Run(tc.name, func(t *testing.T) {
			requireStoreUnavailable(t, serveWrite(t, s, tc.method, tc.url, tc.body))
		})
	}
}

// The repo PATCH (README.md / LICENSE) answers the same refusal with the same
// problem: one spelling, httpx.WriteStoreUnavailable, for every write that finds the
// store gone.
func TestRepoPatch_NoStore_Answers503(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	for _, body := range []string{`{"description":"d"}`, `{"license":"l"}`} {
		t.Run(body, func(t *testing.T) {
			requireStoreUnavailable(t, serveWrite(t, s, http.MethodPatch, "/repos/alpha", body))
		})
	}
}
