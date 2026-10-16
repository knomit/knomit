package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/auth"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
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
		{http.MethodPost, "/fleet/join", `{"bundle":"x"}`},
		{http.MethodPost, "/fleet/join:check", `{"bundle":"x"}`},
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

// Review N6: on the MASTER, the merge route re-judges the pushed branch at
// the expected tip against main before merging. A branch whose one commit
// edits another member's folder is refused 409 judge_refused naming
// not-owner, and the master's branch does not move; one that edits only the
// master's own folder is merged (control).
//
// The master's background merger judges the same branch and demotes (deletes)
// a refused one, so the route can find it gone (404). That is a lost race,
// not an answer: the test plants a fresh branch and asks again. What must
// never happen is a merge.
// SABOTAGE: handlePushedMerge passes main's tip instead of expected_tip to
// FleetMergeRefusal -> nothing to judge -> the not-owner branch merges -> red.
func TestFleetREST_MasterMergeIsReJudged(t *testing.T) {
	ctx := context.Background()
	s, _ := fleetCreateServer(t, "127.0.0.1:0")
	require.Equal(t, http.StatusCreated, postFleetCreate(s, `{}`, true).Code)
	m := s.Manager
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	ri := m.Get(st.FleetRepo)
	own := m.AgentID()
	plant := func(branch, path string) string {
		var tip string
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			require.NoError(t, svc.Branches().CreateBranch(ctx, branch, svc.UpstreamBranch()))
			_, err := svc.Facts().WriteFact(ctx, branch, path, "---\nkind: pragmatic\ntype: policy\n---\n# x\n\nx\n", "plant", "")
			require.NoError(t, err)
			tip, err = svc.Branches().HeadCommit(ctx, branch)
			require.NoError(t, err)
		}))
		return tip
	}
	ownTip := func() string {
		var tip string
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			var err error
			tip, err = svc.Branches().HeadCommit(ctx, ri.AgentBranch())
			require.NoError(t, err)
		}))
		return tip
	}
	r := (&Server{Manager: m, AgentBranch: "agent/rest-fleet", OntologyRoot: "kb"}).NewAPIRouter()
	merge := func(branch, tip string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/"+st.FleetRepo+"/branches/"+urlBranch(branch)+"/merge", strings.NewReader(`{"expected_tip":"`+tip+`"}`)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	before := ownTip()
	refused := false
	for i := 0; i < 20 && !refused; i++ {
		branch := fmt.Sprintf("agent/x%d", i)
		rec := merge(branch, plant(branch, "kb/members/somebody/evil.md"))
		if rec.Code == http.StatusNotFound {
			continue // the merger demoted it first
		}
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"code":"judge_refused"`)
		require.Contains(t, rec.Body.String(), "not-owner")
		refused = true
	}
	require.True(t, refused, "the route never reached a planted branch before the merger demoted it")
	require.Equal(t, before, ownTip(), "nothing was merged into the master's branch")

	rec := merge("agent/y", plant("agent/y", "kb/members/"+own+"/note.md"))
	require.Less(t, rec.Code, 300, "control: an own-folder edit is merged: %s", rec.Body.String())
	require.NotEqual(t, before, ownTip(), "control: the master's branch moved")
}

// POST /fleet/join takes the bundle alone (it names the fleet URL) and runs
// the same URL/auth edge as every other origin against that URL: cert is not
// an input (422 auth_invalid), and nothing is installed.
func TestFleetREST_JoinRefusesCertAsInput(t *testing.T) {
	s, home := fleetCreateServer(t, "127.0.0.1:0")
	f := pkitest.New(t)
	bundle := pkitest.WithURL(t, f.Bundle(t, f.Enroll(t, "rest-fleet@"+pki.AgentEmailDomain)), "https://hub.example:19279/git/fleet")
	body, err := json.Marshal(map[string]string{"bundle": string(bundle), "auth_method": "cert"})
	require.NoError(t, err)
	req := localPeer(httptest.NewRequest(http.MethodPost, "/fleet/join", strings.NewReader(string(body))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"auth_invalid"`)
	_, err = os.Stat(filepath.Join(home, "pki", pki.InstanceCertFile))
	require.ErrorIs(t, err, os.ErrNotExist, "nothing was installed")
}

// POST /fleet/enroll answers the bundle's fleet URL, and an address that is
// not one of external_addresses is 422 address_unknown listing the choices.
func TestFleetREST_EnrollNamesTheFleetURL(t *testing.T) {
	s, _ := fleetCreateServer(t, "127.0.0.1:0")
	require.Equal(t, http.StatusCreated, postFleetCreate(s, `{}`, true).Code)
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testsigner.Named("alpha").PublicKey())))
	enroll := func(extra string) *httptest.ResponseRecorder {
		req := localPeer(httptest.NewRequest(http.MethodPost, "/fleet/enroll",
			strings.NewReader(`{"agent_id":"alpha","public_key":"`+key+`"`+extra+`}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.NewAPIRouter().ServeHTTP(rec, req)
		return rec
	}
	rec := enroll(`,"address":"https://elsewhere.example"`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"address_unknown"`)
	require.Contains(t, rec.Body.String(), `"choices":["https://fleet-host.example:19279"]`)

	rec = enroll(``)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out struct {
		FleetURL string `json:"fleet_url"`
		Bundle   string `json:"bundle"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "https://fleet-host.example:19279/git/fleet", out.FleetURL)
	b, err := pki.SplitBundle([]byte(out.Bundle))
	require.NoError(t, err)
	require.Equal(t, out.FleetURL, b.FleetURL)
}
