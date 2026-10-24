package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// routeChain is one route of a walked router: "METHOD pattern" and the names
// of every middleware that runs before its handler, outermost first.
type routeChain struct {
	route string
	mws   []string
	// endpoint is the runtime name of the handler at the end of the chain.
	endpoint string
}

// walkFull is chi.Walk plus the group middleware chi.Walk drops. A subrouter
// mounted inside r.Group (r.Route or r.Mount there) is reached through a
// *chi.ChainHandler on the mount route; chi.Walk recurses into the subrouter
// and ignores that chain, so it would report the origin-session routes
// without RepoMiddleware even though every request runs it.
func walkFull(r chi.Routes, prefix string, parent []string, out *[]routeChain) {
	name := func(f any) string {
		if v := reflect.ValueOf(f); v.Kind() == reflect.Func {
			return runtime.FuncForPC(v.Pointer()).Name()
		}
		return fmt.Sprintf("%T", f)
	}
	mws := slices.Clone(parent)
	for _, m := range r.Middlewares() {
		mws = append(mws, name(m))
	}
	for _, rt := range r.Routes() {
		chainOf := func(h http.Handler) []string {
			got := slices.Clone(mws)
			if c, ok := h.(*chi.ChainHandler); ok {
				for _, m := range c.Middlewares {
					got = append(got, name(m))
				}
			}
			return got
		}
		if rt.SubRoutes != nil {
			var sub []string
			for _, h := range rt.Handlers {
				sub = chainOf(h)
				break
			}
			walkFull(rt.SubRoutes, prefix+strings.TrimSuffix(rt.Pattern, "/*"), sub, out)
			continue
		}
		for method, h := range rt.Handlers {
			if method != "*" {
				end := h
				if c, ok := h.(*chi.ChainHandler); ok {
					end = c.Endpoint
				}
				*out = append(*out, routeChain{method + " " + prefix + rt.Pattern, chainOf(h), name(end)})
			}
		}
	}
}

// The origin-session routes api/origins' Mount registers sit where the inline
// r.Route block put them before 12d: the same eight method+pattern pairs,
// under exactly the middleware of the /repos/{repo} RepoMiddleware group (the
// chain GET /repos/{repo}/branches runs), so fleetRepoRefused and
// RepoMiddleware still run before every handler.
//
// SABOTAGE: mount /origin-sessions outside the RepoMiddleware group, or drop
// a route from origins.Mount -> red.
func TestRouter_OriginSessionsMountedInTheRepoGroup(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	var all []routeChain
	walkFull(s.NewAPIRouter(), "", nil, &all)

	var group []string
	var routes []string
	chains := map[string][]string{}
	for _, rc := range all {
		if rc.route == "GET /repos/{repo}/branches" {
			group = rc.mws
		}
		if strings.Contains(rc.route, "/origin-sessions") {
			routes = append(routes, rc.route)
			chains[rc.route] = rc.mws
		}
	}
	sort.Strings(routes)
	want := []string{
		"DELETE /repos/{repo}/origin-sessions/{sessionID}",
		"GET /repos/{repo}/origin-sessions/",
		"GET /repos/{repo}/origin-sessions/{sessionID}",
		"GET /repos/{repo}/origin-sessions/{sessionID}/preview",
		"GET /repos/{repo}/origin-sessions/{sessionID}/test",
		"POST /repos/{repo}/origin-sessions/",
		"POST /repos/{repo}/origin-sessions/{sessionID}/apply",
		"POST /repos/{repo}/origin-sessions/{sessionID}/commit",
	}
	if !slices.Equal(routes, want) {
		t.Fatalf("origin-session routes\n%q\nwant\n%q", routes, want)
	}
	if !slices.ContainsFunc(group, func(n string) bool { return strings.Contains(n, ".RepoMiddleware.") }) {
		t.Fatalf("non-vacuity: the reference route's chain has no RepoMiddleware: %q", group)
	}
	for _, r := range routes {
		if !slices.Equal(chains[r], group) {
			t.Errorf("%s runs\n%q\nwant the /repos/{repo} RepoMiddleware group's chain\n%q", r, chains[r], group)
		}
	}
}

// Mount creates the session manager its routes use, and the Server builds the
// subtree once, so every API router of one Server (Handler's and
// OAuthHandler's both come from apiRouter) shares one manager: a session
// opened through one router is found, and listed, through another.
//
// SABOTAGE: call origins.Mount per apiRouter instead of once per Server ->
// the second router answers 404 for the session -> red.
func TestRouter_OriginSessionsShareOneManagerAcrossRouters(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	first, second := s.NewAPIRouter(), s.NewAPIRouter()

	rec := httptest.NewRecorder()
	first.ServeHTTP(rec, fromLoopback(newJSONRequest(http.MethodPost, "/repos/alpha/origin-sessions",
		strings.NewReader(`{"url":"https://example.invalid/kb.git"}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.SessionID == "" {
		t.Fatalf("create body %s: %v", rec.Body.String(), err)
	}

	got := httptest.NewRecorder()
	second.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions/"+created.SessionID, nil))
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), created.SessionID) {
		t.Fatalf("the second router does not know the session the first opened: %d %s", got.Code, got.Body.String())
	}
}
