package web

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/edge/web/api/manage"
	"knomit/internal/edge/web/api/origins"
	"knomit/internal/foundation/authz"
)

// routeRule declares the one permission a route needs (unified authn/z
// design §3, plan P1 Task 1.4). Every route both routers serve has exactly
// one rule (TestRouteRules_EveryRouteDeclared); a request with no rule is
// denied.
//
// THIS TABLE IS NOT YET ENFORCED. The old gates (writeGate, requireOperator,
// requireLocalVia, requireLocalOrSameOriginAdmin) still decide; the flip (P4)
// mounts a gate that asks Policy.Has(rule permission) and deletes them.
// route_rules_equivalence_test.go pins how the two line up.
type routeRule struct {
	// Method and Pattern exactly as walkFull prints them: the API router is
	// rooted at "/" (no httpx.APIBase), the outer router's own routes are
	// "/git/*", "/docs", "/assets/*", "/*".
	Method, Pattern string
	// Perm is the permission. Zero when Resolve is set.
	Perm authz.Permission
	// Resolve picks the permission from the request when it depends on the
	// body or query (inventory G9: create-with-origin, secret_id, git push vs
	// fetch). It must return one of Possible and must leave the body readable
	// for the handler. A body resolver lives next to its handler and decodes
	// into the handler's OWN request struct with httpx.PeekJSON (DecodeJSON's
	// decoder), so the two read the same body the same way: case-insensitive
	// field names, duplicate keys, empty objects. A body it cannot decode
	// resolves to the strongest (last) of Possible: fail closed, and the
	// handler answers 400 anyway.
	//
	// The handlers' own operator/admin checks stay: P4 rewrites them as
	// Policy.Has(repo:origin|secret:manage) and must not delete them, because
	// some depend on stored state no resolver sees (a repo whose origin is
	// linked to a stored secret).
	//
	// P4 TODO: the gate must check Policy.Has(Possible[0]) BEFORE calling
	// Resolve. A caller who lacks even the weakest permission is refused
	// without the body being read, so an anonymous caller cannot make the
	// server buffer bodies.
	//
	// Possible is ordered weakest first. Under the fixed role seed each
	// stronger entry is held only by roles that also hold the weaker ones
	// (secret:manage is admin-only, and admin holds everything but
	// repo:push), so one permission per request is enough.
	Resolve  func(*http.Request) authz.Permission
	Possible []authz.Permission
}

// ruleFor finds the rule for the route chi itself would serve r with. It
// routes r through router's own tree (Routes.Match, which descends into
// mounted subrouters) on the SAME path chi routes on — r.URL.RawPath when it
// is set, else r.URL.Path (chi Mux.routeHTTP) — and reads the matched
// pattern back the way chi reports it at serve time (Context.RoutePattern).
// So a mount root called without its trailing slash (GET /repos/alpha, the
// form the clients use) finds the "/repos/{repo}/" rule, and an encoded slash
// is one segment, exactly as chi sees it.
//
// For the API router the path must be relative to httpx.APIBase. No route, or
// a method the route does not serve, is (zero, false): deny.
func ruleFor(router chi.Routes, rules []routeRule, r *http.Request) (routeRule, bool) {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	if path == "" {
		path = "/"
	}
	rctx := chi.NewRouteContext()
	if !router.Match(rctx, r.Method, path) {
		return routeRule{}, false
	}
	pattern := normalizePattern(rctx.RoutePattern())
	for _, rule := range rules {
		if rule.Method == r.Method && normalizePattern(rule.Pattern) == pattern {
			return rule, true
		}
	}
	return routeRule{}, false
}

