package web

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"knomit/internal/edge/web/httpx"
	"knomit/internal/foundation/authz"
)

// oldGate names the gate a route has TODAY, read off its live middleware
// chain (the chain route_table.golden pins) and its method:
//
//	admin-local  requireLocalOrSameOriginAdmin (secrets, OAuth approvals)
//	local        httpx.RequireLocalVia (fleet operations)
//	operator     Require(operator) (origin, probes, origin sessions, merge)
//	write        writeGate on a request isMutatingRequest does not exempt
//	none         nothing beyond the edge
func oldGate(rc routeChain) string {
	method, pattern, _ := strings.Cut(rc.route, " ")
	fp := make([]string, len(rc.mws))
	for i, m := range rc.mws {
		fp[i] = mwFingerprint(m)
	}
	switch {
	case slices.Contains(fp, "(*Server).requireLocalOrSameOriginAdmin-fm"):
		return "admin-local"
	case slices.Contains(fp, "requireLocalVia"):
		return "local"
	case slices.Contains(fp, "require"):
		return "operator"
	case slices.Contains(fp, "writeGate") && isMutatingRequest(method, httpx.APIBase+pattern):
		return "write"
	}
	return "none"
}

// allowed lists, per old gate, the new permissions that are a faithful
// translation of it. ChatUse and OpsRun under "none" are NOT faithful (they
// tighten an ungated route) and each such row must be in intendedDeviations.
var allowed = map[string][]authz.Permission{
	"none":        {authz.KBRead, authz.Public, authz.ChatUse, authz.OpsRun},
	"write":       {authz.KBWrite, authz.RepoManage, authz.OpsRun},
	"operator":    {authz.RepoOrigin},
	"local":       {authz.FleetManage, authz.AuthManage}, // AuthManage: P3's socket-only /auth/* routes
	"admin-local": {authz.SecretManage, authz.AuthManage},
}

// handlerEscalations are the extra permissions a resolver may add on top of
// its route's base: they replace the operator and admin gates the handlers
// run on the body today (httpx.Admits(operator|admin) in handlers_repos_create,
// handlers_archived_hal, handlers_origin_credential, secret_field, origins).
var handlerEscalations = []authz.Permission{authz.RepoOrigin, authz.SecretManage}

// intendedDeviations: "METHOD pattern" -> the design section that changes
// the route's gate on purpose. Nothing else may differ from today.
var intendedDeviations = map[string]string{
	"GET /chats":                          "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"POST /chats":                         "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"GET /chats/events":                   "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"GET /chats/{id}":                     "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"PATCH /chats/{id}":                   "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"DELETE /chats/{id}":                  "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"GET /chats/{id}/calls/{callId}":      "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"GET /chats/{id}/context":             "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"GET /chats/{id}/grants":              "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"DELETE /chats/{id}/grants":           "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"POST /chats/{id}/approvals/{callId}": "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"POST /chats/{id}/cancel":             "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"POST /chats/{id}/turns":              "§15.3 chat routes need chat:use (exempt from writeGate today)",
	"GET /sessions":                       "§15.3 viewing sessions needs ops:run (ungated today, redacted under read_only)",
	"GET /sessions/events":                "§15.3 viewing sessions needs ops:run (ungated today, redacted under read_only)",
	"GET /logs/events":                    "§15.3 viewing logs needs ops:run (ungated today, redacted under read_only)",
}

func init() {
	// The lens-experiment MCP mount is the one MCP mount mcpRoutePattern
	// (readonly.go) does not match, so today writeGate demands write for its
	// POST/PUT/PATCH/DELETE, reads included. The registry treats it like
	// every other MCP mount; the old gate is left as it is (P1 enforces
	// nothing).
	for _, p := range []string{"/lenses/{lens}/experiments/{name}/mcp", "/lenses/{lens}/experiments/{name}/mcp/*"} {
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			intendedDeviations[m+" "+p] = "§3 every MCP mount is kb:read at the HTTP level and the per-tool gate decides " +
				"(today mcpRoutePattern misses this mount, so writeGate demands write even for reads)"
		}
	}
}

var designSection = regexp.MustCompile(`§\d+(\.\d+)?`)

// TestRouteRules_EquivalentToOldGates pins how the new permissions relate to
// today's gates, route by route. A mismatch that is not an intended,
// design-cited deviation is a table bug: fix the table, not this test.
func TestRouteRules_EquivalentToOldGates(t *testing.T) {
	s := registryServer(t)
	var chains []routeChain
	walkFull(s.NewAPIRouter(), "", nil, &chains)
	rules := map[string]routeRule{}
	for _, r := range apiRouteRules {
		rules[ruleKey(r)] = r
	}
	used := map[string]bool{}
	for _, rc := range chains {
		r, ok := rules[rc.route]
		if !ok {
			continue // TestRouteRules_EveryRouteDeclared reports it
		}
		old := oldGate(rc)
		base, extra := r.Perm, []authz.Permission(nil)
		if r.Resolve != nil {
			base, extra = r.Possible[0], r.Possible[1:]
		}
		for _, e := range extra {
			if !slices.Contains(handlerEscalations, e) {
				t.Errorf("%s: resolver adds %q, which no handler gate on the body corresponds to", rc.route, e)
			}
		}
		why, deviates := intendedDeviations[rc.route]
		if deviates {
			used[rc.route] = true
			if !designSection.MatchString(why) {
				t.Errorf("%s: an intended deviation must cite a design section: %q", rc.route, why)
			}
		}
		switch {
		case !slices.Contains(allowed[old], base):
			if !deviates {
				t.Errorf("%s: old gate %q, new permission %q: not a translation of today's gate", rc.route, old, base)
			}
		case old == "none" && (base == authz.ChatUse || base == authz.OpsRun):
			if !deviates {
				t.Errorf("%s: %q tightens an ungated route: list it in intendedDeviations with its design section", rc.route, base)
			}
		case deviates:
			t.Errorf("%s: listed in intendedDeviations but it translates today's gate %q faithfully: drop the entry", rc.route, old)
		}
	}
	for k := range intendedDeviations {
		if !used[k] {
			t.Errorf("intendedDeviations has %q, which is no route", k)
		}
	}
}
