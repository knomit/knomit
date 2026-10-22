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

	"knomit/internal/corpus/repos"
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
	require.Contains(t, rec.Body.String(), "Fleet → Settings → Index → Rebuild", "the refusal names the Fleet page's Rebuild (#529 N-c)")

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
	// The link resolves (#529 S1): the generic job route (a GET, not
	// refused) reads the rebuild the lifecycle recorded on the repo's hub.
	// The server here has no job registry, as in production.
	rec = call(http.MethodGet, strings.TrimPrefix(out.Links["job"].Href, "/api/v1"), true)
	require.Equal(t, http.StatusOK, rec.Code, "GET _links.job: %s", rec.Body.String())
	var job struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &job))
	require.Equal(t, out.ID, job.ID)
	require.Equal(t, "index-rebuild", job.Kind)
	require.Contains(t, []string{"running", "done"}, job.State, rec.Body.String())
	// Another job id on the same route is still 404.
	rec = call(http.MethodGet, "/repos/"+st.FleetRepo+"/branches/agent:rest-fleet/index-rebuilds/rebuild-999", true)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
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

// POST /fleet/follow never answers a token it was given in the URL's user
// slot (#529 S2): Go's HTTP client quotes the request URL without redacting
// it, and the follow's unreachable refusal quotes that error.
// SABOTAGE: FollowFleetHost returns err without credentialFreeErr -> the
// 502 body carries LEAKTOK -> red.
func TestFleetREST_FollowAnswersNoUserinfo(t *testing.T) {
	dir := t.TempDir()
	url, root := seedFleetBare(t, dir)
	m, key := restFleetManager(t, dir)
	join := joinOverSocket(t, &Server{Manager: m, Grants: localWriteGrants()}, key, url, root)
	require.Equal(t, http.StatusOK, join.Code, join.Body.String())
	srv := httptest.NewServer(http.NotFoundHandler()) // plain http behind an https URL
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	r := (&Server{Manager: m, Grants: localOperatorGrants(), AgentBranch: "agent/rest-fleet", OntologyRoot: "kb"}).NewAPIRouter()
	rec := httptest.NewRecorder()
	req := localPeer(httptest.NewRequest(http.MethodPost, "/fleet/follow", strings.NewReader(`{"url":"https://LEAKTOK@`+hostPort+`/fleet.git"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"unreachable"`)
	require.NotContains(t, rec.Body.String(), "LEAKTOK")
	require.Contains(t, rec.Body.String(), hostPort, "the URL is still named")
}

// A follow answers what it changed (#529 S3): a credential change of the
// stored forge with the stored credential unchanged is change "unchanged",
// and nothing ran.
func TestFleetREST_FollowNamesAnUnchangedCredential(t *testing.T) {
	dir := t.TempDir()
	url, root := seedFleetBare(t, dir)
	m, key := restFleetManager(t, dir)
	join := joinOverSocket(t, &Server{Manager: m, Grants: localWriteGrants()}, key, url, root)
	require.Equal(t, http.StatusOK, join.Code, join.Body.String())
	r := (&Server{Manager: m, Grants: localOperatorGrants(), AgentBranch: "agent/rest-fleet", OntologyRoot: "kb"}).NewAPIRouter()
	rec := httptest.NewRecorder()
	req := localPeer(httptest.NewRequest(http.MethodPost, "/fleet/follow", strings.NewReader(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"change":"unchanged"`)
}
