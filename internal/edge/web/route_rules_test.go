package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"knomit/internal/edge/web/httpx"
	"knomit/internal/foundation/authz"
)

// registryServer is a server with every optional route family mounted: the
// OAuth approval endpoints (only with an issuer), /git (only with a git
// handler) and the static UI (not APIOnly). The registry must cover all of
// them, so a test server that omitted one would let its routes go undeclared.
func registryServer(t *testing.T) *Server {
	t.Helper()
	s := newOAuthWebFixture(t, "http://localhost:19278").s
	s.GitHandler = http.NotFoundHandler()
	return s
}

// walkedRoutes returns the API router's routes ("METHOD pattern", rooted at
// the API router as the golden prints them) and the outer router's own
// routes (everything not under httpx.APIBase).
func walkedRoutes(t *testing.T) (api, outer []string) {
	t.Helper()
	s := registryServer(t)
	var a []routeChain
	walkFull(s.NewAPIRouter(), "", nil, &a)
	for _, rc := range a {
		api = append(api, rc.route)
	}
	h, ok := s.Handler().(chi.Routes)
	if !ok {
		t.Fatalf("Server.Handler is %T, not a chi router", s.Handler())
	}
	var o []routeChain
	walkFull(h, "", nil, &o)
	for _, rc := range o {
		_, pattern, _ := strings.Cut(rc.route, " ")
		if pattern == httpx.APIBase || strings.HasPrefix(pattern, httpx.APIBase+"/") {
			continue
		}
		outer = append(outer, rc.route)
	}
	sort.Strings(api)
	sort.Strings(outer)
	if len(api) < 200 || len(outer) < 10 {
		t.Fatalf("walked %d API and %d outer routes: the fixture lost a route family", len(api), len(outer))
	}
	return api, outer
}

func ruleKey(r routeRule) string { return r.Method + " " + r.Pattern }

// TestRouteRules_EveryRouteDeclared: every route either router serves has
// EXACTLY one rule. Undeclared is denied at the flip, so a new route without a
// row would be a route nobody can call; this names it now.
func TestRouteRules_EveryRouteDeclared(t *testing.T) {
	api, outer := walkedRoutes(t)
	for _, tc := range []struct {
		name   string
		routes []string
		rules  []routeRule
	}{
		{"apiRouteRules", api, apiRouteRules},
		{"outerRouteRules", outer, outerRouteRules},
	} {
		count := map[string]int{}
		for _, r := range tc.rules {
			count[ruleKey(r)]++
		}
		var undeclared, dup []string
		for _, rt := range tc.routes {
			switch count[rt] {
			case 0:
				undeclared = append(undeclared, rt)
			case 1:
			default:
				dup = append(dup, rt)
			}
		}
		if len(undeclared) > 0 {
			t.Errorf("%s: %d routes have no permission rule:\n  %s", tc.name, len(undeclared), strings.Join(undeclared, "\n  "))
		}
		if len(dup) > 0 {
			t.Errorf("%s: routes with more than one rule:\n  %s", tc.name, strings.Join(dup, "\n  "))
		}
	}
}

// TestRouteRules_NoDeadRows: a rule that matches no route is either a typo
// (and the real route is undeclared) or a route that was removed.
func TestRouteRules_NoDeadRows(t *testing.T) {
	api, outer := walkedRoutes(t)
	for _, tc := range []struct {
		name   string
		routes []string
		rules  []routeRule
	}{
		{"apiRouteRules", api, apiRouteRules},
		{"outerRouteRules", outer, outerRouteRules},
	} {
		for _, r := range tc.rules {
			if !slices.Contains(tc.routes, ruleKey(r)) {
				t.Errorf("%s: dead row %q matches no route", tc.name, ruleKey(r))
			}
		}
	}
}

// Every rule is well formed: a fixed permission XOR a resolver with its
// enumeration, and only permissions the registry knows.
func TestRouteRules_WellFormed(t *testing.T) {
	known := append(authz.All(), authz.Public)
	for _, r := range append(slices.Clone(apiRouteRules), outerRouteRules...) {
		switch {
		case r.Resolve == nil && r.Perm == "":
			t.Errorf("%s: neither Perm nor Resolve", ruleKey(r))
		case r.Resolve != nil && r.Perm != "":
			t.Errorf("%s: both Perm and Resolve", ruleKey(r))
		case r.Resolve != nil && len(r.Possible) < 2:
			t.Errorf("%s: a resolver must enumerate at least two Possible permissions", ruleKey(r))
		case r.Resolve == nil && len(r.Possible) > 0:
			t.Errorf("%s: Possible without a resolver", ruleKey(r))
		}
		for _, p := range append([]authz.Permission{r.Perm}, r.Possible...) {
			if p != "" && !slices.Contains(known, p) {
				t.Errorf("%s: unknown permission %q", ruleKey(r), p)
			}
		}
	}
}

