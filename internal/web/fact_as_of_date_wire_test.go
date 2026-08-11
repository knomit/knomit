package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// The as_of.date wire tests.
//
// fact_version_date_test.go covers versionDateFromService in isolation, which
// says nothing about whether any handler actually CALLS it. Each of the three
// stamp lines — handlers_fact_read.go, handlers_commit_anchored.go,
// handlers_lenses_read.go — is one line, individually deletable, and before
// these tests existed the whole Go suite stayed green with any of them gone.
// Every test below is written so that deleting its handler's stamp line fails
// it; that is the property to preserve when editing them.
//
// They run against REAL stores, not stubFactReader: the date comes from the
// repo's commit_log via RevisionsBefore, so a stubbed reader would report ""
// no matter what the handler did.

// seedFactRepo writes one real fact into ri on its agent branch and returns the
// commit that wrote it.
func seedFactRepo(t *testing.T, ri *repos.RepoInstance, path, title string) string {
	t.Helper()
	var commit string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NotNil(t, svc, "the test repo must have a live store")
		res, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path,
			"---\ntype: observation\nconfidence: 0.9\ndomain: [test]\n---\n# "+title+"\n\nbody\n",
			"add "+path, "")
		require.NoError(t, err)
		commit = res.CommitHash
	}))
	require.NotEmpty(t, commit)
	return commit
}

// urlBranch renders an agent branch for a {branch} path segment: the router
// takes the colon form ("machine:test") and BranchFromContext hands the handler
// back the slash form ("machine/test").
func urlBranch(b string) string { return strings.ReplaceAll(b, "/", ":") }

// requireRFC3339Date is the shared assertion: as_of.date must be present and a
// real RFC3339 timestamp, not "" and not the 1970 zero value a naive
// time.Unix(0) stamp would render.
func requireRFC3339Date(t *testing.T, got string, where string) {
	t.Helper()
	require.NotEmpty(t, got, "%s: as_of.date must be stamped on the wire", where)
	parsed, err := time.Parse(time.RFC3339, got)
	require.NoError(t, err, "%s: as_of.date must be RFC3339, got %q", where, got)
	require.WithinDuration(t, time.Now(), parsed, time.Hour,
		"%s: as_of.date must be the commit's real date", where)
}

// decodeAsOfDate pulls as_of.date out of a single-fact response body.
func decodeAsOfDate(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var body struct {
		AsOf AsOf `json:"as_of"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body=%s", rec.Body.String())
	return body.AsOf.Date
}

// TestFactAsOfDate_HEADRead pins the stamp on
// GET /repos/{repo}/branches/{branch}/facts/{path...} (handlers_fact_read.go).
func TestFactAsOfDate_HEADRead(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	ri := m.Get("alpha")
	require.NotNil(t, ri)
	seedFactRepo(t, ri, "kb/x/one.md", "One")

	r := (&Server{Manager: m, AgentBranch: "machine/test"}).NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/"+urlBranch(ri.AgentBranch())+"/facts/kb/x/one.md", nil))

	requireRFC3339Date(t, decodeAsOfDate(t, rec), "HEAD read")
}

// TestFactAsOfDate_CommitAnchoredRead pins the stamp on
// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/{path...}
// (handlers_commit_anchored.go).
func TestFactAsOfDate_CommitAnchoredRead(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	ri := m.Get("alpha")
	require.NotNil(t, ri)
	commit := seedFactRepo(t, ri, "kb/x/one.md", "One")

	r := (&Server{Manager: m, AgentBranch: "machine/test"}).NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/"+urlBranch(ri.AgentBranch())+"/commits/"+commit+"/facts/kb/x/one.md", nil))

	requireRFC3339Date(t, decodeAsOfDate(t, rec), "commit-anchored read")
}

// TestFactAsOfDate_LensReadMountFact pins the stamp on
// GET /lenses/{lens}/facts/{path...} (handlers_lenses_read.go) — and it reads a
// fact on a READ MOUNT, addressed kb://<id12>/…, on purpose.
//
// A write-repo fact would not cover the trap the stamp line's own comment
// warns about: `rel` is the MOUNT-RELATIVE path that commit_log stores, while
// view.Path is rewritten to the kb://<id12>/… wire form a few lines later. For
// the write repo the two are the same string, so a stamp that used view.Path
// would still pass. For a read mount they differ, commit_log matches no row,
// and the date silently becomes "" for every read-mount fact in the product.
// This test is the only thing standing between that swap and a green suite.
func TestFactAsOfDate_LensReadMountFact(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	beta := m.Get("beta")
	require.NotNil(t, beta)
	seedFactRepo(t, beta, "kb/y/two.md", "Two")

	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	// kb://<beta's id12>/kb/y/two.md — the read mount, not the write repo.
	qualified := "kb://" + federate.ID12(beta.ID()) + "/kb/y/two.md"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/lenses/eng/facts/"+url.PathEscape(qualified), nil))

	// Guard the guard: if the address stopped resolving to the read mount this
	// test would be asserting about the write repo (or a 404) without saying so.
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var body struct {
		Path   string `json:"path"`
		AsOf   AsOf   `json:"as_of"`
		Source struct {
			Repo string `json:"repo"`
		} `json:"source"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body=%s", rec.Body.String())
	require.Equal(t, "beta", body.Source.Repo, "must be served from the READ mount")
	require.Equal(t, qualified, body.Path, "must be the kb://-qualified wire path")

	requireRFC3339Date(t, body.AsOf.Date, "lens read-mount fact")
}
