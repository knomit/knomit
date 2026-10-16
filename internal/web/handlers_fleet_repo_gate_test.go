package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"knomit/internal/auth"
	"knomit/internal/pki/pkitest"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// fleetRepoMutatingRoutes is every mutating route under /repos/{repo} in the
// real route table (the MCP mount aside), with its params filled in for the
// fleet repository. Derived, not listed: a mutating route added later is in
// the set without anyone adding it here.
func fleetRepoMutatingRoutes(t *testing.T, s *Server, repo, branch string) [][2]string {
	t.Helper()
	var out [][2]string
	r, ok := s.NewAPIRouter().(chi.Routes)
	require.True(t, ok, "the API router is a chi router")
	require.NoError(t, chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return nil
		}
		if !strings.HasPrefix(route, "/repos/{repo}") || strings.HasSuffix(route, "/mcp") {
			return nil
		}
		p := strings.NewReplacer("{repo}", repo, "{branch}", urlBranch(branch), "{name}", "x", "{sessionID}", "s1",
			"{id}", "j1", "{sha}", "0000000", "{key}", "k", "/*", "/kb/x.md").Replace(route)
		out = append(out, [2]string{method, p})
		return nil
	}))
	sort.Slice(out, func(i, j int) bool { return out[i][1]+out[i][0] < out[j][1]+out[j][0] })
	return out
}

// Review B3 (PR #528): every mutation of the FLEET repository through a
// generic repo route is served to the local listener only. Over loopback TCP
// (a browser page: the anonymous principal holds write and admin) and over
// mTLS (an enrolled peer granted write and operator) each mutating route of
// /repos/{repo} — rename, archive, PUT and DELETE origin, the upstream,
// origin sessions, the pushed-branch merge, index jobs and the rest of the
// route table — answers 403 fleet_local_only, and its handler does not run:
// the fleet repository keeps its name, GET /fleet still names it, the hub
// still has no origin and its agent branch did not move. Over the socket the
// same routes reach their handlers (no gate answer), and a rename there
// works as it does today (renamed, then renamed back). A non-fleet repository
// is unchanged over TCP (control: renamed over loopback TCP).
// SABOTAGE: drop r.Use(fleetRepoLocalOnly) from the /repos/{repo} route ->
// rename over TCP answers 200 -> red; make fleetRepoMutation return false for
// DELETE -> archive and DELETE origin rows are not 403 -> red.
func TestFleetRepo_EveryMutationIsLocalOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := fleetCreateServer(t, "127.0.0.1:0")
	require.Equal(t, http.StatusCreated, postFleetCreate(s, `{}`, true).Code)
	_, err := s.Manager.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	require.NoError(t, err)
	st, err := s.Manager.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, "fleet", st.FleetRepo)
	ri := s.Manager.Get(st.FleetRepo)
	f := pkitest.New(t)
	peer := f.Enroll(t, "peer@agents.knomit.io")
	g := s.Grants.(auth.StaticGrants)
	g[auth.InstancePrincipal(peer.Fingerprint()).String()] = auth.Set{auth.Read: {}, auth.Write: {}, auth.Operator: {}}
	local := auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.LocalVia}
	g[local.String()] = auth.Set{auth.Read: {}, auth.Write: {}, auth.Operator: {}, auth.Admin: {}}
	s.AgentBranch, s.OntologyRoot = "agent/rest-fleet", "kb"

	routes := fleetRepoMutatingRoutes(t, s, st.FleetRepo, ri.AgentBranch())
	t.Logf("%d mutating routes under /repos/{repo}", len(routes))
	have := map[string]bool{}
	for _, rt := range routes {
		have[rt[0]+" "+rt[1]] = true
	}
	base := "/repos/" + st.FleetRepo
	for _, want := range []string{
		"POST " + base + "/rename", "DELETE " + base + "/", "PUT " + base + "/origin", "DELETE " + base + "/origin",
		"PATCH " + base + "/origin/upstream", "PATCH " + base + "/", "POST " + base + "/origin-sessions/",
		"POST " + base + "/origin-sessions/s1/commit", "POST " + base + "/branches/" + urlBranch(ri.AgentBranch()) + "/merge",
		"POST " + base + "/index:cancel",
	} {
		require.True(t, have[want], "the walk found %s (it found %v)", want, routes)
	}

	agentTip := func() string {
		var tip string
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			tip, err = svc.Branches().HeadCommit(ctx, ri.AgentBranch())
			require.NoError(t, err)
		}))
		return tip
	}
	before := agentTip()
	send := func(method, path, body, via string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		switch via {
		case "tcp":
			req.RemoteAddr, req.Host = "127.0.0.1:1", "localhost"
		case "mtls":
			req.RemoteAddr = "10.0.0.7:5555"
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer.Cert}}
			req = req.WithContext(auth.TLSConnContext(req.Context(), nil))
		default:
			req = localPeer(req)
		}
		rec := httptest.NewRecorder()
		s.NewAPIRouter().ServeHTTP(rec, req)
		return rec
	}
	body := func(method, path string) string {
		switch {
		case strings.HasSuffix(path, "/rename"):
			return `{"name":"fleet2"}`
		case method == http.MethodPut && strings.HasSuffix(path, "/origin"):
			return `{"url":"https://elsewhere.example/fleet.git"}`
		}
		return `{}`
	}
	for _, rt := range routes {
		for _, via := range []string{"tcp", "mtls"} {
			rec := send(rt[0], rt[1], body(rt[0], rt[1]), via)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s over %s: %s", rt[0], rt[1], via, rec.Body.String())
			require.Contains(t, rec.Body.String(), `"code":"fleet_local_only"`, "%s %s over %s", rt[0], rt[1], via)
		}
	}
	require.Equal(t, []string{"fleet", "kb"}, s.Manager.Names(), "the fleet repository kept its name")
	st, err = s.Manager.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, "fleet", st.FleetRepo)
	require.Equal(t, "none", st.Hosting, "the hub still has no origin")
	require.Equal(t, before, agentTip(), "nothing was committed on the fleet repository")

	// Over the socket every route reaches its handler. Renames go last-but-
	// one, so the walk runs against the name it was built for.
	for _, rt := range routes {
		if strings.HasSuffix(rt[1], "/rename") || rt[0] == http.MethodDelete && strings.HasSuffix(rt[1], base+"/") {
			continue
		}
		rec := send(rt[0], rt[1], body(rt[0], rt[1]), "local")
		require.NotContains(t, rec.Body.String(), "fleet_local_only", "%s %s over the socket reaches its handler", rt[0], rt[1])
	}
	rec := send(http.MethodPost, base+"/rename", `{"name":"fleet2"}`, "local")
	require.Equal(t, http.StatusOK, rec.Code, "over the socket the rename works as it does today: %s", rec.Body.String())
	rec = send(http.MethodPost, "/repos/fleet2/rename", `{"name":"fleet"}`, "local")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = send(http.MethodDelete, base+"/", ``, "local")
	require.NotContains(t, rec.Body.String(), "fleet_local_only", "archive over the socket reaches its own guard: %s", rec.Body.String())

	// Control: another repository is not gated.
	rec = send(http.MethodPost, "/repos/kb/rename", `{"name":"kb2"}`, "tcp")
	require.Equal(t, http.StatusOK, rec.Code, "control: a non-fleet repository renames over TCP: %s", rec.Body.String())
}