// resolverCase is one request fixture for a body- or query-dependent rule.
type resolverCase struct {
	path, body string
	want       authz.Permission
}

// resolverFixtures has a table for every Resolve rule (checked below), so a
// new resolver without fixtures fails.
var resolverFixtures = map[string][]resolverCase{
	"POST /repos": {
		{"/repos", `{"name":"a"}`, authz.RepoManage},
		{"/repos", `{"name":"a","origin":null}`, authz.RepoManage},
		{"/repos", `{"name":"a","origin":{"url":"https://x/y.git"}}`, authz.RepoOrigin},
		{"/repos", `{"name":"a","origin":{"url":"https://x/y.git","auth_token":"t"}}`, authz.RepoOrigin},
		{"/repos", `{"name":"a","origin":{"url":"https://x/y.git","secret_id":"s1"}}`, authz.SecretManage},
		{"/repos", `{not json`, authz.SecretManage},
		// The handler decodes with encoding/json into its own struct: field
		// names match case-insensitively, a duplicate key decodes into the
		// SAME origin pointer, and an empty origin object is still an origin.
		{"/repos", `{"name":"a","origin":{"url":"https://x","Secret_ID":"s1"}}`, authz.SecretManage},
		{"/repos", `{"name":"a","Origin":{"url":"https://x"}}`, authz.RepoOrigin},
		{"/repos", `{"origin":{"url":"https://x"},"origin":{"secret_id":"s1"}}`, authz.SecretManage},
		{"/repos", `{"origin":{"secret_id":"s1"},"origin":{"url":"https://x"}}`, authz.SecretManage},
		{"/repos", `{"origin":{}}`, authz.RepoOrigin},
		{"/repos", `{"name":"a"} {"origin":{"secret_id":"s1"}}`, authz.SecretManage}, // trailing data: 400, fail closed
	},
	"POST /archived/{id}/restore": {
		{"/archived/u1/restore", ``, authz.RepoManage},
		{"/archived/u1/restore", `{"new_name":"b"}`, authz.RepoManage},
		{"/archived/u1/restore", `{"no_credential":true}`, authz.RepoManage},
		{"/archived/u1/restore", `{"auth_method":"token","token":"t"}`, authz.RepoOrigin},
		{"/archived/u1/restore", `{"auth_method":"basic","user":"u","password":"p"}`, authz.RepoOrigin},
		{"/archived/u1/restore", `{"auth_method":"token"}`, authz.RepoOrigin},
		// No method: AssembleAuthToken ignores user/password, the handler
		// restores with no pasted credential and runs no operator gate.
		{"/archived/u1/restore", `{"user":"u","password":"p"}`, authz.RepoManage},
		{"/archived/u1/restore", `{"Token":"t"}`, authz.RepoOrigin},
		{"/archived/u1/restore", `{"SECRET_ID":"s1"}`, authz.SecretManage},
		{"/archived/u1/restore", `{"secret_id":"s1","secret_id":""}`, authz.RepoManage}, // the last key wins, as in the handler
		{"/archived/u1/restore", `{"secret_id":"s1"}`, authz.SecretManage},
		{"/archived/u1/restore", `[`, authz.SecretManage},
	},
	"POST /repos:probe-origin": {
		{"/repos:probe-origin", `{"url":"https://x"}`, authz.RepoOrigin},
		{"/repos:probe-origin", `{"url":"https://x","secret_id":""}`, authz.RepoOrigin},
		{"/repos:probe-origin", `{"url":"https://x","secret_id":"s"}`, authz.SecretManage},
		{"/repos:probe-origin", `{"url":"https://x","Secret_ID":"s"}`, authz.SecretManage},
	},
	"POST /repos:probe-initialized": {
		{"/repos:probe-initialized", `{"url":"https://x"}`, authz.RepoOrigin},
		{"/repos:probe-initialized", `{"url":"https://x","secret_id":"s"}`, authz.SecretManage},
		{"/repos:probe-initialized", `{"url":"https://x","SECRET_ID":"s"}`, authz.SecretManage},
	},
	"PUT /repos/{repo}/origin/credential": {
		{"/repos/alpha/origin/credential", `{"auth_method":"token","token":"t"}`, authz.RepoOrigin},
		{"/repos/alpha/origin/credential", `{"secret_id":"s"}`, authz.SecretManage},
		// null unlinks a stored secret, which needs the admin gate today.
		{"/repos/alpha/origin/credential", `{"secret_id":null}`, authz.SecretManage},
		{"/repos/alpha/origin/credential", `{"SECRET_ID":"s"}`, authz.SecretManage},
		{"/repos/alpha/origin/credential", `{"Secret_Id":null}`, authz.SecretManage},
	},
	"POST /repos/{repo}/origin-sessions/": {
		{"/repos/alpha/origin-sessions/", `{"url":"https://x"}`, authz.RepoOrigin},
		{"/repos/alpha/origin-sessions/", `{"url":"https://x","secret_id":"s"}`, authz.SecretManage},
		{"/repos/alpha/origin-sessions/", `{"url":"https://x","Secret_id":"s"}`, authz.SecretManage},
	},
	"POST /fleet/create": {
		{"/fleet/create", `{"name":"f"}`, authz.FleetManage},
		{"/fleet/create", `{"origin":{"url":"https://x","auth_token":"t"}}`, authz.FleetManage},
		{"/fleet/create", `{"origin":{"url":"https://x","secret_id":"s"}}`, authz.SecretManage},
		{"/fleet/create", `{"origin":{"url":"https://x","Secret_ID":"s"}}`, authz.SecretManage},
		{"/fleet/create", `{"origin":{"url":"https://x"},"origin":{"secret_id":"s"}}`, authz.SecretManage},
	},
	"POST /fleet/join": {
		{"/fleet/join", `{"bundle":"b"}`, authz.FleetManage},
		{"/fleet/join", `{"bundle":"b","secret_id":"s"}`, authz.SecretManage},
		{"/fleet/join", `{"bundle":"b","SECRET_ID":"s"}`, authz.SecretManage},
	},
	"POST /fleet/join:check": {
		{"/fleet/join:check", `{"bundle":"b"}`, authz.FleetManage},
		{"/fleet/join:check", `{"bundle":"b","secret_id":"s"}`, authz.SecretManage},
	},
	"POST /fleet/follow": {
		{"/fleet/follow", `{"url":"https://x"}`, authz.FleetManage},
		{"/fleet/follow", `{"url":"https://x","secret_id":"s"}`, authz.SecretManage},
		{"/fleet/follow", `{"url":"https://x","Secret_Id":"s"}`, authz.SecretManage},
	},
}

