package web

import (
	"flag"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var updateRouteTable = flag.Bool("update-route-table", false, "rewrite testdata/route_table.golden")

var (
	mwFuncSuffix = regexp.MustCompile(`(\.func\d+|\.\d+)+$`)
	mwPkgPrefix  = regexp.MustCompile(`^.*/`)
)

// mwFingerprint reduces a middleware's runtime name to the function's own
// name: no import path, no package qualifier, no closure suffix and no
// leading-letter case. The step-12e move of the fleet gate from
// web.fleetRepoRefused to httpx.FleetRepoRefused must not change a route's
// fingerprint, so the package and the export case are exactly what it
// forgets.
func mwFingerprint(name string) string {
	name = mwPkgPrefix.ReplaceAllString(name, "")
	name = mwFuncSuffix.ReplaceAllString(name, "")
	if i := strings.Index(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if name != "" {
		name = strings.ToLower(name[:1]) + name[1:]
	}
	return name
}

// The API router's route table - every method+pattern, with the middleware
// chain that runs before its handler, outermost first - is byte-identical to
// the golden file generated from the router before the route registrations
// moved out of router.go into api/manage (origin/dev 97ae7c19). Chains come
// from walkFull, not chi.Walk, which drops the middleware of a group a
// subrouter is mounted in (kb gotcha chi-walk-group-middleware).
//
// SABOTAGE: register one route outside its group, drop a Use, or reorder
// two Uses -> the diff names the route -> red.
func TestRouter_RouteTableMatchesGolden(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	var all []routeChain
	walkFull(s.NewAPIRouter(), "", nil, &all)
	lines := make([]string, 0, len(all))
	for _, rc := range all {
		mws := make([]string, len(rc.mws))
		for i, m := range rc.mws {
			mws[i] = mwFingerprint(m)
		}
		lines = append(lines, rc.route+" | "+strings.Join(mws, " > "))
	}
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"
	const path = "testdata/route_table.golden"
	if *updateRouteTable {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) == got {
		return
	}
	wl, gl := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(string(want), "\n") {
		wl[l] = true
	}
	for _, l := range lines {
		gl[l] = true
		if !wl[l] {
			t.Errorf("route not in golden: %s", l)
		}
	}
	for l := range wl {
		if l != "" && !gl[l] {
			t.Errorf("golden route missing: %s", l)
		}
	}
}
