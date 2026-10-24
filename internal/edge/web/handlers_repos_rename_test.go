package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

func postRename(t *testing.T, r http.Handler, repo, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/"+repo+"/rename", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

// TestHandleRepoRename_InvalidNameIs400 pins the name-grammar rejection to
// 400, matching every other name-grammar check in this package (repo
// create, lens create/patch, archive/restore) — 422 is reserved for
// cross-referential failures, not a fixed-grammar syntax check on the name
// itself.
func TestHandleRepoRename_InvalidNameIs400(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	rec := postRename(t, s.NewAPIRouter(), "alpha", `{"name":"Has Capitals"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleRepoRename_NameTakenIs409 must reach RenameRepo's own
// already-active-name check (ErrRepoExists), which needs a running registry —
// the bare-stub manager from newTestManagerWithRepos has none (its repos are
// never Start()-ed), so RenameRepo would fail earlier with ErrManagerStopped
// instead. Uses the on-disk-repo harness with a second repo created alongside
// "work".
func TestHandleRepoRename_NameTakenIs409(t *testing.T) {
	r, m := newRepoPatchServerWithManager(t)
	_, err := m.Create(context.Background(), repos.CreateSpec{Name: "other", Mode: "preset"}, nil)
	require.NoError(t, err)

	rec := postRename(t, r, "work", `{"name":"other"}`)
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
}

func TestHandleRepoRename_MissingNameIs400(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	rec := postRename(t, s.NewAPIRouter(), "alpha", `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleRepoRename_UnknownRepoIs404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	rec := postRename(t, s.NewAPIRouter(), "ghost", `{"name":"beta"}`)
	require.Equal(t, http.StatusNotFound, rec.Code, "RepoMiddleware answers this")
}

// TestHandleRepoRename_SelfLinkPointsAtTheNewName needs a rename that actually
// commits, which requires a real registry behind the manager —
// newTestManagerWithRepos only registers bare *repos.RepoInstance{} stubs, so
// it uses the on-disk-repo harness instead (see newRepoPatchServerWithManager).
func TestHandleRepoRename_SelfLinkPointsAtTheNewName(t *testing.T) {
	r, _ := newRepoPatchServerWithManager(t)
	rec := postRename(t, r, "work", `{"name":"renamed"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var body struct {
		Name  string `json:"name"`
		Links struct {
			Self struct{ Href string } `json:"self"`
		} `json:"_links"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "renamed", body.Name)
	require.Contains(t, body.Links.Self.Href, "/repos/renamed",
		"the response must address the repo by its new name")
}