// outerResolverFixtures are the git routes' cases, keyed by method; the path
// carries the query.
var outerResolverFixtures = map[string][]resolverCase{
	"/git/*": {
		{"/git/alpha.git/info/refs?service=git-upload-pack", "", authz.KBRead},
		{"/git/alpha/git-upload-pack", "", authz.KBRead},
		{"/git/alpha.git/HEAD", "", authz.KBRead},
		{"/git/alpha.git/info/refs?service=git-receive-pack", "", authz.RepoPush},
		{"/git/alpha/git-receive-pack", "", authz.RepoPush},
	},
}

func TestRouteRules_ResolversStayInPossible(t *testing.T) {
	check := func(r routeRule, c resolverCase, method string) {
		t.Helper()
		var body io.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		req := httptest.NewRequest(method, c.path, body)
		got := r.Resolve(req)
		if !slices.Contains(r.Possible, got) {
			t.Errorf("%s %s %s: resolved %q, not in Possible %v", method, c.path, c.body, got, r.Possible)
		}
		if got != c.want {
			t.Errorf("%s %s %s: resolved %q, want %q", method, c.path, c.body, got, c.want)
		}
		// The handler must still see the whole body.
		if c.body != "" {
			rest, _ := io.ReadAll(req.Body)
			if string(rest) != c.body {
				t.Errorf("%s %s: the resolver consumed the body: handler would read %q", method, c.path, rest)
			}
		}
	}
	for _, r := range apiRouteRules {
		if r.Resolve == nil {
			continue
		}
		cases, ok := resolverFixtures[ruleKey(r)]
		if !ok {
			t.Errorf("%s: a resolver with no fixtures in resolverFixtures", ruleKey(r))
			continue
		}
		for _, c := range cases {
			check(r, c, r.Method)
		}
	}
	for _, r := range outerRouteRules {
		if r.Resolve == nil {
			continue
		}
		cases, ok := outerResolverFixtures[r.Pattern]
		if !ok {
			t.Errorf("%s: a resolver with no fixtures", ruleKey(r))
			continue
		}
		for _, c := range cases {
			check(r, c, r.Method)
		}
	}
}

