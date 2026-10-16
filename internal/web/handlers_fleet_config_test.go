package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// POST /fleet/index-rebuild (user ruling 2026-10-16): the fleet repository's
// Rebuild, the generic index-rebuilds route being refused for it. On the
// local listener it starts the rebuild of the agent branch and links the
// job, which the generic job route (a read) serves; over TCP it is 403; the
// generic route stays 409 fleet_repository.
// SABOTAGE: move the route out of the RequireLocalVia group -> the TCP call
// is 201 -> red; rebuild the upstream instead of the agent branch -> red.
func TestFleetREST_IndexRebuild(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url, root := seedFleetBare(t, dir)
	m, key := restFleetManager(t, dir)
	join := joinOverSocket(t, &Server{Manager: m, Grants: localWriteGrants()}, key, url, root)
	require.Equal(t, http.StatusOK, join.Code, join.Body.String())
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	r := (&Server{Manager: m, Grants: localOperatorGrants(), AgentBranch: "agent/rest-fleet", OntologyRoot: "kb"}).NewAPIRouter()
	call := func(method, path string, local bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		if local {
			req = localPeer(req)
		} else {
			req = fromLoopback(req)
		}
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := call(http.MethodPost, "/fleet/index-rebuild", false)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = call(http.MethodPost, "/repos/"+st.FleetRepo+"/branches/agent:rest-fleet/index-rebuilds", true)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"fleet_repository"`)

	rec = call(http.MethodPost, "/fleet/index-rebuild", true)
	require.Contains(t, []int{http.StatusCreated, http.StatusOK}, rec.Code, rec.Body.String())
	var out struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Links  map[string]struct {
			Href string `json:"href"`
		} `json:"_links"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, "index-rebuild", out.Kind)
	require.Equal(t, st.FleetRepo, out.Repo)
	require.Equal(t, "agent/rest-fleet", out.Branch, "the agent branch, as the generic Rebuild names it")
	require.Equal(t, "/api/v1/repos/"+st.FleetRepo+"/branches/agent:rest-fleet/index-rebuilds/"+out.ID, out.Links["job"].Href)
	// The job reads through the generic route (a GET, not refused).
	rec = call(http.MethodGet, "/repos/"+st.FleetRepo+"/branches/agent:rest-fleet/index-rebuilds", true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// The fleet's refusal while the fleet repository is indexing names the
// Fleet page and carries no cancel-index link: that link was
// /repos//index:cancel, a generic route refused for the fleet repository
// anyway (#528 follow-up).
// SABOTAGE: drop the ErrIndexing arm of writeFleetError -> it falls to the
// lifecycle writer's 409 with the link -> red.
func TestFleetError_IndexingNamesTheFleetPageAndNoLink(t *testing.T) {
	rec := httptest.NewRecorder()
	writeFleetError(rec, httptest.NewRequest(http.MethodPost, "/fleet/follow", nil), fmt.Errorf("attach: %w", repos.ErrIndexing))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"indexing"`)
	require.Contains(t, rec.Body.String(), "Fleet → Settings")
	require.NotContains(t, rec.Body.String(), "index:cancel")
	require.NotContains(t, rec.Body.String(), "_links")
}
