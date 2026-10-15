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
	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/repos"
	"knomit/internal/testsupport/testsigner"
)

func fleetCreateServer(t *testing.T, addr string) (*Server, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	keyPath, err := testsigner.WriteFallbackKey(home)
	require.NoError(t, err)
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb",
			TLS:               config.TLSConfig{Dir: filepath.Join(home, "pki"), Addr: addr},
			ExternalAddresses: []string{"https://fleet-host.example:19279"}},
		AgentBranch: "agent/rest-fleet", Signer: testsigner.Signer(), KeyPath: keyPath,
		Machine: repos.Options{Synchronous: true},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	// The socket principal holds write, as an operator's grant gives it: the
	// route also passes the ordinary write gate. Spelled through LocalVia:
	// "socket" here, "pipe" on Windows.
	local := auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.LocalVia}
	return &Server{Manager: m, Grants: auth.StaticGrants{local.String(): {auth.Read: {}, auth.Write: {}}}}, home
}

func postFleetCreate(s *Server, body string, local bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/fleet/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if local {
		req = localPeer(req)
	} else {
		// Loopback TCP from a page on this machine: the anonymous loopback
		// principal, which holds write (and admin) by default.
		req.RemoteAddr = "127.0.0.1:1"
		req.Host = "localhost"
	}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, req)
	return rec
}

// POST /fleet/create mints the root key and installs trust material, so it
// is served over the LOCAL listener only: loopback TCP (which a browser page
// can reach, and whose anonymous principal holds admin) is 403 and creates
// nothing; over the socket it creates the fleet, 201, registered.
// Sabotage: mount the route without RequireLocalVia -> the TCP request
// creates the fleet -> red.
func TestFleetCreate_LocalListenerOnly(t *testing.T) {
	s, home := fleetCreateServer(t, "127.0.0.1:0")
	rec := postFleetCreate(s, `{}`, false)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "only available over the local authenticated listener", "refused by RequireLocalVia, not the write gate")
	require.Empty(t, s.Manager.Names(), "a refused create writes nothing")
	_, err := os.Stat(filepath.Join(home, pki.FleetRootDirName))
	require.ErrorIs(t, err, os.ErrNotExist, "no root was minted")

	// An mTLS peer (another instance of the fleet, the threat knomit#256
	// names), granted write so that only RequireLocalVia can refuse it.
	f := pkitest.New(t)
	peer := f.Enroll(t, "peer@agents.knomit.io")
	s.Grants.(auth.StaticGrants)[auth.InstancePrincipal(peer.Fingerprint()).String()] = auth.Set{auth.Read: {}, auth.Write: {}}
	req := httptest.NewRequest(http.MethodPost, "/fleet/create", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.7:5555"
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer.Cert}}
	req = req.WithContext(auth.TLSConnContext(req.Context(), nil))
	rec = httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "instance:"+peer.Fingerprint()+"@cert", "refused by RequireLocalVia, naming the peer")
	require.Empty(t, s.Manager.Names(), "a refused create writes nothing")

	rec = postFleetCreate(s, `{"name":"crew"}`, true)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, repos.FleetRegistered, out["state"])
	require.Equal(t, "crew", out["fleet_repo"])
	require.Equal(t, "active", out["record_state"])

	// A second one is refused with its machine code.
	rec = postFleetCreate(s, `{"name":"crew-two"}`, true)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"fleet_present"`)
}

// 201 means main holds this instance's active record: state registered AND
// record_state active. Anything else is 202.
// Sabotage: answer 201 on state registered alone -> the pending row is 201
// -> red.
func TestCreatedFleetStatus(t *testing.T) {
	for _, tc := range []struct {
		state, record string
		want          int
	}{
		{repos.FleetRegistered, "active", http.StatusCreated},
		{repos.FleetRegistered, "pending", http.StatusAccepted},
		{repos.FleetRegistering, "pending", http.StatusAccepted},
		{repos.FleetRegistering, "active", http.StatusAccepted},
	} {
		got := createdFleetStatus(repos.FleetStatus{State: tc.state, RecordState: tc.record})
		require.Equal(t, tc.want, got, "state %s, record %s", tc.state, tc.record)
	}
}

// A fleet refusal carries its status and machine code; a repo create
// refusal maps through the create table.
// Sabotage: send FleetErrors through createErrStatus -> no code, 500 -> red.
func TestFleetCreate_Refusals(t *testing.T) {
	s, _ := fleetCreateServer(t, "")
	rec := postFleetCreate(s, `{}`, true)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"listener_required"`)

	rec = postFleetCreate(s, `{"name":"bad name!","origin":{"url":"https://forge.example/fleet.git"}}`, true)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"title":"Invalid name"`)
	require.Empty(t, s.Manager.Names())
}
