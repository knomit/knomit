package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

func postRename(t *testing.T, r http.Handler, repo, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/"+repo+"/rename", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

func TestHandleRepoRename_InvalidNameIs422(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	rec := postRename(t, s.NewAPIRouter(), "alpha", `{"name":"Has Capitals"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
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

// TestHandleRepoRename_LostRaceIs409 covers the finding parked from Task 4's
// review: once RenameRepo returns ErrRepoNotFound because a concurrent rename
// (or removal) won the race on oldName, the repo this request was addressed to
// plainly still exists — just under a different name — so it must not be
// reported as 404. That would tell the client "this name never existed" when
// the truth is "retry, someone else changed it out from under you".
//
// Constructing this without reaching into the manager's internals is not
// practical from the HTTP layer: it requires racing two RenameRepo calls
// against the same oldName and observing the loser, which is exactly what
// internal/repos/rename_test.go's
// TestRenameRepo_ConcurrentDifferentTargets_ExactlyOneWins already does at the
// manager layer. Rather than duplicate that race here with real goroutines
// (flaky, and not testing anything the handler owns), this drives the exact
// same error the loser observes — repos.ErrRepoNotFound wrapping oldName —
// straight into renameErrStatus and asserts the mapping directly.
func TestHandleRepoRename_LostRaceIs409(t *testing.T) {
	lostRace := fmt.Errorf("%w: %q", repos.ErrRepoNotFound, "alpha")
	status, title, _ := renameErrStatus(lostRace, "alpha", "beta")
	require.Equal(t, http.StatusConflict, status)
	require.NotContains(t, title, "not found",
		"the repo this request named plainly still exists; do not tell the client otherwise")
}

// TestHandleRepoRename_ManagerStoppedIs503 covers the shutdown-grace-window
// case: a request that lands in the 5s gap between Shutdown and the deferred
// Close finds the control.db tenants already nilled out, and RenameRepo
// reports that as repos.ErrManagerStopped. This is transient and retryable —
// the same class handleHALRepoPatch already answers 503 for on
// ErrRepoClosed/ErrStoreUnavailable — not a 500 implying the server is broken.
func TestHandleRepoRename_ManagerStoppedIs503(t *testing.T) {
	status, title, _ := renameErrStatus(repos.ErrManagerStopped, "alpha", "beta")
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "Repo not ready", title)
}