// The gate's MCP exemption matches the mount's whole shape: a fact path that
// ends in "mcp" is not the mount.
func TestIsRepoMCPMount(t *testing.T) {
	require.True(t, isRepoMCPMount("/api/v1/repos/fleet/branches/agent:x/mcp", "fleet"))
	require.True(t, isRepoMCPMount("/repos/fleet/branches/agent%2Fx/mcp/", "fleet"))
	require.False(t, isRepoMCPMount("/repos/fleet/branches/agent:x/facts/kb/mcp", "fleet"))
	require.False(t, isRepoMCPMount("/repos/fleet/rename", "fleet"))
	require.False(t, isRepoMCPMount("/repos/other/branches/b/mcp", "fleet"))
}

// GET /repos marks the fleet repository `fleet: true`, and no other row, so
// the web app keeps it out of every browse context before GET /fleet answers.
// SABOTAGE: drop Fleet from summaryFor -> the fleet row has no flag -> red.
func TestRepos_ListingMarksTheFleetRepository(t *testing.T) {
	s, _ := fleetCreateServer(t, "127.0.0.1:0")
	require.Equal(t, http.StatusCreated, postFleetCreate(s, `{}`, true).Code)
	_, err := s.Manager.Create(context.Background(), repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, localPeer(httptest.NewRequest(http.MethodGet, "/repos", nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Embedded struct {
			Repos []map[string]any `json:"repos"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	flags := map[string]any{}
	for _, r := range out.Embedded.Repos {
		flags[r["name"].(string)] = r["fleet"]
	}
	require.Equal(t, map[string]any{"fleet": true, "kb": nil}, flags)
}
