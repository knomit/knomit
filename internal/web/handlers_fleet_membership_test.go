package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/auth"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/store"
)

// Every fleet operation is served on the local listener only: over loopback
// TCP (a browser page; its anonymous principal holds write and admin) and over
// mTLS (an enrolled peer granted write) each one is 403 from RequireLocalVia,
// and its handler never runs: the instance is the master of a fleet created
// over the socket, and after every refused request that fleet, its root key
// and its installed certificate are all still here. Over the socket the
// same GET /fleet/root answers 200 (control).
// SABOTAGE: mount any one of the routes outside the RequireLocalVia group ->
// its row is not 403 (and decommission, stand down or the root GET run) ->
// red.
func TestFleetMembership_EveryOperationIsLocalOnly(t *testing.T) {
	s, home := fleetCreateServer(t, "127.0.0.1:0")
	require.Equal(t, http.StatusCreated, postFleetCreate(s, `{}`, true).Code)
	f := pkitest.New(t)
	peer := f.Enroll(t, "peer@agents.knomit.io")
	s.Grants.(auth.StaticGrants)[auth.InstancePrincipal(peer.Fingerprint()).String()] = auth.Set{auth.Read: {}, auth.Write: {}}

	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/fleet/create", `{}`},
		{http.MethodPost, "/fleet/enroll", `{"agent_id":"alpha","public_key":"ssh-ed25519 AAAA"}`},
		{http.MethodPost, "/fleet/join", `{"url":"https://forge.example/fleet.git","bundle":"x"}`},
		{http.MethodPost, "/fleet/leave", `{}`},
		{http.MethodPost, "/fleet/revoke", `{"agent_id":"alpha"}`},
		{http.MethodPost, "/fleet/decommission", `{}`},
		{http.MethodGet, "/fleet/root", ``},
		{http.MethodPut, "/fleet/root", `{"files":{}}`},
		{http.MethodDelete, "/fleet/root", ``},
	}
	for _, rt := range routes {
		for _, via := range []string{"tcp", "mtls"} {
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			if rt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if via == "tcp" {
				req.RemoteAddr, req.Host = "127.0.0.1:1", "localhost"
			} else {
				req.RemoteAddr = "10.0.0.7:5555"
				req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer.Cert}}
				req = req.WithContext(auth.TLSConnContext(req.Context(), nil))
			}
			rec := httptest.NewRecorder()
			s.NewAPIRouter().ServeHTTP(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s over %s: %s", rt.method, rt.path, via, rec.Body.String())
			require.Contains(t, rec.Body.String(), "only available over the local authenticated listener", "%s %s over %s: refused by RequireLocalVia", rt.method, rt.path, via)
			require.NotContains(t, rec.Body.String(), "BEGIN OPENSSH PRIVATE KEY", "%s %s over %s", rt.method, rt.path, via)
		}
	}
	require.Equal(t, []string{"fleet"}, s.Manager.Names(), "the fleet repository is still here")
	_, err := os.Stat(filepath.Join(home, pki.FleetRootDirName, pki.RootKeyFile))
	require.NoError(t, err, "the root key is still here")
	_, err = os.Stat(filepath.Join(home, "pki", pki.InstanceCertFile))
	require.NoError(t, err, "the certificate is still installed")

	req := localPeer(httptest.NewRequest(http.MethodGet, "/fleet/root", nil))
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var root struct {
		Files map[string]string `json:"files"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &root))
	require.Contains(t, root.Files[pki.RootKeyFile], "BEGIN OPENSSH PRIVATE KEY", "control: over the socket the handler runs")
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

// GET /fleet carries master, hosting and root_dir: on the creator of a fleet
// hosted here, master true and hosting "none".
func TestFleetREST_MasterAndHosting(t *testing.T) {
	s, home := fleetCreateServer(t, "127.0.0.1:0")
	require.Equal(t, http.StatusCreated, postFleetCreate(s, `{}`, true).Code)
	out := getFleetJSON(t, s)
	require.Equal(t, true, out["master"])
	require.Equal(t, "none", out["hosting"])
	require.Equal(t, filepath.Join(home, pki.FleetRootDirName), out["root_dir"])
}

// R3.3 on a member's copy of the fleet repository, through the routes: DELETE
// origin is refused (a member keeps its origin), PATCH origin/upstream is
// refused (the consensus branch is fixed at create), and the operator's merge
// of a pushed branch is refused (only the master merges). The origin is still
// there afterwards.
// SABOTAGE: handleHALDeleteOrigin sends DetachOrigin without DetachRepoOrigin -> 204
// and the origin is gone -> red; drop the IsFleet check in
// handleHALSetOriginUpstream -> 200 -> red; drop the FleetMergeRefusal call
// in handlePushedMerge -> the member merges -> red.
func TestFleetREST_MemberOriginAndMergeGuards(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url, root := seedFleetBare(t, dir)
	m, key := restFleetManager(t, dir)
	join := joinOverSocket(t, &Server{Manager: m, Grants: localWriteGrants()}, key, url, root)
	require.Equal(t, http.StatusOK, join.Code, join.Body.String())
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	ri := m.Get(st.FleetRepo)
	var tip string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/x", svc.UpstreamBranch()))
		_, err := svc.Facts().WriteFact(ctx, "agent/x", "kb/members/x/r.md", "---\nkind: pragmatic\ntype: policy\n---\n# x\n\nx\n", "x", "")
		require.NoError(t, err)
		tip, err = svc.Branches().HeadCommit(ctx, "agent/x")
		require.NoError(t, err)
	}))
	r := (&Server{Manager: m, AgentBranch: "agent/rest-fleet", OntologyRoot: "kb"}).NewAPIRouter()
	send := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := fromLoopback(httptest.NewRequest(method, path, strings.NewReader(body)))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := send(http.MethodDelete, "/repos/"+st.FleetRepo+"/origin", "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"member_keeps_origin"`)
	require.Contains(t, rec.Body.String(), "only the master hosts the fleet")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		o, err := svc.Remote().GetRemote("origin")
		require.NoError(t, err)
		require.NotNil(t, o, "the origin is still there")
	}))

	rec = send(http.MethodPatch, "/repos/"+st.FleetRepo+"/origin/upstream", `{"branch":"trunk"}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "consensus branch is fixed at create")

	rec = send(http.MethodPost, "/repos/"+st.FleetRepo+"/branches/"+urlBranch("agent/x")+"/merge", `{"expected_tip":"`+tip+`"}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"not_master"`)
	require.Contains(t, rec.Body.String(), "only the fleet's master merges")
}