// ruleFor finds a route's rule through chi's own matcher, through mounted
// subrouters: a request to a concrete path gets the rule of the PATTERN.
func TestRuleFor_FindsMountedSubrouterPatterns(t *testing.T) {
	s := registryServer(t)
	api := s.NewAPIRouter()
	for _, c := range []struct {
		method, path, wantPattern string
		want                      authz.Permission
	}{
		{"POST", "/repos/alpha/branches/x/mcp", "/repos/{repo}/branches/{branch}/mcp", authz.KBRead},
		{"CONNECT", "/repos/alpha/branches/x/mcp", "/repos/{repo}/branches/{branch}/mcp", authz.KBRead},
		{"GET", "/repos/alpha/branches/x/facts/kb/a/b.md", "/repos/{repo}/branches/{branch}/facts/*", authz.KBRead},
		{"PUT", "/repos/alpha/branches/x/facts/kb/a/b.md", "/repos/{repo}/branches/{branch}/facts/*", authz.KBWrite},
		{"GET", "/repos/alpha/origin-sessions/s1/preview", "/repos/{repo}/origin-sessions/{sessionID}/preview", authz.RepoOrigin},
		{"GET", "/secrets/", "/secrets/", authz.SecretManage},
		{"POST", "/oauth/pending/p1/approve", "/oauth/pending/{id}/approve", authz.AuthManage},
		{"GET", "/sessions", "/sessions", authz.OpsRun},
		{"POST", "/chats/c1/turns", "/chats/{id}/turns", authz.ChatUse},
		{"GET", "/version", "/version", authz.Public},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		r, ok := ruleFor(api, apiRouteRules, req)
		if !ok {
			t.Errorf("%s %s: no rule", c.method, c.path)
			continue
		}
		if r.Pattern != c.wantPattern || r.Perm != c.want {
			t.Errorf("%s %s: rule %s -> %q, want %s -> %q", c.method, c.path, ruleKey(r), r.Perm, c.wantPattern, c.want)
		}
	}
	// No route, and a method the route does not serve: no rule (denied).
	for _, c := range [][2]string{{"GET", "/no/such/route"}, {"HEAD", "/sessions"}, {"DELETE", "/version"}} {
		if r, ok := ruleFor(api, apiRouteRules, httptest.NewRequest(c[0], c[1], nil)); ok {
			t.Errorf("%s %s: found rule %s for an unrouted request", c[0], c[1], ruleKey(r))
		}
	}

	outer, ok := s.Handler().(*chi.Mux)
	if !ok {
		t.Fatal("Handler is not a *chi.Mux")
	}
	for _, c := range []struct {
		method, path, wantPattern string
	}{
		{"POST", "/git/alpha/git-upload-pack", "/git/*"},
		{"GET", "/docs", "/docs"},
		{"HEAD", "/assets/index.js", "/assets/*"},
		{"GET", "/repos/alpha", "/*"},
	} {
		r, ok := ruleFor(outer, outerRouteRules, httptest.NewRequest(c.method, c.path, nil))
		if !ok || r.Pattern != c.wantPattern {
			t.Errorf("outer %s %s: rule %s (%v), want pattern %s", c.method, c.path, ruleKey(r), ok, c.wantPattern)
		}
	}
}

// concretePath turns a walked pattern into a path chi routes to it: every
// {param} becomes a plain segment, a trailing * a two-segment tail.
func concretePath(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			j := strings.IndexByte(pattern[i:], '}')
			b.WriteString("v1")
			i += j
		case '*':
			b.WriteString("a/b.md")
		default:
			b.WriteByte(pattern[i])
		}
	}
	return b.String()
}

