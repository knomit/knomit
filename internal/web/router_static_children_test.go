package web

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// walkRoutes returns every registered route pattern, from the router the server
// actually serves. NewAPIRouter is the production constructor — a hand-built
// chi router here would assert about a route table nobody is served.
func walkRoutes(t *testing.T, r chi.Router) []string {
	t.Helper()
	var out []string
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	sort.Strings(out)
	return out
}

// NOTHING STATIC MAY LIVE UNDER /repos/.
//
// Repo names are [a-z0-9_-] (repos.IsValidName), so any static segment
// registered under /repos/ is ALSO a legal repo name, and chi prefers the
// static route — the repo of that name becomes unreachable there, with no
// diagnosis available to its owner. GET /repos/events shipped that way at
// 594b551a and is why this test exists; the route now lives at /index-events.
//
// The rule is deliberately blunt rather than a judgement per route. The cost of
// a collision is NOT uniform — a static LEAF costs one method+path because chi
// backtracks, while a static segment with a PARAM CHILD costs the whole subtree
// below it — and that difference is invisible from "static beats param" alone.
// Rather than ask each new route's author to work out which case they are in,
// nothing static goes under /repos/ at all. Top-level collections
// (/repo-creates, /index-events) are the established way to add one.
//
// This is an EXCLUSION test, not a coverage test: it does not need a list of
// known-bad names and cannot go stale as routes are added. A new static child
// fails it by name, whatever it is called.
func TestRouter_NoStaticChildrenUnderRepos(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	routes := walkRoutes(t, s.NewAPIRouter())

	if len(routes) == 0 {
		t.Fatal("walked the router and found NO routes — the walk is broken, and a " +
			"broken walk makes this test vacuously green")
	}

	var offenders []string
	for _, r := range routes {
		_, pattern, _ := strings.Cut(r, " ")
		rest, under := strings.CutPrefix(pattern, "/repos/")
		if !under {
			continue
		}
		seg := strings.SplitN(rest, "/", 2)[0]
		if seg == "" || strings.HasPrefix(seg, "{") || strings.HasPrefix(seg, "*") {
			continue
		}
		offenders = append(offenders, r)
	}
	if len(offenders) > 0 {
		t.Errorf("static route(s) registered under /repos/, which shadow a repo of the "+
			"same name:\n  %s\n\nMove it to a TOP-LEVEL collection (see /repo-creates and "+
			"/index-events in router.go) rather than adding an exception here.",
			strings.Join(offenders, "\n  "))
	}
}

// THE TOP LEVEL IS SAFE TO PUT A STATIC COLLECTION IN, which is what makes the
// move a fix and not a relocation of the same hazard.
//
// The shadowing problem needs a {param} SIBLING to shadow into: /repos/events
// was a hazard because /repos/{repo} exists beside it. At the API root there is
// no such sibling — every child is static — so /index-events cannot shadow
// anything and nothing can shadow it. If a top-level {param} route is ever
// added, this fails, and every top-level static collection needs re-examining.
func TestRouter_APIRootHasNoParamChild(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}

	var paramChildren []string
	for _, r := range walkRoutes(t, s.NewAPIRouter()) {
		_, pattern, _ := strings.Cut(r, " ")
		seg := strings.SplitN(strings.TrimPrefix(pattern, "/"), "/", 2)[0]
		if strings.HasPrefix(seg, "{") || strings.HasPrefix(seg, "*") {
			paramChildren = append(paramChildren, r)
		}
	}
	if len(paramChildren) > 0 {
		t.Errorf("the API root has a {param} child:\n  %s\n\nEvery top-level static "+
			"collection (/index-events, /repo-creates, /sessions, /logs, /archived) can "+
			"now be shadowed by it. Re-examine them.", strings.Join(paramChildren, "\n  "))
	}
}
