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

// POST /fleet/host and POST /fleet/follow on a MEMBER's copy (user ruling
// 2026-10-16: the fleet operations the generic origin routes used to serve).
// Host here is refused member_keeps_origin (only the master hosts the
// fleet). Follow refuses, each before anything changes and with the origin
// unchanged afterwards:
//   - an http:// URL, origin_invalid (#527 F1; PR #528 S3: this is the
//     connect path's check now, the origin-session commit being refused for
//     the fleet repository outright);
//   - an unparseable URL with a token, origin_invalid with no userinfo in the
//     text (S6);
//   - a URL that is not an instance of this fleet (the file:// bare the fleet
//     lives on is not a peer), not_this_fleet.
//
// Over loopback TCP both routes are 403 (RequireLocalVia).
// SABOTAGE: drop refuseFleetOrigin from followFleetHost -> the http URL is
// probed and refused as not_this_fleet or unreachable instead -> red; drop
// the cert check -> the file:// URL is attached -> red; drop guardFleetDetach
// from hostFleetHere -> the member detaches -> red.
func TestFleetREST_HostAndFollowOnAMember(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url, root := seedFleetBare(t, dir)
	m, key := restFleetManager(t, dir)
	join := joinOverSocket(t, &Server{Manager: m, Grants: localWriteGrants()}, key, url, root)
	require.Equal(t, http.StatusOK, join.Code, join.Body.String())
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	ri := m.Get(st.FleetRepo)
	r := (&Server{Manager: m, Grants: localOperatorGrants(), AgentBranch: "agent/rest-fleet", OntologyRoot: "kb"}).NewAPIRouter()
	send := func(path, body string, local bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if local {
			req = localPeer(req)
		} else {
			req = fromLoopback(req)
		}
		r.ServeHTTP(rec, req)
		return rec
	}
	origin := func() string {
		var u string
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			o, err := svc.Remote().GetRemote("origin")
			require.NoError(t, err)
			require.NotNil(t, o, "the member's copy keeps an origin")
			u = o.URL
		}))
		return u
	}

	rec := send("/fleet/host", `{}`, true)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"member_keeps_origin"`)
	require.Equal(t, url, origin())

	rec = send("/fleet/follow", `{"url":"http://gitea.lan/org/fleet.git"}`, true)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"origin_invalid"`)
	require.Equal(t, url, origin())

	rec = send("/fleet/follow", `{"url":"https://u:tok@exa mple.com/x"}`, true)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"origin_invalid"`)
	require.NotContains(t, rec.Body.String(), "tok@")
	require.Equal(t, url, origin())

	rec = send("/fleet/follow", `{"url":"`+url+`"}`, true)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"not_this_fleet"`)
	require.Equal(t, url, origin())

	rec = send("/fleet/follow", `{}`, true)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"url_required"`)

	for _, p := range []string{"/fleet/host", "/fleet/follow"} {
		rec = send(p, `{"url":"https://x.example/git/fleet"}`, false)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s over TCP: %s", p, rec.Body.String())
	}
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