// TestRuleFor_EveryRouteByConcretePath: a request to a concrete path of EVERY
// walked route finds that route's own rule, and so does the no-trailing-slash
// spelling of a subrouter root (GET /repos/alpha, /secrets, /oauth/pending),
// which is the spelling web/src/api.ts and the bridge actually call: chi
// routes it to the same handler, so the rule must be the same one.
func TestRuleFor_EveryRouteByConcretePath(t *testing.T) {
	s := registryServer(t)
	api := s.NewAPIRouter()
	var chains []routeChain
	walkFull(api, "", nil, &chains)
	outer := s.Handler().(*chi.Mux)
	var outerChains []routeChain
	walkFull(outer, "", nil, &outerChains)

	check := func(router chi.Routes, rules []routeRule, route string) {
		t.Helper()
		method, pattern, _ := strings.Cut(route, " ")
		paths := []string{concretePath(pattern)}
		if strings.HasSuffix(pattern, "/") && pattern != "/" {
			paths = append(paths, strings.TrimSuffix(concretePath(pattern), "/"))
		}
		for _, p := range paths {
			r, ok := ruleFor(router, rules, httptest.NewRequest(method, p, nil))
			if !ok {
				t.Errorf("%s %s (route %s): no rule", method, p, route)
				continue
			}
			if ruleKey(r) != route {
				t.Errorf("%s %s: got rule %s, want %s", method, p, ruleKey(r), route)
			}
		}
	}
	for _, rc := range chains {
		check(api, apiRouteRules, rc.route)
	}
	for _, rc := range outerChains {
		_, pattern, _ := strings.Cut(rc.route, " ")
		if strings.HasPrefix(pattern, httpx.APIBase) {
			continue
		}
		check(outer, outerRouteRules, rc.route)
	}
}

// ruleFor compares patterns in chi's RoutePattern spelling; no two rules of
// one method may collapse onto the same one, or a request would take
// whichever comes first.
func TestRouteRules_NormalizedPatternsUnique(t *testing.T) {
	for name, rules := range map[string][]routeRule{"api": apiRouteRules, "outer": outerRouteRules} {
		seen := map[string]string{}
		for _, r := range rules {
			k := r.Method + " " + normalizePattern(r.Pattern)
			if prev, dup := seen[k]; dup {
				t.Errorf("%s: %s and %s are the same route to chi", name, prev, ruleKey(r))
			}
			seen[k] = ruleKey(r)
		}
	}
}

// webClientPath matches an API path literal in web/src/api.ts: a quoted or
// template string starting with /api/v1.
var webClientPath = regexp.MustCompile("['`](/api/v1[^'`?]*)")

// TestRuleFor_WebClientPaths: every API path the web client spells out finds
// a rule for at least one method. The method lives elsewhere in each call,
// so this checks the PATH spelling (trailing slash or not, parameters), which
// is what a Find-style lookup got wrong. A path whose parameter is glued to a
// segment (`/fleet${path}`) is skipped: its concrete form is not in the file.
func TestRuleFor_WebClientPaths(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "src", "api.ts"))
	if err != nil {
		t.Skipf("web client source not available: %v", err)
	}
	api := registryServer(t).NewAPIRouter()
	param := regexp.MustCompile(`\$\{[^}]*\}`)
	seen := 0
	for _, m := range webClientPath.FindAllStringSubmatch(string(src), -1) {
		p := m[1]
		if loc := param.FindStringIndex(p); loc != nil && p[loc[0]-1] != '/' {
			continue
		}
		p = strings.TrimPrefix(param.ReplaceAllString(p, "v1"), httpx.APIBase)
		if p == "" {
			p = "/"
		}
		seen++
		found := false
		for _, method := range allMethods {
			if _, ok := ruleFor(api, apiRouteRules, httptest.NewRequest(method, p, nil)); ok {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("web client path %q (%s) finds no rule for any method", m[1], p)
		}
	}
	if seen < 30 {
		t.Fatalf("extracted only %d paths from api.ts: the pattern no longer matches the client", seen)
	}
}

