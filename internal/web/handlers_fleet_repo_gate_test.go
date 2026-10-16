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

// User ruling 2026-10-16 (PR #528 B3): no generic repo route creates,
// changes or deletes the FLEET repository, for any caller: over the local
// socket (the desktop app, holding read, write, operator and admin), over
// loopback TCP (a browser page) and over mTLS (an enrolled peer granted write
// and operator), each mutating route of /repos/{repo} in the real route table
// (rename, archive, the repo PATCH, PUT and DELETE origin, the upstream,
// origin sessions, the pushed-branch merge, fact and experiment writes, index
// and synthesis jobs, …) answers 409 fleet_repository, one subtest per
// route, and its handler does not run: on this MEMBER's copy the fleet
// repository keeps its name, its origin is still the fleet's URL, and its
// agent branch did not move. Reads pass (GET of the repo and its origin). A
// non-fleet repository is unchanged (control: renamed over the socket).
// SABOTAGE: drop r.Use(fleetRepoRefused) from the /repos/{repo} route ->
// rename answers 200 -> red; make fleetRepoMutation return false for DELETE
// -> the archive and DELETE origin rows are not 409 -> red.
func TestFleetRepo_EveryGenericMutationIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fleetURL, root := seedFleetBare(t, dir)
	m, key := restFleetManager(t, dir)
	join := joinOverSocket(t, &Server{Manager: m, Grants: localWriteGrants()}, key, fleetURL, root)
	require.Equal(t, http.StatusOK, join.Code, join.Body.String())
	_, err := m.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	require.NoError(t, err)
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	ri := m.Get(st.FleetRepo)
	require.NotNil(t, ri)
	f := pkitest.New(t)
	peer := f.Enroll(t, "peer@agents.knomit.io")
	g := localOperatorGrants()
	g[auth.InstancePrincipal(peer.Fingerprint()).String()] = auth.Set{auth.Read: {}, auth.Write: {}, auth.Operator: {}}
	s := &Server{Manager: m, Grants: g, AgentBranch: "agent/rest-fleet", OntologyRoot: "kb"}

	routes := fleetRepoMutatingRoutes(t, s, st.FleetRepo, ri.AgentBranch())
	have := map[string]bool{}
	for _, rt := range routes {
		have[rt[0]+" "+rt[1]] = true
	}
	base := "/repos/" + st.FleetRepo
	agent := "/branches/" + urlBranch(ri.AgentBranch())
	for _, want := range []string{
		"POST " + base + "/rename", "DELETE " + base + "/", "PATCH " + base + "/", "PUT " + base + "/origin",
		"DELETE " + base + "/origin", "PATCH " + base + "/origin/upstream", "POST " + base + "/origin-sessions/",
		"POST " + base + "/origin-sessions/s1/commit", "POST " + base + agent + "/merge", "POST " + base + agent + "/facts",
		"PUT " + base + agent + "/facts/kb/x.md", "DELETE " + base + agent + "/facts/kb/x.md", "POST " + base + "/experiments",
		"POST " + base + "/index:cancel", "POST " + base + agent + "/index-rebuilds", "POST " + base + agent + "/synthesis-runs",
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
	originURL := func() string {
		var u string
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			o, err := svc.Remote().GetRemote("origin")
			require.NoError(t, err)
			require.NotNil(t, o, "the member's copy keeps its origin")
			u = o.URL
		}))
		return u
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
		case strings.HasSuffix(path, "/facts"):
			return `{"title":"T","body":"b","domain":["x"]}`
		}
		return `{}`
	}
	for _, rt := range routes {
		t.Run(rt[0]+" "+rt[1], func(t *testing.T) {
			for _, via := range []string{"socket", "tcp", "mtls"} {
				rec := send(rt[0], rt[1], body(rt[0], rt[1]), via)
				require.Equal(t, http.StatusConflict, rec.Code, "over %s: %s", via, rec.Body.String())
				require.Contains(t, rec.Body.String(), `"code":"fleet_repository"`, "over %s", via)
				require.Contains(t, rec.Body.String(), "Fleet page", "over %s: the text names the Fleet page", via)
				require.NotContains(t, rec.Body.String(), "/api/v1/", "over %s: the text names no REST route", via)
				// #529 N-c: the text names every Fleet page action that replaces a
				// generic one, the generic Rebuild naming its own.
				if strings.HasSuffix(rt[1], "/index-rebuilds") {
					require.Contains(t, rec.Body.String(), "Fleet → Settings → Index → Rebuild", "over %s", via)
				} else {
					for _, action := range []string{"Move the fleet to a forge", "Update access", "Point at the new host", "Host the fleet here", "Index → Rebuild", "Leave", "Decommission"} {
						require.Contains(t, rec.Body.String(), action, "over %s: the text names %s", via, action)
					}
				}
			}
			require.Equal(t, []string{"fleet", "kb"}, m.Names(), "the fleet repository kept its name")
			require.Equal(t, fleetURL, originURL(), "the origin is unchanged")
			require.Equal(t, before, agentTip(), "nothing was committed on the fleet repository")
		})
	}

	// Reads pass.
	for _, p := range []string{base, base + "/origin", base + agent} {
		rec := send(http.MethodGet, p, ``, "tcp")
		require.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", p, rec.Body.String())
	}
	// Control: another repository is not gated.
	rec := send(http.MethodPost, "/repos/kb/rename", `{"name":"kb2"}`, "socket")
	require.Equal(t, http.StatusOK, rec.Code, "control: a non-fleet repository renames: %s", rec.Body.String())
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
