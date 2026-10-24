package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

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

// The lens single-fact route answers a mount whose store cannot be reached
// with the SAME 404 it answers a missing fact (user ruling 2026-10-24): same
// status, same headers, byte-identical body echoing the REQUESTED path. The
// repo's own route answers that state 503 (TestReads_*); a 503 here would let
// a caller read "mounted, store down" off the status, which
// kb/gotchas/web/lens-fact-404 forbids.
//
// The mount is the lens's write member, addressed by bare path: a closed
// read mount loses its id with its identity (Identify's exit clears it), so a
// kb://<id12>/ path to it already resolves as unmounted before any read. The
// sub-resource and system-file paths are the ones where requested differs
// from the mount-relative path the read uses, so echoing the wrong one fails.
func TestLensFact_MountStoreUnavailableIsTheMissingFact404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	missing := []struct{ name, url, detail string }{
		{"fact", "/lenses/eng/facts/kb/y/2.md", `no fact at path "kb/y/2.md"`},
		{"sub-resource (requested != rel)", "/lenses/eng/facts/kb/y/2.md/incoming", `no fact at path "kb/y/2.md/incoming"`},
		{"system file", "/lenses/eng/facts/.knomit/missing.yaml", `no fact at path ".knomit/missing.yaml"`},
	}
	before := make([]*httptest.ResponseRecorder, len(missing))
	for i, tc := range missing {
		before[i] = getLensFacts(t, r, tc.url)
		requireLensProblem(t, before[i], http.StatusNotFound, "Fact not found", tc.detail)
	}
	// A system file that IS there, so the 404 after the teardown is the store,
	// not the file.
	const present = "/lenses/eng/facts/.knomit/ontology.yaml"
	require.Equal(t, http.StatusOK, getLensFacts(t, r, present).Code)

	closeMounted(t, m, "alpha")

	for i, tc := range missing {
		t.Run(tc.name, func(t *testing.T) {
			after := getLensFacts(t, r, tc.url)
			require.Equal(t, before[i].Code, after.Code, after.Body.String())
			require.Equal(t, before[i].Header(), after.Header())
			require.Equal(t, before[i].Body.String(), after.Body.String(), "byte-identical to the missing-fact 404")
		})
	}
	t.Run("present system file", func(t *testing.T) {
		requireLensProblem(t, getLensFacts(t, r, present), http.StatusNotFound,
			"Fact not found", `no fact at path ".knomit/ontology.yaml"`)
	})
}

// The lens union reads fan out over every mount and fail the whole request on
// any mount's error (RFC §9.1: a lens never silently shrinks its read set).
// A mount whose store cannot be reached is now that error, answered with the
// one store-unavailable 503 — before, the mount's helpers returned zero values
// and the union quietly dropped it. The 503 names no mount: its detail is
// fixed, and which mounts a lens has is already on GET /lenses/{lens}.
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
		{"stats (loadStats)", "/lenses/eng/stats", "", http.StatusOK},
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