// The named mount roots, without their trailing slash, as the clients call
// them.
func TestRuleFor_MountRootsWithoutSlash(t *testing.T) {
	api := registryServer(t).NewAPIRouter()
	for _, c := range []struct {
		method, path, want string
	}{
		{"GET", "/repos/alpha", "GET /repos/{repo}/"},
		{"PATCH", "/repos/alpha", "PATCH /repos/{repo}/"},
		{"DELETE", "/repos/alpha", "DELETE /repos/{repo}/"},
		{"GET", "/lenses/x", "GET /lenses/{lens}/"},
		{"DELETE", "/lenses/x", "DELETE /lenses/{lens}/"},
		{"GET", "/secrets", "GET /secrets/"},
		{"POST", "/secrets", "POST /secrets/"},
		{"GET", "/oauth/pending", "GET /oauth/pending/"},
		{"GET", "/oauth/grants", "GET /oauth/grants/"},
		{"GET", "/repos/a/origin-sessions", "GET /repos/{repo}/origin-sessions/"},
		{"POST", "/repos/a/origin-sessions", "POST /repos/{repo}/origin-sessions/"},
		{"DELETE", "/repos/a/experiments/e", "DELETE /repos/{repo}/experiments/{name}/"},
		{"GET", "/repos/a/branches/b", "GET /repos/{repo}/branches/{branch}/"},
	} {
		r, ok := ruleFor(api, apiRouteRules, httptest.NewRequest(c.method, c.path, nil))
		if !ok || ruleKey(r) != c.want {
			t.Errorf("%s %s: rule %q (%v), want %q", c.method, c.path, ruleKey(r), ok, c.want)
		}
	}
}

// chi routes on r.URL.RawPath when it is set, so an encoded slash is one
// segment to chi. The rule must be the one for the route chi actually picks:
// this DELETE archives repo "a/branches/b/search/factset" (repo:manage), it
// does not close a factset (kb:read).
func TestRuleFor_FollowsRawPathLikeChi(t *testing.T) {
	api := registryServer(t).NewAPIRouter()
	req := httptest.NewRequest("DELETE", "/repos/a%2Fbranches%2Fb%2Fsearch%2Ffactset", nil)
	if req.URL.RawPath == "" {
		t.Fatal("fixture: RawPath not set, the test proves nothing")
	}
	r, ok := ruleFor(api, apiRouteRules, req)
	if !ok || ruleKey(r) != "DELETE /repos/{repo}/" || r.Perm != authz.RepoManage {
		t.Fatalf("rule %q -> %q (%v), want DELETE /repos/{repo}/ -> repo:manage", ruleKey(r), r.Perm, ok)
	}
}

// TestRouteRules_Rulings pins the assignments the design names explicitly, so
// a table edit that breaks one names the ruling.
func TestRouteRules_Rulings(t *testing.T) {
	byKey := map[string]routeRule{}
	for _, r := range apiRouteRules {
		byKey[ruleKey(r)] = r
	}
	for _, r := range apiRouteRules {
		_, pattern, _ := strings.Cut(ruleKey(r), " ")
		switch {
		case strings.HasPrefix(pattern, "/chats"):
			if r.Perm != authz.ChatUse {
				t.Errorf("§15.3: %s must need chat:use, has %q", ruleKey(r), r.Perm)
			}
		case strings.HasPrefix(pattern, "/secrets"):
			if r.Perm != authz.SecretManage {
				t.Errorf("%s must need secret:manage, has %q", ruleKey(r), r.Perm)
			}
		case strings.HasPrefix(pattern, "/oauth/"):
			if r.Perm != authz.AuthManage {
				t.Errorf("%s must need auth:manage, has %q", ruleKey(r), r.Perm)
			}
		case strings.HasSuffix(pattern, "/mcp") || strings.HasSuffix(pattern, "/mcp/*"):
			if r.Perm != authz.KBRead {
				t.Errorf("%s: MCP mounts are kb:read at the HTTP level (the tool gate does the rest), has %q", ruleKey(r), r.Perm)
			}
		case strings.HasPrefix(pattern, "/sessions") || pattern == "/logs/events":
			if r.Perm != authz.OpsRun {
				t.Errorf("§15.3: %s must need ops:run, has %q", ruleKey(r), r.Perm)
			}
		}
	}
	for key, want := range map[string]authz.Permission{
		"GET /version":        authz.Public,
		"POST /facts/resolve": authz.KBRead,
		"DELETE /repos/{repo}/branches/{branch}/search/factset": authz.KBRead,
		"DELETE /lenses/{lens}/search/factset":                  authz.KBRead,
		"POST /repos/{repo}/branches/{branch}/merge":            authz.RepoOrigin,
		"GET /fleet":      authz.KBRead,
		"GET /fleet/root": authz.FleetManage,
	} {
		if got := byKey[key].Perm; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
