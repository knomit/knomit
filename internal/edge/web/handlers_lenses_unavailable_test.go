package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
)

// closeMounted tears name's store down through its lifecycle (Acquire:
// ErrRepoClosed) and puts the closed instance back where the lens resolver
// finds it by uid: a request that resolved the lens before teardown.
func closeMounted(t *testing.T, m *repos.Manager, name string) {
	t.Helper()
	ri := m.Get(name)
	require.NotNil(t, ri)
	m.Remove(name)
	_, _, aerr := ri.Acquire()
	require.ErrorIs(t, aerr, repos.ErrRepoClosed, "the precondition: %s is closed", name)
	m.Set(name, ri)
}

// A lens single-fact read that resolves to the lens's WRITE mount answers the
// store-unavailable 503 every other route answers, while that store is down
// (user ruling 2026-10-24): every lens has a write mount, so saying it is down
// reveals no topology. The read, the sub-resource gate and the .knomit/ read
// all do; each answered 404 for the same path with every store open, so the
// 503 is the store and not the path.
func TestLensFact_WriteMountStoreUnavailableAnswers503(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	paths := []string{
		"/lenses/eng/facts/kb/y/2.md",
		"/lenses/eng/facts/kb/y/2.md/incoming",
		"/lenses/eng/facts/.knomit/missing.yaml",
	}
	for _, p := range paths {
		require.Equal(t, http.StatusNotFound, getLensFacts(t, r, p).Code, "%s with every store open", p)
	}
	const present = "/lenses/eng/facts/.knomit/ontology.yaml"
	require.Equal(t, http.StatusOK, getLensFacts(t, r, present).Code)

	closeMounted(t, m, "alpha")

	for _, p := range append(paths, present) {
		t.Run(p, func(t *testing.T) {
			requireUnavailableProblem(t, getLensFacts(t, r, p), p)
		})
	}
}

// A lens single-fact read addressed to a READ mount whose store is down
// answers the lens's missing-fact 404, byte-identical to the 404 the same
// requested path got when the store was open and the fact simply was not
// there — never a 503, which would be a mount-topology oracle.
//
// REACHABILITY: the store-down branch for a read mount (lensFactMount's
// readMount) is not reachable over HTTP without a fault hook. A read mount's
// id comes from its store and Identify's exit clears it BEFORE Open detaches
// the store, so once the store is gone the kb://<id12>/ path no longer
// resolves and answers through the unmounted-id exit. That reachable
// equivalent is what is pinned here (requested != rel throughout); the
// branch itself is pinned by TestLensFactMount_RoleDecidesTheUnavailableAnswer.
func TestLensFact_ReadMountStoreUnavailableIsTheMissingFact404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	id := federate.ID12(m.Get("beta").ID())

	missing := []struct{ name, wire string }{
		{"fact", "kb://" + id + "/kb/y/2.md"},
		{"sub-resource", "kb://" + id + "/kb/y/2.md/incoming"},
		{"system file", "kb://" + id + "/.knomit/missing.yaml"},
	}
	before := make([]*httptest.ResponseRecorder, len(missing))
	for i, tc := range missing {
		before[i] = getLensFacts(t, r, "/lenses/eng/facts/"+url.PathEscape(tc.wire))
		requireLensProblem(t, before[i], http.StatusNotFound, "Fact not found", `no fact at path "`+tc.wire+`"`)
	}

	closeMounted(t, m, "beta")

	for i, tc := range missing {
		t.Run(tc.name, func(t *testing.T) {
			after := getLensFacts(t, r, "/lenses/eng/facts/"+url.PathEscape(tc.wire))
			require.Equal(t, before[i].Code, after.Code, after.Body.String())
			require.Equal(t, before[i].Header(), after.Header())
			require.Equal(t, before[i].Body.String(), after.Body.String(), "byte-identical to the missing-fact 404")
		})
	}
}

// The lens union reads fan out over every mount and fail the whole request on
// any mount's error (RFC §9.1: a lens never silently shrinks its read set;
// user ruling 2026-10-24 keeps this). A mount whose store cannot be reached is
// now that error, answered with the one store-unavailable 503 — before, the
// mount's helpers returned zero values and the union quietly dropped it. The
// 503 names no mount: its detail is fixed, and which mounts a lens has is
// already on GET /lenses/{lens}.
//
// stats has two legs: the aggregates for every mount, then — only when no axis
// was given and the mounts' default axes disagree, which a closed mount's
// empty default makes true — a highlights re-fetch. The explicit-axis row
// skips the second leg, so its 503 can only come from the first.
func TestLensUnionReads_MountStoreUnavailableAnswer503(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	// open is each route's answer with every mount open: an unknown motif is
	// a 404 there, which the teardown must not turn into anything but the 503.
	routes := []struct {
		name, path, query string
		open              int
	}{
		{"facts (recentFacts)", "/lenses/eng/facts", "", http.StatusOK},
		{"search (searchRepo)", "/lenses/eng/search", "?domain=ai", http.StatusOK},
		{"completions (readCompletions)", "/lenses/eng/completions", "?category=domain", http.StatusOK},
		{"motifs (motifClusters)", "/lenses/eng/motifs", "", http.StatusOK},
		{"motif cluster (motifClusters)", "/lenses/eng/motifs/some-motif", "", http.StatusNotFound},
		{"stats (loadStats, both legs)", "/lenses/eng/stats", "", http.StatusOK},
		{"stats, explicit axis (aggregates leg only)", "/lenses/eng/stats", "?axis=confidence", http.StatusOK},
		{"topics (listTopicDir)", "/lenses/eng/topics", "", http.StatusOK},
	}
	for _, tc := range routes {
		rec := getLensFacts(t, r, tc.path+tc.query)
		require.Equal(t, tc.open, rec.Code, "%s with every mount open: %s", tc.name, rec.Body.String())
	}

	closeMounted(t, m, "beta")

	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			requireUnavailableProblem(t, getLensFacts(t, r, tc.path+tc.query), tc.path)
		})
	}
}
