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

// paramParents returns, for the router the server actually serves, every path
// that has a `{param}` child — the places where a static sibling could shadow
// an entity the param names — mapped to that param's spelling.
func paramParents(t *testing.T, r chi.Router) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := chi.Walk(r, func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		segs := strings.Split(strings.Trim(route, "/"), "/")
		for i, seg := range segs {
			if strings.HasPrefix(seg, "{") {
				out["/"+strings.Join(segs[:i], "/")] = seg
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	return out
}

// paramClass records, for every path in the router that has a `{param}` child,
// whether that param's value is a name A USER CAN CAUSE TO EXIST. Only those
// need guarding: a static sibling can only shadow something if a user could
// have brought a thing of that name into being.
//
// THE MEMBERSHIP TEST IS "can a user cause a value with that name to exist",
// NOT "does a user own it". Ownership tracks how BAD a collision would be; only
// the first question decides whether one can happen at all, and that is what
// guarding turns on. A domain tag is not owned by anyone and is still authored
// by whoever writes `domain: [stats]` in a fact's frontmatter, so
// `/domains/stats` would shadow something real.
//
// KEYED BY PARENT PREFIX, NOT BY PARAM SPELLING, because one spelling can hold
// two truths: `{name}` is a product-shipped preset at /ontologies/presets/{name}
// and an author-written tag at /domains/{name}. A table keyed by spelling
// cannot state both, and an earlier version of this test hedged in prose
// instead of deciding. If you find yourself writing a caveat into one of these
// reasons, check first whether the key is at the wrong granularity.
//
// Each entry carries its reason. A bare list ages exactly the way an
// enumeration does; one with reasons lets the next author judge whether their
// case belongs.
//
// AN UNCLASSIFIED PARENT FAILS THIS TEST, deliberately. A new param subtree must
// force a decision rather than defaulting either way — defaulting to guarded
// would quietly forbid legitimate routes, defaulting to unguarded would quietly
// reopen the hazard, and neither silence is acceptable.
var paramClass = map[string]struct {
	userNamed bool
	reason    string
}{
	"/repos":                 {true, "{repo}: repo names are chosen by the user, [a-z0-9_-] via repos.IsValidName"},
	"/lenses":                {true, "{lens}: lens names go through the SAME validator as repo names (validateLensLocked, CreateLens and UpdateLens all pass l.Name to isValidRepoName)"},
	"/repos/{repo}/branches": {true, "{branch}: branch names are chosen by whoever creates the branch"},
	"/repos/{repo}/branches/{branch}/domains": {true, "{name}: domain tags are authored by fact writers — someone writing `domain: [stats]` in frontmatter creates one"},
	"/repos/{repo}/branches/{branch}/motifs":  {true, "{key}: motif keys are authored by fact writers, the same way domain tags are"},
	"/lenses/{lens}/motifs":                   {true, "{key}: the lens-scoped view of the same author-written motif keys"},
	"/lenses/{lens}/experiments":              {true, "{name}: experiment names are chosen by whoever calls knomit_experiment open — kebab-case, validated but not drawn from any set the product defines"},

	"/ontologies/presets": {false, "{name}: OUR shipped preset names — the product defines this set, no user adds to it"},
	"/archived":           {false, "{id}: a server-minted identifier"},
	"/repo-creates":       {false, "{id}: a server-minted job id"},
	"/repos/{repo}/branches/{branch}/commits":        {false, "{sha}: a commit hash"},
	"/repos/{repo}/branches/{branch}/index-rebuilds": {false, "{id}: a server-minted job id"},
	"/repos/{repo}/branches/{branch}/synthesis-runs": {false, "{id}: a server-minted job id"},
	"/repos/{repo}/origin-sessions":                  {false, "{sessionID}: comes from the origin, not from a name chosen here"},
}

// NO STATIC ROUTE MAY SIT BESIDE A {param} THAT HOLDS A USER-CHOSEN NAME.
//
// Such a segment is also a legal value for that param; chi prefers the static
// route, and the entity called that becomes unreachable there, with nothing
// diagnosable from the outside by whoever owns it. GET /repos/events shipped
// exactly that way at 594b551a — a repo named `events` lost
// GET /api/v1/repos/events — and the route now lives at /repo-events.
//
// THE PREFIXES ARE DERIVED FROM THE ROUTER, not listed here. An earlier version
// of this test cut on the "/repos/" prefix alone, which left /lenses/{lens} —
// the same hazard, through the very same name validator — completely
// unguarded, and /repos/{repo}/branches/{branch} with it. A rule enforced
// against one instance of a general hazard is how the general hazard recurs one
// subtree over. The walk yields CANDIDATES; paramClass decides which are real.
//
// The rule is deliberately blunt rather than a judgement per route. The cost of
// a collision is NOT uniform — a static LEAF costs one method+path because chi
// backtracks, while a static segment with a PARAM CHILD costs the whole subtree
// below it — and that difference is invisible from "static beats param" alone.
// Rather than ask each new route's author to work out which case they are in,
// nothing static goes beside a user-named param at all. Top-level collections
// (/repo-creates, /repo-events) are the established way to add one.
//
// This is an EXCLUSION test: it needs no list of known-bad names and cannot go
// stale as routes are added. A new static sibling fails it by name.
func TestRouter_NoStaticSiblingsOfUserNamedParams(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	router := s.NewAPIRouter()
	routes := walkRoutes(t, router)
	parents := paramParents(t, router)

	if len(routes) == 0 || len(parents) == 0 {
		t.Fatal("walked the router and found no routes or no {param} parents — the " +
			"derivation is broken, and a broken derivation makes this test vacuously green")
	}

	guarded := map[string]string{}
	for parent, param := range parents {
		class, known := paramClass[parent]
		if !known {
			t.Errorf("unclassified {param} subtree %q (param %s): add it to paramClass "+
				"and say whether a user can cause a value with that name to exist, WITH "+
				"the reason. An unclassified parent fails on purpose — it must not "+
				"default to guarded or to unguarded.", parent, param)
			continue
		}
		if class.userNamed {
			guarded[parent] = param
		}
	}

	// The derivation must actually FIND the three subtrees this rule exists for.
	// If a refactor renames or restructures them, the loop below would pass by
	// guarding nothing, so these are a tripwire ON THE DERIVATION — not the
	// definition of what is guarded, which paramClass decides.
	// SIX, not five: `motifs` hangs under BOTH /repos/{repo}/branches/{branch}
	// and /lenses/{lens}, so the lens-scoped view is its own subtree and its own
	// entry. A tripwire that listed five would have been satisfied while one
	// real subtree went unguarded, which is the failure this whole rule is about.
	for _, must := range []string{
		"/repos",
		"/lenses",
		"/repos/{repo}/branches",
		"/repos/{repo}/branches/{branch}/domains",
		"/repos/{repo}/branches/{branch}/motifs",
		"/lenses/{lens}/motifs",
	} {
		if _, ok := guarded[must]; !ok {
			t.Errorf("the derivation did not find %q as a guarded {param} parent; it "+
				"found %v. Either the route moved or the derivation is broken — do not "+
				"silence this by deleting the entry.", must, keysOf(guarded))
		}
	}

	var offenders []string
	for _, r := range routes {
		_, pattern, _ := strings.Cut(r, " ")
		segs := strings.Split(strings.Trim(pattern, "/"), "/")
		for i, seg := range segs {
			if strings.HasPrefix(seg, "{") || strings.HasPrefix(seg, "*") || seg == "" {
				continue
			}
			parent := "/" + strings.Join(segs[:i], "/")
			if param, ok := guarded[parent]; ok {
				offenders = append(offenders, r+"  (static sibling of "+param+" at "+parent+")")
			}
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("static route(s) sitting beside a {param} that holds a user-chosen "+
			"name, which shadow an entity of the same name:\n  %s\n\nMove it to a "+
			"TOP-LEVEL collection (see /repo-creates and /repo-events in router.go) "+
			"rather than reclassifying the param — paramClass is for params that are "+
			"NOT user-chosen names, not an escape hatch for a route you want to keep.",
			strings.Join(offenders, "\n  "))
	}
	t.Logf("guarded {param} parents: %v", keysOf(guarded))
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// THE TOP LEVEL IS SAFE TO PUT A STATIC COLLECTION IN, which is what makes the
// move a fix and not a relocation of the same hazard.
//
// The shadowing problem needs a {param} SIBLING to shadow into: /repos/events
// was a hazard because /repos/{repo} exists beside it. At the API root there is
// no such sibling — every child is static — so /repo-events cannot shadow
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
			"collection (/repo-events, /repo-creates, /sessions, /logs, /archived) can "+
			"now be shadowed by it. Re-examine them.", strings.Join(paramChildren, "\n  "))
	}
}