// normalizePattern spells a pattern the way chi's Context.RoutePattern does:
// "/*/" collapsed to "/", and no trailing slash except on the root. The table
// keeps walkFull's spelling ("/repos/{repo}/"); TestRouteRules_NormalizedPatternsUnique
// guarantees no two rules of one method collapse onto each other.
func normalizePattern(p string) string {
	for strings.Contains(p, "/*/") {
		p = strings.ReplaceAll(p, "/*/", "/")
	}
	if p != "/" {
		p = strings.TrimSuffix(p, "//")
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// allMethods are the methods chi registers for HandleFunc and Mount (chi
// v5.4 includes the QUERY method, which net/http has no constant for), the
// methods walkFull lists for such a route.
var allMethods = []string{
	http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodOptions,
	http.MethodPatch, http.MethodPost, http.MethodPut, "QUERY", http.MethodTrace,
}

// anyMethod declares one permission for every method of a HandleFunc/Mount
// route.
func anyMethod(pattern string, perm authz.Permission) []routeRule {
	out := make([]routeRule, 0, len(allMethods))
	for _, m := range allMethods {
		out = append(out, routeRule{Method: m, Pattern: pattern, Perm: perm})
	}
	return out
}

func rule(method, pattern string, perm authz.Permission) routeRule {
	return routeRule{Method: method, Pattern: pattern, Perm: perm}
}

func resolved(method, pattern string, f func(*http.Request) authz.Permission, possible ...authz.Permission) routeRule {
	return routeRule{Method: method, Pattern: pattern, Resolve: f, Possible: possible}
}

// resolveGit: a push (receive-pack, either leg) needs repo:push, everything
// else under /git is a fetch and needs kb:read. isGitPush is the same test the
// write gate exempts pushes with.
func resolveGit(r *http.Request) authz.Permission {
	if isGitPush(r) {
		return authz.RepoPush
	}
	return authz.KBRead
}

// Shorthands for the table.
const (
	pRead    = authz.KBRead
	pWrite   = authz.KBWrite
	pChat    = authz.ChatUse
	pManage  = authz.RepoManage
	pOrigin  = authz.RepoOrigin
	pOps     = authz.OpsRun
	pSecret  = authz.SecretManage
	pAuthMgr = authz.AuthManage
	pFleet   = authz.FleetManage
	pPublic  = authz.Public
)

const (
	mGet, mPost, mPut, mPatch, mDel = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
)

// apiRouteRules covers every route of NewAPIRouter, rooted at "/". Assignment
// rules (design §3, §13.6, §15.3, plan Task 1.4): reads are kb:read; fact,
// lens-content and experiment writes kb:write; repo and lens lifecycle
// repo:manage; origin, credential, probe and origin-session routes and the
// pushed-branch merge repo:origin; rebuilds, jobs and cancels ops:run;
// sessions and logs ops:run; /chats chat:use; /secrets secret:manage; OAuth
// approvals auth:manage; fleet mutations fleet:manage.
//
// Some handlers ALSO decide on state the request does not carry (a repo whose
// origin is linked to a stored secret needs the admin gate to change or
// detach it: handlers_origin_hal.go linkedOriginSpec and handleHALDeleteOrigin,
// origins/handlers.go's session commit). Those stay in-handler checks and
// become Policy.Has(secret:manage) at the flip; the route's rule is its base.
var apiRouteRules = concat(
	// api root, version, spec, ontologies, templates
	[]routeRule{
		rule(mGet, "/", pRead),
		rule(mGet, "/version", pPublic),
		// /docs (public) is useless without its spec, and the spec is the
		// same for every instance: no instance data.
		rule(mGet, "/openapi.yaml", pPublic),
		rule(mGet, "/ontologies/presets", pRead),
		rule(mGet, "/ontologies/presets/{name}", pRead),
		rule(mGet, "/ontologies/schema", pRead),
		// Validates a YAML body and writes nothing; it is part of the
		// create/edit flows and is gated as a write today.
		rule(mPost, "/ontologies:validate", pWrite),
		rule(mGet, "/templates", pRead),
	},

	// sessions and logs (§15.3: ops:run; they lose read-only redaction at the flip)
	[]routeRule{
		rule(mGet, "/sessions", pOps),
		rule(mGet, "/sessions/events", pOps),
		rule(mGet, "/logs/events", pOps),
	},

	// chats (§15.3: every /chats route is chat:use; each tool call inside
	// is checked against the caller's own policy)
	[]routeRule{
		rule(mGet, "/chat-providers", pRead),
		rule(mGet, "/chats", pChat),
		rule(mPost, "/chats", pChat),
		rule(mGet, "/chats/events", pChat),
		rule(mGet, "/chats/{id}", pChat),
		rule(mPatch, "/chats/{id}", pChat),
		rule(mDel, "/chats/{id}", pChat),
		rule(mGet, "/chats/{id}/calls/{callId}", pChat),
		rule(mGet, "/chats/{id}/context", pChat),
		rule(mGet, "/chats/{id}/grants", pChat),
		rule(mDel, "/chats/{id}/grants", pChat),
		rule(mPost, "/chats/{id}/approvals/{callId}", pChat),
		rule(mPost, "/chats/{id}/cancel", pChat),
		rule(mPost, "/chats/{id}/turns", pChat),
	},

	// api/kb top level
	[]routeRule{
		// A POST only because a turn's handles are a list: a lookup.
		rule(mPost, "/facts/resolve", pRead),
	},

	// api/manage: repo lifecycle, repo-creates, archived
	[]routeRule{
		rule(mGet, "/repo-events", pRead),
		rule(mGet, "/repos", pRead),
		resolved(mPost, "/repos", manage.ResolveRepoCreate, pManage, pOrigin, pSecret),
		resolved(mPost, "/repos:probe-origin", manage.ResolveProbeOrigin, pOrigin, pSecret),
		resolved(mPost, "/repos:probe-initialized", manage.ResolveProbeInitialized, pOrigin, pSecret),
		rule(mGet, "/repos/{repo}/", pRead),
		rule(mPatch, "/repos/{repo}/", pManage),
		rule(mDel, "/repos/{repo}/", pManage),
		rule(mPost, "/repos/{repo}/rename", pManage),
		rule(mGet, "/repos/{repo}/templates", pRead),
		rule(mGet, "/repos/{repo}/pushed-branches", pRead),
		rule(mPost, "/repos/{repo}/index:cancel", pOps),
		rule(mGet, "/repo-creates", pRead),
		rule(mGet, "/repo-creates/{id}", pRead),
		rule(mDel, "/repo-creates/{id}", pManage),
		rule(mPost, "/repo-creates/{id}:cancel", pManage),
		rule(mGet, "/archived", pRead),
		rule(mGet, "/archived/{id}", pRead),
		rule(mDel, "/archived/{id}", pManage),
		resolved(mPost, "/archived/{id}/restore", manage.ResolveArchivedRestore, pManage, pOrigin, pSecret),
	},

	// api/manage: origin (the operator gate today)
	[]routeRule{
		rule(mGet, "/repos/{repo}/origin", pRead),
		rule(mPut, "/repos/{repo}/origin", pOrigin),
		rule(mDel, "/repos/{repo}/origin", pOrigin),
		rule(mPatch, "/repos/{repo}/origin/upstream", pOrigin),
		resolved(mPut, "/repos/{repo}/origin/credential", manage.ResolveOriginCredential, pOrigin, pSecret),
		rule(mPost, "/repos/{repo}/branches/{branch}/merge", pOrigin),
	},

	// api/origins: origin sessions (the operator gate today, reads included)
	[]routeRule{
		rule(mGet, "/repos/{repo}/origin-sessions/", pOrigin),
		resolved(mPost, "/repos/{repo}/origin-sessions/", origins.ResolveCreateSession, pOrigin, pSecret),
		rule(mGet, "/repos/{repo}/origin-sessions/{sessionID}", pOrigin),
		rule(mDel, "/repos/{repo}/origin-sessions/{sessionID}", pOrigin),
		rule(mGet, "/repos/{repo}/origin-sessions/{sessionID}/preview", pOrigin),
		rule(mGet, "/repos/{repo}/origin-sessions/{sessionID}/test", pOrigin),
		rule(mPost, "/repos/{repo}/origin-sessions/{sessionID}/apply", pOrigin),
		rule(mGet, "/repos/{repo}/origin-sessions/{sessionID}/apply/events", pOrigin),
		rule(mPost, "/repos/{repo}/origin-sessions/{sessionID}/commit", pOrigin),
		rule(mGet, "/repos/{repo}/origin-sessions/{sessionID}/commit/events", pOrigin),
	},

	// api/kb: experiments
	[]routeRule{
		rule(mGet, "/repos/{repo}/experiments", pRead),
		rule(mPost, "/repos/{repo}/experiments", pWrite),
		rule(mDel, "/repos/{repo}/experiments/{name}/", pWrite),
		rule(mPost, "/repos/{repo}/experiments/{name}/commit", pWrite),
		rule(mPost, "/repos/{repo}/experiments/{name}/rollback", pWrite),
		rule(mPost, "/repos/{repo}/experiments/{name}/sync", pWrite),
	},

	// api/kb: one branch
	[]routeRule{
		rule(mGet, "/repos/{repo}/branches", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/activity", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/changes", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/commits", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/commits/{sha}", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/commits/{sha}/facts/*", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/commits/{sha}/topics", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/commits/{sha}/topics/*", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/completions", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/domains", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/domains/{name}", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/events", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/facts", pRead),
		rule(mPost, "/repos/{repo}/branches/{branch}/facts", pWrite),
		rule(mGet, "/repos/{repo}/branches/{branch}/facts/*", pRead),
		rule(mPut, "/repos/{repo}/branches/{branch}/facts/*", pWrite),
		rule(mDel, "/repos/{repo}/branches/{branch}/facts/*", pWrite),
		rule(mGet, "/repos/{repo}/branches/{branch}/motifs", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/motifs/{key}", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/search", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/search/factset", pRead),
		// Closing the caller's own factset frees a read session: a read.
		rule(mDel, "/repos/{repo}/branches/{branch}/search/factset", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/search/factset/export", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/stats", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/topics", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/topics/*", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/triggers", pRead),
		rule(mGet, "/repos/{repo}/branches/{branch}/index-rebuilds", pRead),
		rule(mPost, "/repos/{repo}/branches/{branch}/index-rebuilds", pOps),
		rule(mGet, "/repos/{repo}/branches/{branch}/index-rebuilds/{id}", pRead),
		rule(mDel, "/repos/{repo}/branches/{branch}/index-rebuilds/{id}", pOps),
		rule(mGet, "/repos/{repo}/branches/{branch}/index-rebuilds/{id}/events", pRead),
	},

	// api/kb + api/manage: lenses
	[]routeRule{
		rule(mGet, "/lenses", pRead),
		rule(mPost, "/lenses", pManage),
		rule(mGet, "/lenses/{lens}/", pRead),
		rule(mPatch, "/lenses/{lens}/", pManage),
		rule(mDel, "/lenses/{lens}/", pManage),
		rule(mPost, "/lenses/{lens}/rename", pManage),
		rule(mGet, "/lenses/{lens}/completions", pRead),
		rule(mGet, "/lenses/{lens}/events", pRead),
		rule(mGet, "/lenses/{lens}/facts", pRead),
		rule(mGet, "/lenses/{lens}/facts/*", pRead),
		rule(mGet, "/lenses/{lens}/motifs", pRead),
		rule(mGet, "/lenses/{lens}/motifs/{key}", pRead),
		rule(mGet, "/lenses/{lens}/search", pRead),
		rule(mGet, "/lenses/{lens}/search/factset", pRead),
		rule(mDel, "/lenses/{lens}/search/factset", pRead),
		rule(mGet, "/lenses/{lens}/search/factset/export", pRead),
		rule(mGet, "/lenses/{lens}/stats", pRead),
		rule(mGet, "/lenses/{lens}/topics", pRead),
		rule(mGet, "/lenses/{lens}/topics/*", pRead),
	},

	// MCP mounts: kb:read at the HTTP level; the per-(tool, action) gate
	// (mcp.PermissionFor) decides each call.
	anyMethod("/mcp", pRead),
	anyMethod("/mcp/*", pRead),
	anyMethod("/repos/{repo}/branches/{branch}/mcp", pRead),
	anyMethod("/repos/{repo}/branches/{branch}/mcp/*", pRead),
	anyMethod("/lenses/{lens}/mcp", pRead),
	anyMethod("/lenses/{lens}/mcp/*", pRead),
	anyMethod("/lenses/{lens}/experiments/{name}/mcp", pRead),
	anyMethod("/lenses/{lens}/experiments/{name}/mcp/*", pRead),

	// fleet. The three reads are open to any reader; everything else uses
	// the fleet root key or installs trust material. GET /fleet/root RETURNS
	// the root key files, so it is fleet:manage, not a read.
	[]routeRule{
		rule(mGet, "/fleet", pRead),
		rule(mGet, "/fleet/members", pRead),
		rule(mGet, "/fleet/activity", pRead),
		rule(mGet, "/fleet/root", pFleet),
		rule(mPut, "/fleet/root", pFleet),
		rule(mDel, "/fleet/root", pFleet),
		resolved(mPost, "/fleet/create", manage.ResolveFleetCreate, pFleet, pSecret),
		rule(mPost, "/fleet/enroll", pFleet),
		resolved(mPost, "/fleet/join", manage.ResolveFleetJoin, pFleet, pSecret),
		resolved(mPost, "/fleet/join:check", manage.ResolveFleetJoin, pFleet, pSecret),
		rule(mPost, "/fleet/leave", pFleet),
		rule(mPost, "/fleet/revoke", pFleet),
		rule(mPost, "/fleet/decommission", pFleet),
		rule(mPost, "/fleet/host", pFleet),
		resolved(mPost, "/fleet/follow", manage.ResolveFleetFollow, pFleet, pSecret),
		rule(mPost, "/fleet/index-rebuild", pFleet),
		rule(mPost, "/fleet/drop-commit", pFleet),
	},

	// api/secrets
	[]routeRule{
		rule(mGet, "/secrets/", pSecret),
		rule(mPost, "/secrets/", pSecret),
		rule(mGet, "/secrets/{id}", pSecret),
		rule(mPatch, "/secrets/{id}", pSecret),
		rule(mDel, "/secrets/{id}", pSecret),
	},

	// OAuth approvals and approved logins (mounted only with an issuer)
	[]routeRule{
		rule(mGet, "/oauth/pending/", pAuthMgr),
		rule(mPost, "/oauth/pending/{id}/approve", pAuthMgr),
		rule(mPost, "/oauth/pending/{id}/deny", pAuthMgr),
		rule(mGet, "/oauth/grants/", pAuthMgr),
		rule(mPost, "/oauth/grants/{id}/revoke", pAuthMgr),
	},
)

// outerRouteRules covers the outer router's own routes (Server.Handler); the
// API mount is covered by apiRouteRules.
var outerRouteRules = concat(
	// git over HTTP: fetch is kb:read, push (receive-pack) repo:push, which
	// only the node role holds (§16).
	resolvedAnyMethod("/git/*", resolveGit, pRead, authz.RepoPush),
	[]routeRule{
		rule(mGet, "/docs", pPublic),
		rule(http.MethodHead, "/docs", pPublic),
		rule(mGet, "/assets/*", pPublic),
		rule(http.MethodHead, "/assets/*", pPublic),
		// The SPA shell: the page itself carries no data.
		rule(mGet, "/*", pPublic),
		rule(http.MethodHead, "/*", pPublic),
	},
)

func resolvedAnyMethod(pattern string, f func(*http.Request) authz.Permission, possible ...authz.Permission) []routeRule {
	out := make([]routeRule, 0, len(allMethods))
	for _, m := range allMethods {
		out = append(out, resolved(m, pattern, f, possible...))
	}
	return out
}

func concat(groups ...[]routeRule) []routeRule {
	var out []routeRule
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
