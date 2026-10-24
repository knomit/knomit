package kb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/model"
)

// The fixtures below are COPIES of web's of the same name
// (handlers_fact_write_unavailable_test.go there), whose HTTP tests keep
// theirs; serveWrite here takes the handler under test instead of a Server.

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

func serveWrite(t *testing.T, h http.Handler, method, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, url, nil)
	} else {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	h.ServeHTTP(rec, req)
	return rec
}

// A repo whose lifecycle tore its store down (Acquire: ErrRepoClosed), with
// the closed instance still where the repo middleware finds it — a request
// that resolved it before teardown. Teardown exits the Identify stage before
// it detaches the store, and Identify's exit forgets the ontology, so
// WritableBranch refuses EVERY branch first: the write is a 403 from
// httpx.RefuseUnwritableBranch, before any helper runs. What matters is that it is
// not a success and nothing reaches git: afterwards the repo's database holds
// the seeded fact unchanged and no commit was added on the branch.
//
// The helpers themselves, on this same closed instance, return ErrRepoClosed,
// and httpx.WriteStoreError answers it 503 — the answer a write gets when teardown
// starts after the eligibility check has passed.
func TestFactWrite_ClosedRepo_RefusedAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	m := newRealManagerInHome(t, home)
	s := newTestRouter(m)
	createRepo(t, m, "alpha")
	ri := m.Get("alpha")
	require.NotNil(t, ri)
	branch := ri.AgentBranch()
	require.Equal(t, "machine/test", branch)
	const seeded = "kb/ai/test.md"

	// Seed through the API, so the PUT and DELETE below target a real fact.
	seed := serveWrite(t, s, http.MethodPut, "/repos/alpha/branches/machine:test/facts/"+seeded,
		`{"content":"`+testFactContent+`"}`)
	require.Equal(t, http.StatusOK, seed.Code, seed.Body.String())

	m.Remove("alpha")
	_, _, aerr := ri.Acquire()
	require.ErrorIs(t, aerr, repos.ErrRepoClosed, "the precondition: this instance is closed")
	m.Set("alpha", ri)

	for _, tc := range unavailableWrites("machine:test", seeded) {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveWrite(t, s, tc.method, tc.url, tc.body)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			require.Equal(t, "Read-only branch", writeProblem(t, rec).Title)
		})
	}

	ctx := context.Background()
	t.Run("helpers return ErrRepoClosed, answered 503", func(t *testing.T) {
		_, err := writeFact(ctx, ri, branch, seeded, "x", "m")
		require.ErrorIs(t, err, repos.ErrRepoClosed, "writeFact")
		_, err = deleteFact(ctx, ri, branch, seeded, "m")
		require.ErrorIs(t, err, repos.ErrRepoClosed, "deleteFact")
		_, err = priorRefs(ctx, ri, branch, seeded)
		require.ErrorIs(t, err, repos.ErrRepoClosed, "priorRefs")
		_, err = factResolves(ctx, ri, branch, seeded)
		require.ErrorIs(t, err, repos.ErrRepoClosed, "factResolves")
		_, err = systemFileResolver(ri, branch)(ctx, ".knomit/ontology.yaml")
		require.ErrorIs(t, err, repos.ErrRepoClosed, "systemFileResolver")

		rec := httptest.NewRecorder()
		httpx.WriteStoreError(rec, httptest.NewRequest(http.MethodPut, "/x", nil), err, "Failed to write fact", branch)
		requireStoreUnavailable(t, rec)
	})

	// Nothing reached git: reopen the database the instance owned.
	svc, err := store.Open(m.RepoPath(ri.UID()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.OpenRepo())
	res, err := svc.Facts().ReadFact(ctx, branch, seeded, nil)
	require.NoError(t, err, "the DELETE removed nothing")
	require.Equal(t, unescape(testFactContent), res.Content, "the PUT changed nothing")
	entries, _, _, err := svc.HistoryQuery().LogPaginated(ctx, branch, "", 100, "", "", "")
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	require.Equal(t, "edit: update "+seeded+" via API", entries[0].Message,
		"the seed is still the branch's newest commit: no POST, PUT or DELETE committed")
}

// Each store-touching helper of the write path returns WithRead's refusal,
// not a zero-valued success. The HTTP tests above cannot tell priorRefs'
// error from "no prior version" on their own — with the store gone, the next
// step fails with the same 503 either way — so the helpers are pinned here.
func TestFactWriteHelpers_ReturnAcquireRefusal(t *testing.T) {
	ri := newTestManagerWithRepos(t, "alpha").Get("alpha")
	ctx := context.Background()
	const branch, path = "agent/test", "kb/ai/test.md"

	ok, err := factResolves(ctx, ri, branch, path)
	require.ErrorIs(t, err, repos.ErrStoreUnavailable, "factResolves")
	require.False(t, ok)

	ok, err = systemFileResolver(ri, branch)(ctx, ".knomit/ontology.yaml")
	require.ErrorIs(t, err, repos.ErrStoreUnavailable, "systemFileResolver")
	require.False(t, ok)

	prior, err := priorRefs(ctx, ri, branch, path)
	require.ErrorIs(t, err, repos.ErrStoreUnavailable, "priorRefs: not \"no prior version\"")
	require.Nil(t, prior)

	h, err := writeFact(ctx, ri, branch, path, "x", "m")
	require.ErrorIs(t, err, repos.ErrStoreUnavailable, "writeFact")
	require.Empty(t, h)

	h, err = deleteFact(ctx, ri, branch, path, "m")
	require.ErrorIs(t, err, repos.ErrStoreUnavailable, "deleteFact")
	require.NotErrorIs(t, err, errFactNotFound, "deleteFact: nothing was looked up, so not a 404")
	require.Empty(t, h)
}

// httpx.StoreUnavailable is exactly Acquire's refusals, however wrapped — including
// the populating one, which wraps ErrStoreUnavailable — and nothing else.
func TestStoreUnavailable_MatchesEveryAcquireRefusal(t *testing.T) {
	require.True(t, httpx.StoreUnavailable(repos.ErrStoreUnavailable))
	require.True(t, httpx.StoreUnavailable(repos.ErrRepoClosed))
	// The populating refusal's shape (repos' errPopulating) and the gate's
	// wrapping of a resolver error.
	require.True(t, httpx.StoreUnavailable(fmt.Errorf("%w: repo is populating", repos.ErrStoreUnavailable)))
	require.True(t, httpx.StoreUnavailable(fmt.Errorf("ref gate: resolve(kb/x.md): %w", repos.ErrRepoClosed)))
	require.False(t, httpx.StoreUnavailable(errors.New(repos.ErrStoreUnavailable.Error())),
		"matched by identity, not by text")
	require.False(t, httpx.StoreUnavailable(model.ErrPathNotFound))
	require.False(t, httpx.StoreUnavailable(errFactNotFound))
}
